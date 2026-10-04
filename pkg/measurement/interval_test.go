package measurement

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
)

func TestParseInterval(t *testing.T) {
	for _, c := range []struct {
		in   string
		set  bool
		want time.Duration
		bad  bool
	}{
		{set: false, want: 10 * time.Second},
		{in: "", set: true, want: 10 * time.Second},
		{in: "5", set: true, want: 5 * time.Second},
		{in: " 2 ", set: true, want: 2 * time.Second},
		{in: "1s", set: true, want: time.Second},
		{in: "500ms", set: true, want: 500 * time.Millisecond},
		{in: "100ms", set: true, want: 100 * time.Millisecond},
		{in: "99ms", set: true, bad: true},
		{in: "0", set: true, bad: true},
		{in: "-1", set: true, bad: true},
		{in: "abc", set: true, bad: true},
		// integer seconds that overflow a Duration are an error, not a wrapped value
		{in: "9223372036", set: true, want: 9223372036 * time.Second},
		{in: "9223372037", set: true, bad: true},
		{in: "18446744074", set: true, bad: true},
	} {
		p := properties.NewProperties()
		if c.set {
			p.Set(prop.LogInterval, c.in)
		}
		got, err := ParseInterval(p)
		if c.bad {
			if err == nil {
				t.Errorf("%q: got %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

// Concurrent recorders against a reporter cutting intervals: every sample lands
// in exactly one interval, and cum_count is the running total.
func TestIntervalsLoseNothing(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows = true // as with an output file
	h.IntervalStart(time.Now())

	const workers, perWorker = 8, 20000
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			op := []string{"READ", "UPDATE"}[w%2]
			for i := 0; i < perWorker; i++ {
				h.Measure(op, time.Now(), time.Duration(i%5000)*time.Microsecond)
			}
		}(w)
	}

	sum := map[string]int64{}
	lastCum := map[string]int64{}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	cut := func() {
		for _, r := range h.cutInterval(time.Now()) {
			sum[r.Op] += r.Count
			if r.CumCount < lastCum[r.Op] || r.CumCount != sum[r.Op] {
				t.Fatalf("%s: cum_count %d, running sum %d, previous %d", r.Op, r.CumCount, sum[r.Op], lastCum[r.Op])
			}
			lastCum[r.Op] = r.CumCount
		}
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-time.After(time.Millisecond):
		}
		cut()
	}
	cut() // nothing left: an interval of zero counts

	for _, op := range []string{"READ", "UPDATE"} {
		want := int64(workers / 2 * perWorker)
		if sum[op] != want || h.histograms[op].hist.TotalCount() != want {
			t.Errorf("%s: intervals sum to %d, cumulative %d, want %d", op, sum[op], h.histograms[op].hist.TotalCount(), want)
		}
	}
}

func TestIntervalOutputFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intervals.jsonl")
	h := InitHistograms(properties.NewProperties())
	if err := h.openIntervals(path); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	h.IntervalStart(t0)
	for i := 1; i <= 100; i++ {
		h.Measure("READ", t0, time.Duration(i)*time.Microsecond)
	}
	h.Measure("READ_ERROR", t0, 7*time.Microsecond)
	h.IntervalTick(t0.Add(time.Second))
	h.Measure("READ", t0, 50*time.Microsecond)
	if err := h.IntervalClose(t0.Add(1500 * time.Millisecond)); err != nil { // the last, partial interval
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 4 {
		t.Fatalf("%d lines, want 4 (2 ops x 2 intervals): %v", len(lines), lines)
	}
	keys := func(m map[string]any) []string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		sort.Strings(k)
		return k
	}
	full := []string{"avg_us", "count", "cum_count", "max_us", "min_us", "op", "ops", "p50_us", "p90_us", "p95_us", "p999_us", "p9999_us", "p99_us", "t", "ts", "window_s"}
	empty := []string{"count", "cum_count", "op", "ops", "t", "ts", "window_s"}
	sort.Strings(full)

	r := lines[0]
	if r["op"] != "READ" || r["count"] != 100.0 || r["cum_count"] != 100.0 || r["window_s"] != 1.0 || r["t"] != 1.0 || r["ops"] != 100.0 {
		t.Errorf("first READ interval = %v", r)
	}
	if r["ts"] != "2026-10-03T12:00:01Z" || r["min_us"] != 1.0 || r["max_us"] != 100.0 || r["p50_us"] != 50.0 || r["avg_us"] != 50.5 {
		t.Errorf("first READ interval = %v", r)
	}
	if !reflect.DeepEqual(keys(r), full) {
		t.Errorf("keys %v, want %v", keys(r), full)
	}
	if e := lines[1]; e["op"] != "READ_ERROR" || e["count"] != 1.0 {
		t.Errorf("error op interval = %v", e)
	}
	if r := lines[2]; r["op"] != "READ" || r["count"] != 1.0 || r["cum_count"] != 101.0 || r["window_s"] != 0.5 || r["t"] != 1.5 {
		t.Errorf("last (partial) READ interval = %v", r)
	}
	if e := lines[3]; e["count"] != 0.0 || e["ops"] != 0.0 || e["cum_count"] != 1.0 || !reflect.DeepEqual(keys(e), empty) {
		t.Errorf("an interval with no samples carries no latencies: %v (keys %v)", e, keys(e))
	}
}

// With no warm-up, workers record before the reporter starts the intervals:
// those samples go to the first interval, so the intervals add up to the total.
func TestSamplesBeforeStartCount(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows = true
	h.Measure("READ", time.Now(), time.Microsecond)
	t0 := time.Now()
	h.IntervalStart(t0)
	h.Measure("READ", time.Now(), time.Microsecond)
	recs := h.cutInterval(t0.Add(time.Second))
	if len(recs) != 1 || recs[0].Count != 2 || recs[0].CumCount != 2 {
		t.Errorf("first interval = %+v, want count 2 = cum_count", recs)
	}
}

func TestNoIntervalBeforeStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intervals.jsonl")
	h := InitHistograms(properties.NewProperties())
	if err := h.openIntervals(path); err != nil {
		t.Fatal(err)
	}
	h.Measure("READ", time.Now(), time.Microsecond)
	if err := h.IntervalClose(time.Now()); err != nil { // the run ended in warm-up
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); len(b) != 0 {
		t.Errorf("wrote %q before the intervals started", b)
	}
}

// A cut before the intervals start (still in warm-up) has no interval to end:
// it returns nothing rather than records timed from the zero time.
func TestCutBeforeStart(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows = true
	h.Measure("READ", time.Now(), time.Microsecond)
	if recs := h.cutInterval(time.Now()); recs != nil {
		t.Errorf("cut %+v before the intervals started", recs)
	}
}

// An interval file that can't be written fails the run: IntervalClose returns
// the error (go-ycsb exits non-zero after the summary) instead of leaving a
// truncated file that looks complete.
func TestIntervalWriteErrorIsReturned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intervals.jsonl")
	h := InitHistograms(properties.NewProperties())
	if err := h.openIntervals(path); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	h.IntervalStart(t0)
	h.Measure("READ", time.Now(), time.Microsecond)
	h.iv.file.Close() // every later write and flush fails
	h.IntervalTick(t0.Add(time.Second))
	if err := h.IntervalClose(t0.Add(1500 * time.Millisecond)); err == nil || !strings.Contains(err.Error(), "interval output file") {
		t.Errorf("IntervalClose = %v, want the write error", err)
	}

	// and a clean run returns nil
	h = InitHistograms(properties.NewProperties())
	if err := h.openIntervals(filepath.Join(t.TempDir(), "ok.jsonl")); err != nil {
		t.Fatal(err)
	}
	h.IntervalStart(t0)
	h.Measure("READ", time.Now(), time.Microsecond)
	if err := h.IntervalClose(t0.Add(time.Second)); err != nil {
		t.Errorf("IntervalClose = %v on a writable file", err)
	}
}

// The client's DB wrapper measures each successful operation and then TOTAL
// (a failed one only as <op>_ERROR, with no TOTAL). A cut takes every
// operation's window before summarising any, so in every interval TOTAL equals
// the sum of the successful operations up to the operation/TOTAL pairs the cut
// falls between: such a pair counts +1 in one interval and -1 in the next, and
// the sums match exactly over the whole run. Here each goroutine records
// directly, so at most one pair per goroutine is split; in the real pipeline
// (TestIntervalTotalMatchesOpsPipeline) the bound is one per client thread.
func TestIntervalTotalMatchesOps(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows = true
	h.IntervalStart(time.Now())

	const workers, perWorker = 4, 50000
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			op := []string{"READ", "UPDATE", "INSERT", "READ"}[w]
			for i := 0; i < perWorker; i++ {
				lat := time.Duration(i%5000) * time.Microsecond
				if w == 3 && i%10 == 0 { // a failed operation: no TOTAL
					h.Measure("READ_ERROR", time.Now(), lat)
					continue
				}
				h.Measure(op, time.Now(), lat)
				h.Measure("TOTAL", time.Now(), lat)
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	checkTotals(t, h, done, workers, workers*perWorker-perWorker/10)
}

// Like the client: many threads send operation/TOTAL pairs into one channel,
// and a single goroutine records them (InitMeasure's measureChan reader) while
// the reporter cuts intervals. A pair is split only while both halves are in
// flight, so per interval TOTAL and the operations differ by at most one per
// client thread.
func TestIntervalTotalMatchesOpsPipeline(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows = true
	h.IntervalStart(time.Now())

	const threads, perThread = 64, 5000
	ch := make(chan measureEvent, 1000)
	recorded := make(chan struct{})
	go func() {
		for ev := range ch {
			h.Measure(ev.op, ev.start, ev.lan)
		}
		close(recorded)
	}()
	var wg sync.WaitGroup
	wg.Add(threads)
	for th := 0; th < threads; th++ {
		go func(th int) {
			defer wg.Done()
			op := []string{"READ", "UPDATE"}[th%2]
			for i := 0; i < perThread; i++ {
				lat := time.Duration(i%3000) * time.Microsecond
				ch <- measureEvent{op, time.Now(), lat}
				ch <- measureEvent{"TOTAL", time.Now(), lat}
			}
		}(th)
	}
	go func() { wg.Wait(); close(ch) }()
	checkTotals(t, h, recorded, threads, threads*perThread)
}

// checkTotals cuts intervals until done, checking that TOTAL matches the sum of
// the successful operations within bound per interval and exactly overall.
func checkTotals(t *testing.T, h *histograms, done <-chan struct{}, bound int, want int) {
	t.Helper()
	var okAll, totalAll int64
	check := func() {
		var ok, total int64
		for _, r := range h.cutInterval(time.Now()) {
			switch {
			case r.Op == "TOTAL":
				total += r.Count
			case !strings.HasSuffix(r.Op, "_ERROR"):
				ok += r.Count
			}
		}
		okAll, totalAll = okAll+ok, totalAll+total
		if d := ok - total; d < -int64(bound) || d > int64(bound) {
			t.Fatalf("interval: TOTAL %d, successful operations %d: off by %d (at most %d in flight)", total, ok, d, bound)
		}
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-time.After(200 * time.Microsecond):
		}
		check()
	}
	check()
	if okAll != int64(want) || totalAll != int64(want) {
		t.Errorf("over the run: successful operations %d, TOTAL %d, want %d each", okAll, totalAll, want)
	}
}

// Without an interval output file no windows are kept and the reporter cuts
// nothing: the run costs what it did before intervals existed.
func TestNoWindowsWithoutOutputFile(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.IntervalStart(time.Now())
	h.Measure("READ", time.Now(), time.Microsecond)
	if h.histograms["READ"].win != nil {
		t.Error("a window histogram was kept without an interval output file")
	}
	if recs := h.cutInterval(time.Now()); recs != nil {
		t.Errorf("cut %v without windows", recs)
	}
	h.IntervalTick(time.Now()) // no file: nothing to do, no panic
	if c := h.histograms["READ"].hist.TotalCount(); c != 1 {
		t.Errorf("cumulative count %d, want 1", c)
	}
}

// failWriter fails every write, like a full disk.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

var errWriteFailed = &os.PathError{Op: "write", Path: "intervals.jsonl", Err: os.ErrInvalid}

// captureStderr returns what f writes to os.Stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()
	f()
	w.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

func TestIntervalWriteErrorReportedOnce(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	if err := h.openIntervals(filepath.Join(t.TempDir(), "intervals.jsonl")); err != nil {
		t.Fatal(err)
	}
	h.iv.out = bufio.NewWriterSize(failWriter{}, 16) // every flush fails
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	h.IntervalStart(t0)
	h.Measure("READ", t0, time.Microsecond)
	var closeErr error
	out := captureStderr(t, func() {
		h.IntervalTick(t0.Add(time.Second))
		h.Measure("READ", t0, time.Microsecond)
		h.IntervalTick(t0.Add(2 * time.Second))
		closeErr = h.IntervalClose(t0.Add(2500 * time.Millisecond))
	})
	if n := strings.Count(out, errWriteFailed.Error()); n != 1 {
		t.Errorf("write error printed %d times, want once:\n%s", n, out)
	}
	if !errors.Is(closeErr, errWriteFailed) {
		t.Errorf("IntervalClose = %v, want the write error, so the run fails", closeErr)
	}
}

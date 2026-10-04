package measurement

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
)

// MinInterval is the shortest reporting interval accepted.
const MinInterval = 100 * time.Millisecond

// ParseInterval reads prop.LogInterval: a Go duration ("1s", "500ms") or, as
// before, an integer number of seconds. Unset means the default, 10s.
func ParseInterval(p *properties.Properties) (time.Duration, error) {
	v, ok := p.Get(prop.LogInterval)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return prop.LogIntervalDefault, nil
	}
	var d time.Duration
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n > int64(math.MaxInt64/time.Second) {
			return 0, fmt.Errorf("%s=%q: too large", prop.LogInterval, v)
		}
		d = time.Duration(n) * time.Second
	} else if d, err = time.ParseDuration(v); err != nil {
		return 0, fmt.Errorf("%s=%q: want a duration like 1s or 500ms, or a number of seconds", prop.LogInterval, v)
	}
	if d < MinInterval {
		return 0, fmt.Errorf("%s=%q: the minimum is %s", prop.LogInterval, v, MinInterval)
	}
	return d, nil
}

// IntervalRecord is one line of the interval output file: one operation over
// one reporting interval. Latencies are in microseconds and are absent when the
// operation had no samples in the interval (count 0).
type IntervalRecord struct {
	TS       string   `json:"ts"`       // end of the interval, RFC 3339 UTC
	T        float64  `json:"t"`        // seconds since measurement started
	WindowS  float64  `json:"window_s"` // interval length in seconds
	Op       string   `json:"op"`
	Count    int64    `json:"count"`
	Ops      float64  `json:"ops"` // count / window_s
	AvgUs    *float64 `json:"avg_us,omitempty"`
	MinUs    *int64   `json:"min_us,omitempty"`
	MaxUs    *int64   `json:"max_us,omitempty"`
	P50Us    *int64   `json:"p50_us,omitempty"`
	P90Us    *int64   `json:"p90_us,omitempty"`
	P95Us    *int64   `json:"p95_us,omitempty"`
	P99Us    *int64   `json:"p99_us,omitempty"`
	P999Us   *int64   `json:"p999_us,omitempty"`
	P9999Us  *int64   `json:"p9999_us,omitempty"`
	CumCount int64    `json:"cum_count"` // cumulative count at the end of the interval
}

// intervals cuts the histograms into reporting intervals. One reporter at a
// time: mu is held across a cut and the write of its records, so the ticker and
// the final interval at the end of the run can't interleave.
type intervals struct {
	mu    sync.Mutex
	start time.Time // measurement start: when warm-up ended
	last  time.Time // end of the previous interval
	out   *bufio.Writer
	file  *os.File
	err   error // the first write error; later writes are skipped
}

// startIntervals starts the intervals at now: the end of warm-up. Samples
// recorded before it (none during warm-up, which records nothing) belong to the
// first interval, so the intervals always add up to the cumulative counts.
func (h *histograms) startIntervals(now time.Time) {
	h.iv.mu.Lock()
	defer h.iv.mu.Unlock()
	h.iv.start, h.iv.last = now, now
}

// cutInterval ends the current interval at now and returns one record per
// operation seen so far, sorted by operation, then starts the next interval.
func (h *histograms) cutInterval(now time.Time) []IntervalRecord {
	h.iv.mu.Lock()
	defer h.iv.mu.Unlock()
	return h.cutIntervalLocked(now)
}

// cutIntervalLocked is cutInterval with iv.mu held. Every operation's window is
// taken first, with recording held off (histograms.cut), and only then
// summarised, so the windows end at the same instant: TOTAL matches the sum of
// the other operations up to an operation whose TOTAL falls in the next interval.
func (h *histograms) cutIntervalLocked(now time.Time) []IntervalRecord {
	if !h.windows {
		return nil
	}
	if h.iv.start.IsZero() {
		h.iv.start, h.iv.last = now, now
	}
	window := now.Sub(h.iv.last).Seconds()
	t := now.Sub(h.iv.start).Seconds()
	ts := now.UTC().Format(time.RFC3339Nano)
	h.iv.last = now

	h.mu.RLock()
	ops := make([]string, 0, len(h.histograms))
	for op := range h.histograms {
		ops = append(ops, op)
	}
	hs := make([]*histogram, len(ops))
	sort.Strings(ops)
	for i, op := range ops {
		hs[i] = h.histograms[op]
	}
	h.mu.RUnlock()

	ws := make([]*hdrhistogram.Histogram, len(ops))
	cums := make([]int64, len(ops))
	h.cut.Lock() // no recording while the windows are taken: they end together
	for i := range ops {
		ws[i], cums[i] = hs[i].takeWindow()
	}
	h.cut.Unlock()
	recs := make([]IntervalRecord, 0, len(ops))
	for i, op := range ops {
		w, cum := ws[i], cums[i]
		r := IntervalRecord{TS: ts, T: t, WindowS: window, Op: op, Count: w.TotalCount(), CumCount: cum}
		if window > 0 {
			r.Ops = float64(r.Count) / window
		}
		if r.Count > 0 {
			avg, min, max := w.Mean(), w.Min(), w.Max()
			p := func(q float64) *int64 { v := w.ValueAtPercentile(q); return &v }
			r.AvgUs, r.MinUs, r.MaxUs = &avg, &min, &max
			r.P50Us, r.P90Us, r.P95Us, r.P99Us, r.P999Us, r.P9999Us = p(50), p(90), p(95), p(99), p(99.9), p(99.99)
		}
		hs[i].returnWindow(w)
		recs = append(recs, r)
	}
	return recs
}

// writeInterval cuts an interval and appends its records to the output file
// and flushes them. Without an output file there are no windows to cut.
func (h *histograms) writeInterval(now time.Time) {
	h.iv.mu.Lock()
	defer h.iv.mu.Unlock()
	if h.iv.start.IsZero() { // the run ended in warm-up: no interval
		return
	}
	if h.iv.out == nil || h.iv.err != nil {
		return
	}
	recs := h.cutIntervalLocked(now)
	enc := json.NewEncoder(h.iv.out)
	for i := range recs {
		if err := enc.Encode(&recs[i]); err != nil {
			h.iv.err = err
			break
		}
	}
	if h.iv.err == nil {
		h.iv.err = h.iv.out.Flush()
	}
	if h.iv.err != nil {
		fmt.Fprintf(os.Stderr, "interval output: %v (no more interval records)\n", h.iv.err)
	}
}

// closeIntervals flushes and closes the interval output file.
func (h *histograms) closeIntervals() {
	h.iv.mu.Lock()
	defer h.iv.mu.Unlock()
	if h.iv.file == nil {
		return
	}
	reported := h.iv.err != nil // writeInterval has printed it already
	if !reported {
		h.iv.err = h.iv.out.Flush()
	}
	closeErr := h.iv.file.Close()
	switch {
	case !reported && h.iv.err == nil && closeErr != nil:
		h.iv.err = closeErr
		fallthrough
	case !reported && h.iv.err != nil:
		fmt.Fprintf(os.Stderr, "interval output: %v\n", h.iv.err)
	case reported && closeErr != nil:
		fmt.Fprintf(os.Stderr, "interval output: %v\n", closeErr)
	}
	h.iv.file, h.iv.out = nil, nil
}

func (h *histograms) openIntervals(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("interval output file: %w", err)
	}
	h.iv.file, h.iv.out = f, bufio.NewWriter(f)
	h.windows = true
	return nil
}

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/prop"
	_ "github.com/pingcap/go-ycsb/pkg/workload"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// warmUpAfterInit is whether InitMeasure, for a load with warmuptime set,
// left the measurement in warm-up.
var warmUpAfterInit bool

func TestMain(m *testing.M) {
	p := properties.NewProperties()
	p.Set(prop.DoTransactions, "false")
	p.Set(prop.WarmUpTime, "5")
	measurement.InitMeasure(p)
	warmUpAfterInit = !measurement.IsWarmUpFinished()
	measurement.EnableWarmUp(false)
	os.Exit(m.Run())
}

// A load starts no warm-up, even with warmuptime set: its first records are
// counted and measured like the rest, so it inserts insertcount records.
func TestInitMeasureLoadHasNoWarmUp(t *testing.T) {
	if warmUpAfterInit {
		t.Fatal("InitMeasure started a warm-up for a load")
	}
}

// memDB is a ycsb.DB that keeps the keys inserted into it.
type memDB struct {
	mu      sync.Mutex
	keys    map[string]int // key -> times inserted
	batches []int          // BatchInsert sizes
	fail    func(key string) error
	reads   int
	updates int
	onRead  func(reads int)
}

func newMemDB() *memDB { return &memDB{keys: map[string]int{}} }

func (d *memDB) insert(key string) error {
	if d.fail != nil {
		if err := d.fail(key); err != nil {
			return err
		}
	}
	d.keys[key]++
	return nil
}

func (d *memDB) Insert(_ context.Context, _ string, key string, _ map[string][]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.insert(key)
}

func (d *memDB) Close() error                                             { return nil }
func (d *memDB) InitThread(ctx context.Context, _, _ int) context.Context { return ctx }
func (d *memDB) CleanupThread(context.Context)                            {}
func (d *memDB) Read(_ context.Context, _ string, key string, _ []string) (map[string][]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	if d.onRead != nil {
		d.onRead(d.reads)
	}
	return map[string][]byte{"field0": []byte(key)}, nil
}
func (d *memDB) Scan(context.Context, string, string, int, []string) ([]map[string][]byte, error) {
	return nil, nil
}
func (d *memDB) Update(context.Context, string, string, map[string][]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updates++
	return nil
}
func (d *memDB) Delete(context.Context, string, string) error { return nil }

// batchMemDB adds BatchInsert to memDB.
type batchMemDB struct{ *memDB }

func (d batchMemDB) BatchInsert(_ context.Context, _ string, keys []string, _ []map[string][]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches = append(d.batches, len(keys))
	errs := make([]error, len(keys))
	for i, key := range keys {
		errs[i] = d.insert(key)
	}
	return ycsb.NewBatchError(errs)
}

func loadProps(insertStart, insertCount, threads, batchSize int) *properties.Properties {
	p := properties.NewProperties()
	p.Set(prop.DoTransactions, "false")
	p.Set(prop.RecordCount, fmt.Sprint(insertStart+insertCount+50))
	p.Set(prop.InsertStart, fmt.Sprint(insertStart))
	p.Set(prop.InsertCount, fmt.Sprint(insertCount))
	p.Set(prop.ThreadCount, fmt.Sprint(threads))
	p.Set(prop.BatchSize, fmt.Sprint(batchSize))
	p.Set(prop.FieldCount, "2")
	p.Set(prop.InsertOrder, "ordered")
	p.Set(prop.Silence, "true")
	return p
}

func load(t *testing.T, p *properties.Properties, db ycsb.DB) {
	t.Helper()
	w, err := ycsb.GetWorkloadCreator("core").Create(p)
	if err != nil {
		t.Fatal(err)
	}
	NewClient(p, w, DbWrapper{db}).Run(context.Background())
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k, n := range m {
		for i := 0; i < n; i++ {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// A batched load inserts exactly the keys of [insertstart, insertstart +
// insertcount), each once, the same keys as an unbatched one, also when
// insertcount isn't a multiple of threadcount x batch.size: each thread's
// last batch is cut to what is left of its share.
func TestBatchLoadInsertsExactRange(t *testing.T) {
	const start, count, threads = 37, 1013, 8
	want := make([]string, 0, count)
	for k := start; k < start+count; k++ {
		want = append(want, fmt.Sprintf("user%d", k))
	}
	sort.Strings(want)

	for _, batch := range []int{1, 7, 100, 5000} {
		for _, native := range []bool{true, false} {
			if batch == 1 && !native {
				continue
			}
			t.Run(fmt.Sprintf("batch=%d/native=%v", batch, native), func(t *testing.T) {
				mem := newMemDB()
				var db ycsb.DB = mem
				if native {
					db = batchMemDB{mem}
				}
				load(t, loadProps(start, count, threads, batch), db)
				if got := sortedKeys(mem.keys); !reflect.DeepEqual(got, want) {
					t.Fatalf("inserted %d keys (%v ... %v), want %d (%v ... %v)",
						len(got), first(got), last(got), len(want), first(want), last(want))
				}
				if batch == 1 && len(mem.batches) != 0 {
					t.Fatalf("batch.size=1 batched: %v", mem.batches)
				}
				// thread i loads count/threads (+1 for the first count%threads)
				// records: in ceil(share/batch) batches, all full but the last
				wantBatches := 0
				for i := 0; i < threads; i++ {
					share := count / threads
					if i < count%threads {
						share++
					}
					wantBatches += (share + batch - 1) / batch
				}
				if native && batch > 1 {
					if len(mem.batches) != wantBatches {
						t.Errorf("%d batches, want %d", len(mem.batches), wantBatches)
					}
					for _, n := range mem.batches {
						if n > batch || n < 1 {
							t.Fatalf("a batch of %d records with batch.size=%d", n, batch)
						}
					}
				}
			})
		}
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

type sample struct {
	op string
	n  int64
}

func recordSamples(t *testing.T) *[]sample {
	t.Helper()
	var mu sync.Mutex
	samples := &[]sample{}
	orig := measureN
	measureN = func(op string, _ time.Time, _ time.Duration, n int64) {
		mu.Lock()
		defer mu.Unlock()
		*samples = append(*samples, sample{op, n})
	}
	t.Cleanup(func() { measureN = orig })
	return samples
}

func counts(samples []sample) map[string]int64 {
	c := map[string]int64{}
	for _, s := range samples {
		c[s.op] += s.n
	}
	return c
}

// A batch counts as its records: INSERT/TOTAL for the ones that succeeded,
// INSERT_ERROR for the ones that failed, plus one BATCH_INSERT: no *_ERROR
// count but the failed records'.
func TestBatchInsertMeasurement(t *testing.T) {
	keys := []string{"a", "b", "c", "d", "e"}
	values := make([]map[string][]byte, len(keys))
	for _, c := range []struct {
		name string
		fail func(string) error
		want map[string]int64
	}{
		{"ok", nil, map[string]int64{"INSERT": 5, "TOTAL": 5, "BATCH_INSERT": 1}},
		{"partial", func(k string) error {
			if k == "b" || k == "d" {
				return errors.New("OOM")
			}
			return nil
		}, map[string]int64{"INSERT": 3, "TOTAL": 3, "INSERT_ERROR": 2, "BATCH_INSERT": 1}},
		{"all", func(string) error { return errors.New("down") },
			map[string]int64{"INSERT_ERROR": 5, "BATCH_INSERT": 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			samples := recordSamples(t)
			mem := newMemDB()
			mem.fail = c.fail
			err := DbWrapper{batchMemDB{mem}}.BatchInsert(context.Background(), "t", keys, values)
			if (err != nil) != (c.fail != nil) {
				t.Fatalf("err %v", err)
			}
			if got := counts(*samples); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("samples %v, want %v", got, c.want)
			}
		})
	}
}

// An error that doesn't say which records failed fails them all.
type opaqueBatchDB struct{ *memDB }

func (d opaqueBatchDB) BatchInsert(context.Context, string, []string, []map[string][]byte) error {
	return errors.New("pipeline: connection reset")
}

func TestBatchInsertMeasurementOpaqueError(t *testing.T) {
	samples := recordSamples(t)
	keys := []string{"a", "b", "c"}
	_ = DbWrapper{opaqueBatchDB{newMemDB()}}.BatchInsert(context.Background(), "t", keys, make([]map[string][]byte, 3))
	if got, want := counts(*samples), map[string]int64{"INSERT_ERROR": 3, "BATCH_INSERT": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("samples %v, want %v", got, want)
	}
}

// A DB without BatchInsert gets one Insert per record, each measured, and a
// failed record doesn't stop the rest.
func TestBatchInsertFallback(t *testing.T) {
	samples := recordSamples(t)
	mem := newMemDB()
	mem.fail = func(k string) error {
		if k == "b" {
			return errors.New("OOM")
		}
		return nil
	}
	keys := []string{"a", "b", "c"}
	err := DbWrapper{mem}.BatchInsert(context.Background(), "t", keys, make([]map[string][]byte, 3))
	var be *ycsb.BatchError
	if !errors.As(err, &be) || be.Errs[0] != nil || be.Errs[1] == nil || be.Errs[2] != nil {
		t.Fatalf("err %v, want a BatchError for b alone", err)
	}
	if !reflect.DeepEqual(sortedKeys(mem.keys), []string{"a", "c"}) {
		t.Fatalf("inserted %v, want a and c", sortedKeys(mem.keys))
	}
	if got, want := counts(*samples), map[string]int64{"INSERT": 2, "TOTAL": 2, "INSERT_ERROR": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("samples %v, want %v", got, want)
	}
}

func runProps(operations, threads, batchSize int, readProportion float64) *properties.Properties {
	p := properties.NewProperties()
	p.Set(prop.DoTransactions, "true")
	p.Set(prop.RecordCount, "100")
	p.Set(prop.OperationCount, fmt.Sprint(operations))
	p.Set(prop.ThreadCount, fmt.Sprint(threads))
	p.Set(prop.BatchSize, fmt.Sprint(batchSize))
	p.Set(prop.FieldCount, "2")
	p.Set(prop.ReadProportion, fmt.Sprint(readProportion))
	p.Set(prop.UpdateProportion, fmt.Sprint(1-readProportion))
	p.Set(prop.RequestDistribution, "uniform")
	p.Set(prop.Silence, "true")
	return p
}

// runWorker runs one worker of a run phase to its end.
func runWorker(t *testing.T, p *properties.Properties, db ycsb.DB) *worker {
	t.Helper()
	wl, err := ycsb.GetWorkloadCreator("core").Create(p)
	if err != nil {
		t.Fatal(err)
	}
	wdb := DbWrapper{db}
	w := newWorker(p, 0, 1, wl, wdb)
	ctx := wl.InitThread(context.Background(), 0, 1)
	w.run(wdb.InitThread(ctx, 0, 1))
	return w
}

// A batched run does operationcount operations: its last batch is cut to
// what is left. Without native batch reads/updates (redis), every record is
// a READ or UPDATE of its own, with its own latency, and no BATCH_ sample.
func TestBatchRunTrimsAndMeasuresRecords(t *testing.T) {
	samples := recordSamples(t)
	mem := newMemDB()
	w := runWorker(t, runProps(50, 1, 7, 0.5), mem)
	if w.opsDone != 50 || mem.reads+mem.updates != 50 {
		t.Fatalf("opsDone %d, %d reads + %d updates, want 50", w.opsDone, mem.reads, mem.updates)
	}
	c := counts(*samples)
	if c["READ"] != int64(mem.reads) || c["UPDATE"] != int64(mem.updates) || c["TOTAL"] != 50 {
		t.Fatalf("samples %v for %d reads and %d updates", c, mem.reads, mem.updates)
	}
	for op := range c {
		if strings.HasPrefix(op, "BATCH_") {
			t.Errorf("per-record fallback recorded %s", op)
		}
	}
}

// A warm-up that ends inside a batch: the batch, started in the warm-up, is
// neither measured nor counted, so the run measures exactly operationcount
// operations (before, up to a batch per thread short).
func TestBatchRunWarmUpEndsMidBatch(t *testing.T) {
	samples := recordSamples(t)
	measurement.EnableWarmUp(true)
	t.Cleanup(func() { measurement.EnableWarmUp(false) })
	mem := newMemDB()
	mem.onRead = func(reads int) {
		if reads == 10 { // inside the second batch of 7
			measurement.EnableWarmUp(false)
		}
	}
	w := runWorker(t, runProps(50, 1, 7, 1), mem)
	c := counts(*samples)
	if w.opsDone != 50 || c["READ"] != 50 || c["TOTAL"] != 50 {
		t.Fatalf("opsDone %d, samples %v, want 50 READ and TOTAL", w.opsDone, c)
	}
	if mem.reads != 50+14 {
		t.Errorf("%d reads, want the two warm-up batches' 14 + 50", mem.reads)
	}
}

// The per-record fallback returns every record's values.
func TestBatchReadFallbackValues(t *testing.T) {
	recordSamples(t)
	keys := []string{"a", "b", "c"}
	values, err := DbWrapper{newMemDB()}.BatchRead(context.Background(), "t", keys, nil)
	if err != nil || len(values) != 3 {
		t.Fatalf("BatchRead = %v, %v", values, err)
	}
	for i, v := range values {
		if string(v["field0"]) != keys[i] {
			t.Errorf("record %d: %v", i, v)
		}
	}
}

// fullBatchDB implements all of ycsb.BatchDB.
type fullBatchDB struct{ batchMemDB }

func (d fullBatchDB) BatchRead(_ context.Context, _ string, keys []string, _ []string) ([]map[string][]byte, error) {
	return make([]map[string][]byte, len(keys)), nil
}
func (d fullBatchDB) BatchUpdate(context.Context, string, []string, []map[string][]byte) error {
	return nil
}
func (d fullBatchDB) BatchDelete(_ context.Context, _ string, keys []string) error {
	errs := make([]error, len(keys))
	errs[0] = errors.New("gone")
	return ycsb.NewBatchError(errs)
}

// A DB's native batch reads, updates and deletes count per record too.
func TestNativeBatchMeasurement(t *testing.T) {
	db := DbWrapper{fullBatchDB{batchMemDB{newMemDB()}}}
	keys := []string{"a", "b", "c", "d"}
	ctx := context.Background()
	for _, c := range []struct {
		op   string
		run  func() error
		want map[string]int64
	}{
		{"READ", func() error { _, err := db.BatchRead(ctx, "t", keys, nil); return err },
			map[string]int64{"READ": 4, "TOTAL": 4, "BATCH_READ": 1}},
		{"UPDATE", func() error { return db.BatchUpdate(ctx, "t", keys, make([]map[string][]byte, 4)) },
			map[string]int64{"UPDATE": 4, "TOTAL": 4, "BATCH_UPDATE": 1}},
		{"DELETE", func() error { return db.BatchDelete(ctx, "t", keys) },
			map[string]int64{"DELETE": 3, "TOTAL": 3, "DELETE_ERROR": 1, "BATCH_DELETE": 1}},
	} {
		samples := recordSamples(t)
		_ = c.run()
		if got := counts(*samples); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: samples %v, want %v", c.op, got, c.want)
		}
	}
}

// A batch the worker started in the warm-up records nothing, natively or not.
func TestBatchStartedInWarmUpNotMeasured(t *testing.T) {
	samples := recordSamples(t)
	ctx := withBatchMeasured(context.Background(), false)
	keys := []string{"a", "b"}
	_ = DbWrapper{batchMemDB{newMemDB()}}.BatchInsert(ctx, "t", keys, make([]map[string][]byte, 2))
	_ = DbWrapper{newMemDB()}.BatchInsert(ctx, "t", keys, make([]map[string][]byte, 2))
	if len(*samples) != 0 {
		t.Fatalf("recorded %v", *samples)
	}
}

// A batch started in the warm-up records nothing through a DB's native batch
// reads, updates and deletes either.
func TestNativeBatchStartedInWarmUpNotMeasured(t *testing.T) {
	samples := recordSamples(t)
	ctx := withBatchMeasured(context.Background(), false)
	db := DbWrapper{fullBatchDB{batchMemDB{newMemDB()}}}
	keys := []string{"a", "b"}
	_, _ = db.BatchRead(ctx, "t", keys, nil)
	_ = db.BatchUpdate(ctx, "t", keys, make([]map[string][]byte, 2))
	_ = db.BatchDelete(ctx, "t", keys)
	if len(*samples) != 0 {
		t.Fatalf("recorded %v", *samples)
	}
}

// callDB records each per-record call: operation, key and value.
type callDB struct {
	memDB
	calls []string
}

func (d *callDB) Read(_ context.Context, _ string, key string, _ []string) (map[string][]byte, error) {
	d.calls = append(d.calls, "read "+key)
	return nil, nil
}
func (d *callDB) Update(_ context.Context, _ string, key string, v map[string][]byte) error {
	d.calls = append(d.calls, "update "+key+"="+string(v["f"]))
	return nil
}
func (d *callDB) Insert(_ context.Context, _ string, key string, v map[string][]byte) error {
	d.calls = append(d.calls, "insert "+key+"="+string(v["f"]))
	return nil
}
func (d *callDB) Delete(_ context.Context, _ string, key string) error {
	d.calls = append(d.calls, "delete "+key)
	return nil
}

// The per-record fallback calls the DB with each record's own key and value.
func TestBatchFallbackRecords(t *testing.T) {
	recordSamples(t)
	ctx := context.Background()
	keys := []string{"a", "b", "c"}
	values := []map[string][]byte{{"f": []byte("1")}, {"f": []byte("2")}, {"f": []byte("3")}}
	db := &callDB{}
	w := DbWrapper{db}
	_, _ = w.BatchRead(ctx, "t", keys, nil)
	_ = w.BatchUpdate(ctx, "t", keys, values)
	_ = w.BatchInsert(ctx, "t", keys, values)
	_ = w.BatchDelete(ctx, "t", keys)
	want := []string{"read a", "read b", "read c", "update a=1", "update b=2", "update c=3",
		"insert a=1", "insert b=2", "insert c=3", "delete a", "delete b", "delete c"}
	if !reflect.DeepEqual(db.calls, want) {
		t.Fatalf("calls %v, want %v", db.calls, want)
	}
}

// A thread waiting out its start-up spread stops at once when the run is
// stopped, without sending anything: no batch on a canceled context, so no
// false errors.
func TestStartSpreadHonoursCancel(t *testing.T) {
	p := loadProps(0, 1000, 1, 100)
	p.Set(prop.Target, "1") // one op/s: a spread of up to 100 s
	mem := newMemDB()
	wl, err := ycsb.GetWorkloadCreator("core").Create(p)
	if err != nil {
		t.Fatal(err)
	}
	orig := spreadDraw
	spreadDraw = func(n int64) int64 { return n - 1 } // the longest wait: deterministic
	t.Cleanup(func() { spreadDraw = orig })
	w := newWorker(p, 0, 1, wl, DbWrapper{batchMemDB{mem}})
	ctx, cancel := context.WithCancel(wl.InitThread(context.Background(), 0, 1))
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker didn't stop within 2 s of the cancel")
	}
	if len(mem.batches) != 0 {
		t.Fatalf("sent %v after the cancel", mem.batches)
	}
}

// A target so high that one operation's tick rounds to 0 ns must not panic
// the start-up spread.
func TestStartSpreadZeroTick(t *testing.T) {
	p := loadProps(0, 40, 1, 4)
	p.Set(prop.Target, "1000000000000")
	mem := newMemDB()
	load(t, p, batchMemDB{mem})
	if len(mem.keys) != 40 {
		t.Fatalf("%d keys, want 40", len(mem.keys))
	}
}

// The threads of a throttled batched run start at offsets spread over one
// batch period (batch.size ticks), not one tick: their batches don't all go
// out at once.
func TestStartSpreadCoversBatchPeriod(t *testing.T) {
	const threads, batch = 50, 10
	// 1000 ops/s over 50 threads: 20 ops/s each, a 50 ms tick, a 500 ms batch period
	p := loadProps(0, threads*batch, threads, batch)
	p.Set(prop.Target, "1000")
	var mu sync.Mutex
	var starts []time.Time
	db := spreadDB{newMemDB(), func() {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
	}}
	load(t, p, db)
	if len(starts) != threads {
		t.Fatalf("%d batches, want one per thread (%d)", len(starts), threads)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	// 50 uniform draws over 500 ms: their range is over 250 ms but for odds
	// of about 1e-13; spread over one 50 ms tick, it can't be
	if span := starts[len(starts)-1].Sub(starts[0]); span < 250*time.Millisecond {
		t.Fatalf("the threads' first batches span %v, want them spread over the 500 ms batch period", span)
	}
}

type spreadDB struct {
	*memDB
	sent func()
}

func (d spreadDB) BatchInsert(ctx context.Context, table string, keys []string, values []map[string][]byte) error {
	d.sent()
	return batchMemDB{d.memDB}.BatchInsert(ctx, table, keys, values)
}

// A run stopped while a batch-less DB's records are run one by one: the rest
// of the batch isn't run, and nothing the stop failed counts as an error.
func TestBatchFallbackStopsOnCancel(t *testing.T) {
	samples := recordSamples(t)
	ctx, cancel := context.WithCancel(context.Background())
	mem := newMemDB()
	mem.onRead = func(reads int) {
		if reads == 2 {
			cancel()
		}
	}
	_, err := DbWrapper{mem}.BatchRead(ctx, "t", []string{"a", "b", "c", "d"}, nil)
	if mem.reads != 2 {
		t.Fatalf("%d reads, want 2: none after the stop", mem.reads)
	}
	if got, want := counts(*samples), map[string]int64{"READ": 2, "TOTAL": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("samples %v, want %v", got, want)
	}
	var be *ycsb.BatchError
	if !errors.As(err, &be) || len(be.Errs) != 4 || be.Errs[0] != nil || be.Errs[1] != nil ||
		!errors.Is(be.Errs[2], context.Canceled) || !errors.Is(be.Errs[3], context.Canceled) ||
		!errors.Is(be.Errs[2], ycsb.ErrNotRun) || !errors.Is(be.Errs[3], ycsb.ErrNotRun) {
		t.Errorf("err %v, want records 3 and 4 failed with the stop, 1 and 2 not", err)
	}

	// a batch that starts after the stop runs nothing
	mem = newMemDB()
	*samples = nil
	_, err = DbWrapper{mem}.BatchRead(ctx, "t", []string{"a", "b"}, nil)
	if mem.reads != 0 || len(*samples) != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("pre-stopped batch: %d reads, samples %v, err %v", mem.reads, *samples, err)
	}
}

// The stop rule, for every DB: an operation the stop came before isn't
// handed to the DB and counts as nothing; one the DB was given counts as it
// ends, a context.Canceled from the DB (a driver giving up in flight) as an
// error, as before batches: it may have been written.
func TestStopRule(t *testing.T) {
	keys := []string{"a", "b", "c"}
	for _, c := range []struct {
		name string
		err  error
		want map[string]int64
	}{
		{"all", context.Canceled, map[string]int64{"INSERT_ERROR": 3, "BATCH_INSERT": 1}},
		{"wrapped", fmt.Errorf("pipeline: %w", context.Canceled), map[string]int64{"INSERT_ERROR": 3, "BATCH_INSERT": 1}},
		{"some", &ycsb.BatchError{Errs: []error{nil, context.Canceled, errors.New("OOM")}},
			map[string]int64{"INSERT": 1, "TOTAL": 1, "INSERT_ERROR": 2, "BATCH_INSERT": 1}},
		// a record the DB didn't run (the stop came before it) counts as
		// nothing; the batch, sent, counts once
		{"some not run", &ycsb.BatchError{Errs: []error{nil, fmt.Errorf("%w: %w", ycsb.ErrNotRun, context.Canceled), nil}},
			map[string]int64{"INSERT": 2, "TOTAL": 2, "BATCH_INSERT": 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			samples := recordSamples(t)
			_ = DbWrapper{errBatchDB{newMemDB(), c.err}}.BatchInsert(context.Background(), "t", keys, make([]map[string][]byte, 3))
			if got := counts(*samples); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("samples %v, want %v", got, c.want)
			}
		})
	}

	// in flight: the DB returns context.Canceled for an operation it was given
	samples := recordSamples(t)
	w := DbWrapper{cancelDB{newMemDB()}}
	ctx, cancel := context.WithCancel(context.Background())
	inFlight := context.WithValue(ctx, cancelInFlight{}, cancel)
	_ = w.Insert(inFlight, "t", "a", nil)
	if got, want := counts(*samples), map[string]int64{"INSERT_ERROR": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("an Insert the DB was given, stopped in flight: %v, want %v", got, want)
	}

	// after the stop: nothing is handed to the DB, nothing is measured
	*samples = nil
	db := &countingDB{memDB: newMemDB()}
	w = DbWrapper{db}
	_, _ = w.Read(ctx, "t", "a", nil)
	_ = w.Update(ctx, "t", "a", nil)
	_ = w.Insert(ctx, "t", "a", nil)
	_ = w.Delete(ctx, "t", "a")
	_, _ = w.Scan(ctx, "t", "a", 1, nil)
	_, _ = w.BatchRead(ctx, "t", keys, nil)
	_ = w.BatchUpdate(ctx, "t", keys, make([]map[string][]byte, 3))
	_ = w.BatchInsert(ctx, "t", keys, make([]map[string][]byte, 3))
	_ = w.BatchDelete(ctx, "t", keys)
	_ = DbWrapper{fullBatchDB{batchMemDB{newMemDB()}}}.BatchInsert(ctx, "t", keys, make([]map[string][]byte, 3))
	if db.calls != 0 || len(*samples) != 0 {
		t.Fatalf("after the stop: %d DB calls, samples %v; want none", db.calls, *samples)
	}
}

type cancelInFlight struct{}

// countingDB counts the calls it gets.
type countingDB struct {
	*memDB
	calls int
}

func (d *countingDB) Read(context.Context, string, string, []string) (map[string][]byte, error) {
	d.calls++
	return nil, nil
}
func (d *countingDB) Update(context.Context, string, string, map[string][]byte) error {
	d.calls++
	return nil
}
func (d *countingDB) Insert(context.Context, string, string, map[string][]byte) error {
	d.calls++
	return nil
}
func (d *countingDB) Delete(context.Context, string, string) error { d.calls++; return nil }
func (d *countingDB) Scan(context.Context, string, string, int, []string) ([]map[string][]byte, error) {
	d.calls++
	return nil, nil
}

type errBatchDB struct {
	*memDB
	err error
}

func (d errBatchDB) BatchInsert(context.Context, string, []string, []map[string][]byte) error {
	return d.err
}

// cancelDB's Insert is stopped while in flight: the run's stop comes while
// the DB has it, and it returns the context's error.
type cancelDB struct{ *memDB }

func (d cancelDB) Insert(ctx context.Context, _ string, _ string, _ map[string][]byte) error {
	if cancel, ok := ctx.Value(cancelInFlight{}).(context.CancelFunc); ok {
		cancel()
	}
	return ctx.Err()
}

// sleepDB's Read takes as long as its key says.
type sleepDB struct{ *memDB }

func (d sleepDB) Read(_ context.Context, _ string, key string, _ []string) (map[string][]byte, error) {
	if key == "slow" {
		time.Sleep(30 * time.Millisecond)
	}
	return nil, nil
}

// Without the batch operation, each record is timed on its own: a fast record
// after a slow one isn't charged the slow one's time.
func TestBatchFallbackTimesEachRecord(t *testing.T) {
	var lats []time.Duration
	orig := measureN
	measureN = func(op string, _ time.Time, lan time.Duration, _ int64) {
		if op == "READ" {
			lats = append(lats, lan)
		}
	}
	t.Cleanup(func() { measureN = orig })
	_, _ = DbWrapper{sleepDB{newMemDB()}}.BatchRead(context.Background(), "t", []string{"slow", "fast"}, nil)
	if len(lats) != 2 || lats[0] < 30*time.Millisecond || lats[1] >= 30*time.Millisecond {
		t.Fatalf("latencies %v, want the slow record >= 30ms and the fast one < 30ms", lats)
	}
}

// The same at a per-thread rate over 1 op/ms (where a plain run has no
// spread at all), as at large scale: 50 threads at 5 ops/ms each, batches of
// 1000, a 200 ms batch period.
func TestStartSpreadAtHighRate(t *testing.T) {
	const threads, batch = 50, 1000
	p := loadProps(0, threads*batch, threads, batch)
	p.Set(prop.Target, fmt.Sprint(threads*5000))
	var mu sync.Mutex
	var starts []time.Time
	load(t, p, spreadDB{newMemDB(), func() {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
	}})
	if len(starts) != threads {
		t.Fatalf("%d batches, want %d", len(starts), threads)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	if span := starts[len(starts)-1].Sub(starts[0]); span < 100*time.Millisecond {
		t.Fatalf("the threads' first batches span %v, want them spread over the 200 ms batch period", span)
	}
}

// batchOutcome: a BatchError covering the batch splits its records (one by
// one: errors.Is on the BatchError would match if any record matched); any
// other error fails them all, the stop's context error included, unless it
// wraps ycsb.ErrNotRun.
func TestBatchOutcome(t *testing.T) {
	oom := errors.New("OOM")
	notRun := fmt.Errorf("%w: %w", ycsb.ErrNotRun, context.Canceled)
	be := ycsb.NewBatchError([]error{nil, oom, nil, oom})
	for _, c := range []struct {
		err            error
		n              int
		failed, notRun int
	}{
		{nil, 4, 0, 0},
		{be, 4, 2, 0},
		{fmt.Errorf("wrapped: %w", be), 4, 2, 0},
		{be, 5, 5, 0},  // doesn't cover the batch: all failed
		{oom, 4, 4, 0}, // says nothing about records: all failed
		{&ycsb.BatchError{Errs: make([]error, 4)}, 4, 4, 0}, // an error without a failed record
		{context.Canceled, 4, 4, 0},
		{ycsb.NewBatchError([]error{context.Canceled, oom, nil}), 3, 2, 0},
		{notRun, 4, 0, 4},
		{ycsb.NewBatchError([]error{notRun, oom, nil, notRun}), 4, 1, 2},
		// not covering the batch, one not-run record says nothing of the rest
		{ycsb.NewBatchError([]error{notRun, nil}), 4, 4, 0},
	} {
		if f, nr := batchOutcome(c.err, c.n); f != c.failed || nr != c.notRun {
			t.Errorf("batchOutcome(%v, %d) = %d failed, %d not run; want %d, %d", c.err, c.n, f, nr, c.failed, c.notRun)
		}
	}
}

// flipCtx is a context the run's stop cancels right after the client's
// check: its first Err is nil, the next ones context.Canceled.
type flipCtx struct {
	context.Context
	calls atomic.Int32
}

func (c *flipCtx) Err() error {
	if c.calls.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

// startedDB checks the context again before running anything, as the redis
// adapter does: what the stop came before isn't run, with an error wrapping
// ycsb.ErrNotRun.
type startedDB struct {
	*memDB
	calls int
}

func (d *startedDB) started(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ycsb.ErrNotRun, err)
	}
	d.calls++
	return nil
}

func (d *startedDB) Insert(ctx context.Context, _ string, _ string, _ map[string][]byte) error {
	return d.started(ctx)
}

func (d *startedDB) BatchInsert(ctx context.Context, _ string, keys []string, _ []map[string][]byte) error {
	if err := d.started(ctx); err != nil {
		errs := make([]error, len(keys))
		for i := range errs {
			errs[i] = err
		}
		return ycsb.NewBatchError(errs)
	}
	return nil
}

// The stop coming between the client's check and the DB's: the DB doesn't
// run the operation, and it counts as nothing, not as an error.
func TestStopBetweenTheChecks(t *testing.T) {
	samples := recordSamples(t)
	db := &startedDB{memDB: newMemDB()}
	w := DbWrapper{db}
	if err := w.Insert(&flipCtx{Context: context.Background()}, "t", "a", nil); !errors.Is(err, ycsb.ErrNotRun) {
		t.Errorf("Insert: %v, want ycsb.ErrNotRun", err)
	}
	err := w.BatchInsert(&flipCtx{Context: context.Background()}, "t", []string{"a", "b"}, make([]map[string][]byte, 2))
	if !errors.Is(err, ycsb.ErrNotRun) {
		t.Errorf("BatchInsert: %v, want ycsb.ErrNotRun", err)
	}
	if db.calls != 0 || len(*samples) != 0 {
		t.Fatalf("%d operations run, samples %v; want none", db.calls, *samples)
	}
}

// countingBatchDB implements all of ycsb.BatchDB and counts the calls.
type countingBatchDB struct {
	*memDB
	calls int
}

func (d *countingBatchDB) BatchInsert(context.Context, string, []string, []map[string][]byte) error {
	d.calls++
	return nil
}
func (d *countingBatchDB) BatchRead(_ context.Context, _ string, keys []string, _ []string) ([]map[string][]byte, error) {
	d.calls++
	return make([]map[string][]byte, len(keys)), nil
}
func (d *countingBatchDB) BatchUpdate(context.Context, string, []string, []map[string][]byte) error {
	d.calls++
	return nil
}
func (d *countingBatchDB) BatchDelete(context.Context, string, []string) error {
	d.calls++
	return nil
}

// A DB's native batch operations are not handed a batch after the run's
// stop either: no call, no sample, and an error wrapping ycsb.ErrNotRun.
func TestNativeBatchNotRunAfterStop(t *testing.T) {
	samples := recordSamples(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := &countingBatchDB{memDB: newMemDB()}
	w := DbWrapper{db}
	keys := []string{"a", "b"}
	vals := make([]map[string][]byte, 2)
	_, errRead := w.BatchRead(ctx, "t", keys, nil)
	for name, err := range map[string]error{
		"BatchRead":   errRead,
		"BatchUpdate": w.BatchUpdate(ctx, "t", keys, vals),
		"BatchInsert": w.BatchInsert(ctx, "t", keys, vals),
		"BatchDelete": w.BatchDelete(ctx, "t", keys),
	} {
		if !errors.Is(err, ycsb.ErrNotRun) || !errors.Is(err, context.Canceled) {
			t.Errorf("%s after the stop: %v, want ycsb.ErrNotRun and the stop", name, err)
		}
	}
	if db.calls != 0 || len(*samples) != 0 {
		t.Fatalf("after the stop: %d native batch calls, samples %v; want none", db.calls, *samples)
	}
	// and before the stop, they are called
	if err := w.BatchDelete(context.Background(), "t", keys); err != nil || db.calls != 1 {
		t.Fatalf("before the stop: err %v, %d calls", err, db.calls)
	}
}

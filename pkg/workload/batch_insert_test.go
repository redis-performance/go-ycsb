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

package workload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/client"
	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

func TestMain(m *testing.M) {
	// DbWrapper measures through the measurement package
	measurement.InitMeasure(properties.NewProperties())
	os.Exit(m.Run())
}

// batchRecorder is a ycsb.DB with BatchInsert that records every call and
// answers it with fail's error.
type batchRecorder struct {
	calls  [][]string
	values []map[string]string // the latest call's values per key
	fail   func(call int, keys []string) error
}

func (b *batchRecorder) BatchInsert(_ context.Context, _ string, keys []string, values []map[string][]byte) error {
	b.calls = append(b.calls, append([]string(nil), keys...))
	b.values = b.values[:0]
	for _, v := range values {
		m := map[string]string{}
		for f, bs := range v {
			m[f] = string(bs)
		}
		b.values = append(b.values, m)
	}
	if b.fail == nil {
		return nil
	}
	return b.fail(len(b.calls), keys)
}

func (b *batchRecorder) Close() error                                             { return nil }
func (b *batchRecorder) InitThread(ctx context.Context, _, _ int) context.Context { return ctx }
func (b *batchRecorder) CleanupThread(context.Context)                            {}
func (b *batchRecorder) Read(context.Context, string, string, []string) (map[string][]byte, error) {
	return nil, nil
}
func (b *batchRecorder) Scan(context.Context, string, string, int, []string) ([]map[string][]byte, error) {
	return nil, nil
}
func (b *batchRecorder) Update(context.Context, string, string, map[string][]byte) error { return nil }
func (b *batchRecorder) Insert(context.Context, string, string, map[string][]byte) error { return nil }
func (b *batchRecorder) Delete(context.Context, string, string) error                    { return nil }

func newBatchCore(t *testing.T, retryLimit int) (ycsb.Workload, context.Context) {
	t.Helper()
	p := properties.NewProperties()
	p.Set(prop.RecordCount, "1000")
	p.Set(prop.FieldCount, "3")
	p.Set(prop.InsertOrder, "ordered")
	p.Set(prop.InsertStart, "100")
	p.Set(prop.InsertionRetryLimit, fmt.Sprint(retryLimit))
	p.Set(prop.InsertionRetryInterval, "0")
	w, err := coreCreator{}.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	return w, w.InitThread(context.Background(), 0, 1)
}

func TestDoBatchInsertSuccessIsNotRetried(t *testing.T) {
	w, ctx := newBatchCore(t, 3)
	db := &batchRecorder{}
	if err := w.DoBatchInsert(ctx, 5, db); err != nil {
		t.Fatal(err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("%d BatchInsert calls for one successful batch, want 1: %v", len(db.calls), db.calls)
	}
	want := []string{"user100", "user101", "user102", "user103", "user104"}
	if !reflect.DeepEqual(db.calls[0], want) {
		t.Fatalf("keys %v, want %v", db.calls[0], want)
	}
}

// Only the records that failed are retried, with the values they were built
// with.
func TestDoBatchInsertRetriesFailedRecords(t *testing.T) {
	w, ctx := newBatchCore(t, 3)
	var firstValues []map[string]string
	db := &batchRecorder{}
	db.fail = func(call int, keys []string) error {
		if call == 1 {
			firstValues = append(firstValues, db.values...)
			errs := make([]error, len(keys))
			errs[1], errs[3] = errors.New("OOM"), errors.New("OOM")
			return &ycsb.BatchError{Errs: errs}
		}
		return nil
	}
	if err := w.DoBatchInsert(ctx, 5, db); err != nil {
		t.Fatal(err)
	}
	if len(db.calls) != 2 {
		t.Fatalf("%d calls, want 2: %v", len(db.calls), db.calls)
	}
	if want := []string{"user101", "user103"}; !reflect.DeepEqual(db.calls[1], want) {
		t.Fatalf("retried %v, want %v", db.calls[1], want)
	}
	if !reflect.DeepEqual(db.values, []map[string]string{firstValues[1], firstValues[3]}) {
		t.Fatalf("retried values %v, want the first attempt's %v and %v", db.values, firstValues[1], firstValues[3])
	}
}

// Without a per-record error, the whole batch failed and is retried, up to the
// retry limit; the last error is returned.
func TestDoBatchInsertRetryLimit(t *testing.T) {
	for _, limit := range []int{0, 2} {
		w, ctx := newBatchCore(t, limit)
		boom := errors.New("connection refused")
		db := &batchRecorder{fail: func(int, []string) error { return boom }}
		if err := w.DoBatchInsert(ctx, 4, db); !errors.Is(err, boom) {
			t.Fatalf("limit %d: err %v, want %v", limit, err, boom)
		}
		if len(db.calls) != limit+1 {
			t.Fatalf("limit %d: %d calls, want %d", limit, len(db.calls), limit+1)
		}
		for _, c := range db.calls {
			if len(c) != 4 {
				t.Fatalf("limit %d: a retry sent %v, want the whole batch", limit, c)
			}
		}
	}
}

// A BatchError that doesn't cover the batch says nothing usable about which
// records failed: the whole batch is retried.
func TestDoBatchInsertBatchErrorOfOtherLength(t *testing.T) {
	w, ctx := newBatchCore(t, 1)
	db := &batchRecorder{fail: func(call int, keys []string) error {
		if call == 1 {
			return &ycsb.BatchError{Errs: []error{errors.New("OOM")}}
		}
		return nil
	}}
	if err := w.DoBatchInsert(ctx, 4, db); err != nil {
		t.Fatal(err)
	}
	if len(db.calls) != 2 || len(db.calls[1]) != 4 {
		t.Fatalf("calls %v, want the whole batch retried once", db.calls)
	}
}

// A run stopped (its context canceled) doesn't retry a failed batch, and ends
// without an error, as DoInsert does.
func TestDoBatchInsertCanceled(t *testing.T) {
	w, ctx := newBatchCore(t, 3)
	ctx, cancel := context.WithCancel(ctx)
	db := &batchRecorder{fail: func(int, []string) error {
		cancel()
		return context.Canceled
	}}
	if err := w.DoBatchInsert(ctx, 4, db); err != nil {
		t.Fatalf("err %v, want nil once canceled", err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("%d calls, want no retry once canceled", len(db.calls))
	}
}

// A run stopped during the retry back-off doesn't wait it out, nor resend.
func TestDoBatchInsertBackoffHonoursCancel(t *testing.T) {
	w, ctx := newBatchCore(t, 3)
	ctx, cancel := context.WithCancel(ctx)
	w.(*core).insertionRetryInterval = 60
	db := &batchRecorder{fail: func(int, []string) error { return errors.New("OOM") }}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() { done <- w.DoBatchInsert(ctx, 4, db) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err %v, want nil once stopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DoBatchInsert waited out a 60 s back-off after the stop")
	}
	if len(db.calls) != 1 {
		t.Fatalf("%d calls, want no resend after the stop", len(db.calls))
	}
}

// The same for DoInsert.
func TestDoInsertBackoffHonoursCancel(t *testing.T) {
	w, ctx := newBatchCore(t, 3)
	ctx, cancel := context.WithCancel(ctx)
	w.(*core).insertionRetryInterval = 60
	db := &batchRecorder{}
	calls := 0
	failing := insertFunc(func() error { calls++; return errors.New("OOM") })
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() { done <- w.DoInsert(ctx, failingInsertDB{db, failing}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err %v, want nil once stopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DoInsert waited out a 60 s back-off after the stop")
	}
	if calls != 1 {
		t.Fatalf("%d inserts, want no resend after the stop", calls)
	}
}

type insertFunc func() error

type failingInsertDB struct {
	*batchRecorder
	insert insertFunc
}

func (d failingInsertDB) Insert(context.Context, string, string, map[string][]byte) error {
	return d.insert()
}

// rmwDB fails its reads with readErr and its updates with updateErr.
type rmwDB struct {
	*batchRecorder
	readErr, updateErr error
}

func (d rmwDB) Read(context.Context, string, string, []string) (map[string][]byte, error) {
	return nil, d.readErr
}

func (d rmwDB) Update(context.Context, string, string, map[string][]byte) error {
	return d.updateErr
}

// READ_MODIFY_WRITE follows the rule of READ and UPDATE: a failure, of its
// read or its update, a context.Canceled from the DB included, is
// READ_MODIFY_WRITE_ERROR, and one the run's stop came before records nothing.
func TestReadModifyWriteMeasurement(t *testing.T) {
	var got []string
	orig := measure
	measure = func(op string, _ time.Time, _ time.Duration) { got = append(got, op) }
	t.Cleanup(func() { measure = orig })
	w, ctx := newBatchCore(t, 0)
	c := w.(*core)
	state := ctx.Value(stateKey).(*coreState)
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		ctx                context.Context
		readErr, updateErr error
		want               []string
	}{
		{ctx, nil, nil, []string{"READ_MODIFY_WRITE"}},
		{ctx, errors.New("OOM"), nil, []string{"READ_MODIFY_WRITE_ERROR"}},
		{ctx, nil, errors.New("OOM"), []string{"READ_MODIFY_WRITE_ERROR"}},
		{ctx, fmt.Errorf("redis: %w", context.Canceled), nil, []string{"READ_MODIFY_WRITE_ERROR"}},
		{ctx, nil, fmt.Errorf("redis: %w", context.Canceled), []string{"READ_MODIFY_WRITE_ERROR"}},
		{ctx, nil, fmt.Errorf("%w: %w", ycsb.ErrNotRun, context.Canceled), nil},
		{stopped, nil, nil, nil},
	} {
		got = nil
		_ = c.doTransactionReadModifyWrite(tc.ctx, rmwDB{&batchRecorder{}, tc.readErr, tc.updateErr}, state)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("read error %v, update error %v, stopped %v: recorded %v, want %v",
				tc.readErr, tc.updateErr, tc.ctx.Err() != nil, got, tc.want)
		}
	}
}

// stopDuringRead's Read is in flight when the run stops: it succeeds, and
// the stop comes before the update half.
type stopDuringRead struct {
	*batchRecorder
	stop    context.CancelFunc
	updates int
}

func (d *stopDuringRead) Read(context.Context, string, string, []string) (map[string][]byte, error) {
	d.stop()
	return nil, nil
}

func (d *stopDuringRead) Update(context.Context, string, string, map[string][]byte) error {
	d.updates++
	return nil
}

// A clean stop between a READ_MODIFY_WRITE's read and its update: the client
// doesn't send the update (the stop rule), and the operation counts as not
// run, not as READ_MODIFY_WRITE_ERROR.
func TestReadModifyWriteStoppedBeforeItsUpdate(t *testing.T) {
	var got []string
	orig := measure
	measure = func(op string, _ time.Time, _ time.Duration) { got = append(got, op) }
	t.Cleanup(func() { measure = orig })
	w, ctx := newBatchCore(t, 0)
	c := w.(*core)
	state := ctx.Value(stateKey).(*coreState)
	run, stop := context.WithCancel(ctx)
	db := &stopDuringRead{batchRecorder: &batchRecorder{}, stop: stop}
	err := c.doTransactionReadModifyWrite(run, client.DbWrapper{DB: db}, state)
	if !errors.Is(err, ycsb.ErrNotRun) || db.updates != 0 {
		t.Fatalf("err %v, %d updates sent: want the update not run", err, db.updates)
	}
	for _, op := range got {
		if strings.HasPrefix(op, "READ_MODIFY_WRITE") {
			t.Fatalf("recorded %s for a READ_MODIFY_WRITE the stop cut before its update", op)
		}
	}
}

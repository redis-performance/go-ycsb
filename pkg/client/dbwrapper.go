// Copyright 2018 PingCAP, Inc.
//
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
	"time"

	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// DbWrapper stores the pointer to a implementation of ycsb.DB.
type DbWrapper struct {
	DB ycsb.DB
}

// measure measures an operation that took from start to now: as op (and
// TOTAL), or op_ERROR if it failed, or nothing if the DB didn't run it
// because the run had stopped (an error wrapping ycsb.ErrNotRun: the stop
// came between the client's check and the DB's).
func measure(start time.Time, op string, err error) {
	if errors.Is(err, ycsb.ErrNotRun) {
		return
	}
	lan := time.Now().Sub(start)
	if err != nil {
		measureN(fmt.Sprintf("%s_ERROR", op), start, lan, 1)
		return
	}

	measureN(op, start, lan, 1)
	measureN("TOTAL", start, lan, 1)
}

// measureBatch measures a batch of n records of op that took from start to
// now: each record counts as one op (and TOTAL, if it succeeded) or one
// op_ERROR, with the batch's latency, since that is when it completed; a
// failed record is one the DB reported in a *ycsb.BatchError, or every record
// for any other error. A record the DB didn't run because the run had
// stopped (its error wraps ycsb.ErrNotRun) counts as nothing. The batch
// itself is one BATCH_op sample, failed or not (unless none of it ran),
// counted in no TOTAL: the *_ERROR counts add up to the failed records alone.
func measureBatch(start time.Time, op string, n int, err error) {
	lan := time.Now().Sub(start)
	failed, notRun := batchOutcome(err, n)
	if ok := int64(n - failed - notRun); ok > 0 {
		measureN(op, start, lan, ok)
		measureN("TOTAL", start, lan, ok)
	}
	if failed > 0 {
		measureN(op+"_ERROR", start, lan, int64(failed))
	}
	if notRun < n {
		measureN("BATCH_"+op, start, lan, 1)
	}
}

// batchOutcome returns how many of a batch's n records failed, and how many
// the DB didn't run because the run had stopped: per record for a
// *ycsb.BatchError that covers the batch (errors.Is on the BatchError itself
// would match if any one record matched), else for the whole batch.
func batchOutcome(err error, n int) (failed, notRun int) {
	if err == nil {
		return 0, 0
	}
	var be *ycsb.BatchError
	if errors.As(err, &be) {
		if len(be.Errs) != n || be.Failed() == 0 {
			return n, 0 // it doesn't say which records failed: all did
		}
		for _, recErr := range be.Errs {
			switch {
			case recErr == nil:
			case errors.Is(recErr, ycsb.ErrNotRun):
				notRun++
			default:
				failed++
			}
		}
		return failed, notRun
	}
	if errors.Is(err, ycsb.ErrNotRun) {
		return 0, n
	}
	return n, 0
}

// measureN records the samples (a variable for the tests).
var measureN = measurement.MeasureN

type batchMeasuredKey struct{}

// withBatchMeasured marks a batch's context with whether its records are
// measured: the worker decides it once, when the batch starts, and counts the
// batch's operations accordingly, so a warm-up that ends while the batch runs
// can't leave some of its records measured and the rest not.
func withBatchMeasured(ctx context.Context, measured bool) context.Context {
	return context.WithValue(ctx, batchMeasuredKey{}, measured)
}

// batchMeasured says whether a batch's records are measured: as the worker
// decided, or, for a batch it didn't start, whether the warm-up is over.
func batchMeasured(ctx context.Context) bool {
	if measured, ok := ctx.Value(batchMeasuredKey{}).(bool); ok {
		return measured
	}
	return measurement.IsWarmUpFinished()
}

// eachRecord runs op on every record of a batch, on its own, for a DB without
// the batch operation: each record is measured as op with its own latency,
// if the batch is measured. Once the run is stopped it runs no more records.
// It returns the records' errors.
func eachRecord(ctx context.Context, name string, n int, op func(i int) error) error {
	measured := batchMeasured(ctx)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		// the run stopped: the rest of the batch isn't run (nor measured)
		if err := ctx.Err(); err != nil {
			for ; i < n; i++ {
				errs[i] = notRun(err)
			}
			break
		}
		start := time.Now()
		errs[i] = op(i)
		if measured {
			measure(start, name, errs[i])
		}
	}
	return ycsb.NewBatchError(errs)
}

func (db DbWrapper) Close() error {
	return db.DB.Close()
}

func (db DbWrapper) InitThread(ctx context.Context, threadID int, threadCount int) context.Context {
	return db.DB.InitThread(ctx, threadID, threadCount)
}

func (db DbWrapper) CleanupThread(ctx context.Context) {
	db.DB.CleanupThread(ctx)
}

// notRun is the error of an operation the run's stop came before: it wraps
// ycsb.ErrNotRun (so that a caller such as READ_MODIFY_WRITE can tell it
// wasn't run) and the stop's error.
func notRun(err error) error {
	return fmt.Errorf("%w: %w", ycsb.ErrNotRun, err)
}

// Every operation follows one rule for the run's stop (its context
// canceled): an operation the stop came before isn't run, and counts as
// nothing; one already handed to the DB is measured as it ends, a
// context.Canceled from the DB included, as any error.

func (db DbWrapper) Read(ctx context.Context, table string, key string, fields []string) (_ map[string][]byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, notRun(err)
	}
	start := time.Now()
	defer func() {
		measure(start, "READ", err)
	}()

	return db.DB.Read(ctx, table, key, fields)
}

func (db DbWrapper) BatchRead(ctx context.Context, table string, keys []string, fields []string) (_ []map[string][]byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, notRun(err)
	}
	batchDB, ok := db.DB.(ycsb.BatchDB)
	if ok {
		start, measured := time.Now(), batchMeasured(ctx)
		defer func() {
			if measured {
				measureBatch(start, "READ", len(keys), err)
			}
		}()
		return batchDB.BatchRead(ctx, table, keys, fields)
	}
	values := make([]map[string][]byte, len(keys))
	err = eachRecord(ctx, "READ", len(keys), func(i int) (err error) {
		values[i], err = db.DB.Read(ctx, table, keys[i], fields)
		return err
	})
	return values, err
}

func (db DbWrapper) Scan(ctx context.Context, table string, startKey string, count int, fields []string) (_ []map[string][]byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, notRun(err)
	}
	start := time.Now()
	defer func() {
		measure(start, "SCAN", err)
	}()

	return db.DB.Scan(ctx, table, startKey, count, fields)
}

func (db DbWrapper) Update(ctx context.Context, table string, key string, values map[string][]byte) (err error) {
	if err := ctx.Err(); err != nil {
		return notRun(err)
	}
	start := time.Now()
	defer func() {
		measure(start, "UPDATE", err)
	}()

	return db.DB.Update(ctx, table, key, values)
}

func (db DbWrapper) BatchUpdate(ctx context.Context, table string, keys []string, values []map[string][]byte) (err error) {
	if err := ctx.Err(); err != nil {
		return notRun(err)
	}
	batchDB, ok := db.DB.(ycsb.BatchDB)
	if ok {
		start, measured := time.Now(), batchMeasured(ctx)
		defer func() {
			if measured {
				measureBatch(start, "UPDATE", len(keys), err)
			}
		}()
		return batchDB.BatchUpdate(ctx, table, keys, values)
	}
	return eachRecord(ctx, "UPDATE", len(keys), func(i int) error {
		return db.DB.Update(ctx, table, keys[i], values[i])
	})
}

func (db DbWrapper) Insert(ctx context.Context, table string, key string, values map[string][]byte) (err error) {
	if err := ctx.Err(); err != nil {
		return notRun(err)
	}
	start := time.Now()
	defer func() {
		measure(start, "INSERT", err)
	}()

	return db.DB.Insert(ctx, table, key, values)
}

// BatchInsert needs only ycsb.BatchInserter of the DB, so a DB can batch its
// load without implementing the rest of ycsb.BatchDB.
func (db DbWrapper) BatchInsert(ctx context.Context, table string, keys []string, values []map[string][]byte) (err error) {
	if err := ctx.Err(); err != nil {
		return notRun(err)
	}
	batchDB, ok := db.DB.(ycsb.BatchInserter)
	if ok {
		start, measured := time.Now(), batchMeasured(ctx)
		defer func() {
			if measured {
				measureBatch(start, "INSERT", len(keys), err)
			}
		}()
		return batchDB.BatchInsert(ctx, table, keys, values)
	}
	return eachRecord(ctx, "INSERT", len(keys), func(i int) error {
		return db.DB.Insert(ctx, table, keys[i], values[i])
	})
}

func (db DbWrapper) Delete(ctx context.Context, table string, key string) (err error) {
	if err := ctx.Err(); err != nil {
		return notRun(err)
	}
	start := time.Now()
	defer func() {
		measure(start, "DELETE", err)
	}()

	return db.DB.Delete(ctx, table, key)
}

func (db DbWrapper) BatchDelete(ctx context.Context, table string, keys []string) (err error) {
	if err := ctx.Err(); err != nil {
		return notRun(err)
	}
	batchDB, ok := db.DB.(ycsb.BatchDB)
	if ok {
		start, measured := time.Now(), batchMeasured(ctx)
		defer func() {
			if measured {
				measureBatch(start, "DELETE", len(keys), err)
			}
		}()
		return batchDB.BatchDelete(ctx, table, keys)
	}
	return eachRecord(ctx, "DELETE", len(keys), func(i int) error {
		return db.DB.Delete(ctx, table, keys[i])
	})
}

func (db DbWrapper) Analyze(ctx context.Context, table string) error {
	if analyzeDB, ok := db.DB.(ycsb.AnalyzeDB); ok {
		return analyzeDB.Analyze(ctx, table)
	}
	return nil
}

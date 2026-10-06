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

package ycsb

import (
	"context"
	"errors"
	"fmt"

	"github.com/magiconair/properties"
)

// DBCreator creates a database layer.
type DBCreator interface {
	Create(p *properties.Properties) (DB, error)
}

// DB is the layer to access the database to be benchmarked.
type DB interface {
	// Close closes the database layer.
	Close() error

	// InitThread initializes the state associated to the goroutine worker.
	// The Returned context will be passed to the following usage.
	InitThread(ctx context.Context, threadID int, threadCount int) context.Context

	// CleanupThread cleans up the state when the worker finished.
	CleanupThread(ctx context.Context)

	// Read reads a record from the database and returns a map of each field/value pair.
	// table: The name of the table.
	// key: The record key of the record to read.
	// fields: The list of fields to read, nil|empty for reading all.
	Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error)

	// Scan scans records from the database.
	// table: The name of the table.
	// startKey: The first record key to read.
	// count: The number of records to read.
	// fields: The list of fields to read, nil|empty for reading all.
	Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error)

	// Update updates a record in the database. Any field/value pairs will be written into the
	// database or overwritten the existing values with the same field name.
	// table: The name of the table.
	// key: The record key of the record to update.
	// values: A map of field/value pairs to update in the record.
	Update(ctx context.Context, table string, key string, values map[string][]byte) error

	// Insert inserts a record in the database. Any field/value pairs will be written into the
	// database.
	// table: The name of the table.
	// key: The record key of the record to insert.
	// values: A map of field/value pairs to insert in the record.
	Insert(ctx context.Context, table string, key string, values map[string][]byte) error

	// Delete deletes a record from the database.
	// table: The name of the table.
	// key: The record key of the record to delete.
	Delete(ctx context.Context, table string, key string) error
}

// BatchInserter is the part of BatchDB a load needs: a DB can implement it
// alone to batch inserts while its reads, updates and deletes stay per record.
type BatchInserter interface {
	// BatchInsert inserts batch records in the database.
	// table: The name of the table.
	// keys: The keys of batch records.
	// values: The values of batch records.
	// The records may succeed or fail independently: return a *BatchError
	// then, so that only the failed ones are counted as failed (and retried).
	BatchInsert(ctx context.Context, table string, keys []string, values []map[string][]byte) error
}

// BatchDB batches every operation. Like BatchInsert, BatchRead, BatchUpdate
// and BatchDelete may return a *BatchError, so that only the failed records
// count as failed.
type BatchDB interface {
	BatchInserter

	// BatchRead reads records from the database.
	// table: The name of the table.
	// keys: The keys of records to read.
	// fields: The list of fields to read, nil|empty for reading all.
	BatchRead(ctx context.Context, table string, keys []string, fields []string) ([]map[string][]byte, error)

	// BatchUpdate updates records in the database.
	// table: The name of table.
	// keys: The keys of records to update.
	// values: The values of records to update.
	BatchUpdate(ctx context.Context, table string, keys []string, values []map[string][]byte) error

	// BatchDelete deletes records from the database.
	// table: The name of the table.
	// keys: The keys of the records to delete.
	BatchDelete(ctx context.Context, table string, keys []string) error
}

// ErrNotRun is (wrapped in) the error of an operation the client didn't hand
// to the DB because the run had stopped: it counts as nothing.
var ErrNotRun = errors.New("not run: the run stopped")

// BatchError is the error of a batch operation whose records failed
// independently: Errs[i] is the error of the batch's i-th record, nil if that
// record succeeded. Any other error from a batch operation means that every
// record of the batch failed.
type BatchError struct {
	Errs []error
}

// NewBatchError returns a *BatchError of the per-record errors errs, or nil if
// every record succeeded.
func NewBatchError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return &BatchError{Errs: errs}
		}
	}
	return nil
}

// Failed returns the number of failed records.
func (e *BatchError) Failed() int {
	n := 0
	for _, err := range e.Errs {
		if err != nil {
			n++
		}
	}
	return n
}

func (e *BatchError) Error() string {
	for _, err := range e.Errs {
		if err != nil {
			return fmt.Sprintf("%d of %d records failed, the first: %v", e.Failed(), len(e.Errs), err)
		}
	}
	return fmt.Sprintf("0 of %d records failed", len(e.Errs))
}

// Unwrap makes errors.Is and errors.As look at every record's error.
func (e *BatchError) Unwrap() []error {
	errs := make([]error, 0, len(e.Errs))
	for _, err := range e.Errs {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// AnalyzeDB is the interface for the DB that can perform an analysis on given table.
type AnalyzeDB interface {
	// Analyze performs a key distribution analysis for the table.
	// table: The name of the table.
	Analyze(ctx context.Context, table string) error
}

var dbCreators = map[string]DBCreator{}

// RegisterDBCreator registers a creator for the database
func RegisterDBCreator(name string, creator DBCreator) {
	_, ok := dbCreators[name]
	if ok {
		panic(fmt.Sprintf("duplicate register database %s", name))
	}

	dbCreators[name] = creator
}

// GetDBCreator gets the DBCreator for the database
func GetDBCreator(name string) DBCreator {
	return dbCreators[name]
}

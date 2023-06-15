package workload

import (
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/generator"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	"sync"
	"testing"
)

func Test_core_nextKeyNum(t *testing.T) {
	type fields struct {
		p                            *properties.Properties
		table                        string
		fieldCount                   int64
		fieldNames                   []string
		fieldLengthGenerator         ycsb.Generator
		readAllFields                bool
		writeAllFields               bool
		dataIntegrity                bool
		keySequence                  ycsb.Generator
		operationChooser             *generator.Discrete
		keyChooser                   ycsb.Generator
		fieldChooser                 ycsb.Generator
		transactionInsertKeySequence *generator.AcknowledgedCounter
		scanLength                   ycsb.Generator
		orderedInserts               bool
		recordCount                  int64
		zeroPadding                  int64
		insertionRetryLimit          int64
		insertionRetryInterval       int64
		valuePool                    sync.Pool
	}
	type args struct {
		state *coreState
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   int64
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &core{
				p:                            tt.fields.p,
				table:                        tt.fields.table,
				fieldCount:                   tt.fields.fieldCount,
				fieldNames:                   tt.fields.fieldNames,
				fieldLengthGenerator:         tt.fields.fieldLengthGenerator,
				readAllFields:                tt.fields.readAllFields,
				writeAllFields:               tt.fields.writeAllFields,
				dataIntegrity:                tt.fields.dataIntegrity,
				keySequence:                  tt.fields.keySequence,
				operationChooser:             tt.fields.operationChooser,
				keyChooser:                   tt.fields.keyChooser,
				fieldChooser:                 tt.fields.fieldChooser,
				transactionInsertKeySequence: tt.fields.transactionInsertKeySequence,
				scanLength:                   tt.fields.scanLength,
				orderedInserts:               tt.fields.orderedInserts,
				recordCount:                  tt.fields.recordCount,
				zeroPadding:                  tt.fields.zeroPadding,
				insertionRetryLimit:          tt.fields.insertionRetryLimit,
				insertionRetryInterval:       tt.fields.insertionRetryInterval,
				valuePool:                    tt.fields.valuePool,
			}
			if got := c.nextKeyNum(tt.args.state); got != tt.want {
				t.Errorf("nextKeyNum() = %v, want %v", got, tt.want)
			}
		})
	}
}

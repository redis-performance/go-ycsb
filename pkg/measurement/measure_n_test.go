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

package measurement

import (
	"io"
	"testing"
	"time"
	"unsafe"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
)

// MeasureN(n) is n samples of one latency: in the cumulative histogram and in
// the interval's, so counts, OPS and percentiles weigh a batch's records.
func TestHistogramsMeasureN(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows = true
	t0 := time.Now()
	h.IntervalStart(t0)
	h.MeasureN("INSERT", t0, 2*time.Millisecond, 99)
	h.Measure("INSERT", t0, 10*time.Millisecond)

	if got := h.histograms["INSERT"].hist.TotalCount(); got != 100 {
		t.Fatalf("cumulative count %d, want 100", got)
	}
	recs := h.cutInterval(t0.Add(time.Second))
	if len(recs) != 1 || recs[0].Count != 100 || recs[0].CumCount != 100 {
		t.Fatalf("interval records %+v, want one INSERT with count 100", recs)
	}
	if p50 := *recs[0].P50Us; p50 < 1990 || p50 > 2010 {
		t.Errorf("p50 %dus, want ~2000 (99 of the 100 samples)", p50)
	}
	if max := *recs[0].MaxUs; max < 9990 || max > 10010 {
		t.Errorf("max %dus, want ~10000", max)
	}
}

type countingMeasurer struct{ calls map[string]int }

func (m *countingMeasurer) Measure(op string, _ time.Time, _ time.Duration) { m.calls[op]++ }
func (m *countingMeasurer) Summary()                                        {}
func (m *countingMeasurer) GenerateExtendedOutputs()                        {}
func (m *countingMeasurer) Output(io.Writer) error                          { return nil }

// A Measurer without MeasureN (e.g. the raw/csv one) gets n Measure calls.
func TestMeasureNFallback(t *testing.T) {
	cm := &countingMeasurer{calls: map[string]int{}}
	m := &measurement{measurer: cm}
	m.measure("INSERT", time.Now(), time.Millisecond, 7)
	m.measure("TOTAL", time.Now(), time.Millisecond, 1)
	if cm.calls["INSERT"] != 7 || cm.calls["TOTAL"] != 1 {
		t.Fatalf("calls %v, want INSERT:7 TOTAL:1", cm.calls)
	}
}

// A load never starts a warm-up, even with warmuptime set: its first records
// must be measured and counted like the rest.
func TestStartsInWarmUp(t *testing.T) {
	for _, c := range []struct {
		warmUpTime, doTransactions string
		want                       bool
	}{{"5", "false", false}, {"5", "true", true}, {"0", "true", false}, {"", "", false}, {"5", "", true}} {
		p := properties.NewProperties()
		if c.warmUpTime != "" {
			p.Set(prop.WarmUpTime, c.warmUpTime)
		}
		if c.doTransactions != "" {
			p.Set(prop.DoTransactions, c.doTransactions)
		}
		if got := startsInWarmUp(p); got != c.want {
			t.Errorf("warmuptime=%q dotransactions=%q: %v, want %v", c.warmUpTime, c.doTransactions, got, c.want)
		}
	}
}

// The measure channel holds 1M events: each byte of measureEvent is 1 MB of
// client memory: it is 40 bytes (48 before batches, with a time.Time start).
func TestMeasureEventSize(t *testing.T) {
	if size := unsafe.Sizeof(measureEvent{}); size > 40 {
		t.Fatalf("measureEvent is %d bytes, want at most 40", size)
	}
}

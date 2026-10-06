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

package measurement

import (
	"sync"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/pingcap/go-ycsb/pkg/util"
)

// histogram keeps the cumulative latencies of one operation and, alongside,
// the latencies of the current reporting interval (window). mu guards both:
// Measure runs on the measurement goroutine while the reporter reads them.
type histogram struct {
	startTime time.Time

	mu   sync.Mutex
	hist *hdrhistogram.Histogram
	// win collects the current interval; spare is the other buffer, swapped in
	// at each interval so recording never waits for the reporter's reads.
	win   *hdrhistogram.Histogram
	spare *hdrhistogram.Histogram
}

// Metric name.
const (
	ELAPSED   = "ELAPSED"
	COUNT     = "COUNT"
	QPS       = "QPS"
	AVG       = "AVG"
	MIN       = "MIN"
	MAX       = "MAX"
	PER50TH   = "PER50TH"
	PER90TH   = "PER90TH"
	PER95TH   = "PER95TH"
	PER99TH   = "PER99TH"
	PER999TH  = "PER999TH"
	PER9999TH = "PER9999TH"
)

func newHDR() *hdrhistogram.Histogram {
	return hdrhistogram.New(1, 24*60*60*1000*1000, 3)
}

// newHistogram makes an operation's histogram; with windows it also keeps the
// per-interval latencies.
func newHistogram(windows bool) *histogram {
	h := new(histogram)
	h.startTime = time.Now()
	h.hist = newHDR()
	if windows {
		h.win = newHDR()
		h.spare = newHDR()
	}
	return h
}

func (h *histogram) Measure(latency time.Duration) {
	h.MeasureN(latency, 1)
}

// MeasureN records n samples of latency.
func (h *histogram) MeasureN(latency time.Duration, n int64) {
	us := latency.Microseconds()
	h.mu.Lock()
	h.hist.RecordValues(us, n)
	if h.win != nil {
		h.win.RecordValues(us, n)
	}
	h.mu.Unlock()
}

// takeWindow returns the interval's latencies and the cumulative count at the
// same instant, and starts a new interval. The returned histogram is owned by
// the caller until it passes it back to returnWindow (one reporter at a time).
func (h *histogram) takeWindow() (*hdrhistogram.Histogram, int64) {
	h.mu.Lock()
	w := h.win
	h.win = h.spare
	h.spare = nil
	cum := h.hist.TotalCount()
	h.mu.Unlock()
	return w, cum
}

// returnWindow makes a read window the spare buffer again.
func (h *histogram) returnWindow(w *hdrhistogram.Histogram) {
	w.Reset()
	h.mu.Lock()
	h.spare = w
	h.mu.Unlock()
}

func (h *histogram) Summary() []string {
	res := h.getInfo()

	return []string{
		util.FloatToOneString(res[ELAPSED]),
		util.IntToString(res[COUNT]),
		util.FloatToOneString(res[QPS]),
		util.IntToString(res[AVG]),
		util.IntToString(res[MIN]),
		util.IntToString(res[MAX]),
		util.IntToString(res[PER50TH]),
		util.IntToString(res[PER90TH]),
		util.IntToString(res[PER95TH]),
		util.IntToString(res[PER99TH]),
		util.IntToString(res[PER999TH]),
		util.IntToString(res[PER9999TH]),
	}
}

func (h *histogram) getInfo() map[string]interface{} {
	h.mu.Lock()
	min := h.hist.Min()
	max := h.hist.Max()
	avg := int64(h.hist.Mean())
	count := h.hist.TotalCount()

	per50 := h.hist.ValueAtPercentile(50)
	per90 := h.hist.ValueAtPercentile(90)
	per95 := h.hist.ValueAtPercentile(95)
	per99 := h.hist.ValueAtPercentile(99)
	per999 := h.hist.ValueAtPercentile(99.9)
	per9999 := h.hist.ValueAtPercentile(99.99)
	h.mu.Unlock()

	elapsed := time.Now().Sub(h.startTime).Seconds()
	qps := float64(count) / elapsed
	res := make(map[string]interface{})
	res[ELAPSED] = elapsed
	res[COUNT] = count
	res[QPS] = qps
	res[AVG] = avg
	res[MIN] = min
	res[MAX] = max
	res[PER50TH] = per50
	res[PER90TH] = per90
	res[PER95TH] = per95
	res[PER99TH] = per99
	res[PER999TH] = per999
	res[PER9999TH] = per9999

	return res
}

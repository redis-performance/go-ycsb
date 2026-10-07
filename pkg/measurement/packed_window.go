package measurement

import (
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
)

const packedWindowSlots = 60

// HDRBucket is one non-cumulative latency bucket in an HDR window snapshot.
type HDRBucket struct {
	ValueUs int64 `json:"value_us"`
	Count   int64 `json:"count"`
}

// HDRWindowRecord is a snapshot of completed reporting intervals. The
// covered duration exposes ticker delays and a partial final interval.
type HDRWindowRecord struct {
	Op             string      `json:"op"`
	WindowSeconds  int         `json:"window_seconds"`
	CoveredSeconds float64     `json:"covered_seconds"`
	CoverageValid  bool        `json:"coverage_valid"`
	End            time.Time   `json:"end"`
	Count          int64       `json:"count"`
	Dropped        int64       `json:"dropped"`
	P50Us          int64       `json:"p50_us"`
	P90Us          int64       `json:"p90_us"`
	P95Us          int64       `json:"p95_us"`
	P99Us          int64       `json:"p99_us"`
	P999Us         int64       `json:"p999_us"`
	Buckets        []HDRBucket `json:"buckets"`
}

type packedSlice struct {
	hist      *hdrhistogram.PackedHistogram
	durationS float64
	dropped   int64
}

// packedRoll is touched only by the interval reporter, which serializes cuts
// with intervals.mu. It keeps one packed histogram per completed interval.
type packedRoll struct {
	slots  [packedWindowSlots]packedSlice
	next   int
	filled int
	agg30  *hdrhistogram.PackedHistogram
	agg60  *hdrhistogram.PackedHistogram
}

func newPackedHDR() *hdrhistogram.PackedHistogram {
	return hdrhistogram.NewPacked(1, 24*60*60*1000*1000, 3)
}

func newPackedRoll() *packedRoll {
	r := &packedRoll{agg30: newPackedHDR(), agg60: newPackedHDR()}
	for i := range r.slots {
		r.slots[i].hist = newPackedHDR()
	}
	return r
}

func (r *packedRoll) push(op string, dense *hdrhistogram.Histogram, durationS float64, end time.Time) [2]HDRWindowRecord {
	slot := &r.slots[r.next]
	slot.hist.Reset()
	// Both histograms have the same geometry, so only count overflow can drop
	// samples. Keep the loss visible rather than failing the benchmark.
	slot.dropped = slot.hist.MergeFrom(dense)
	slot.durationS = durationS
	r.next = (r.next + 1) % len(r.slots)
	if r.filled < len(r.slots) {
		r.filled++
	}
	return [2]HDRWindowRecord{
		r.snapshot(op, 30, r.agg30, end),
		r.snapshot(op, 60, r.agg60, end),
	}
}

func (r *packedRoll) snapshot(op string, window int, agg *hdrhistogram.PackedHistogram, end time.Time) HDRWindowRecord {
	agg.Reset()
	record := HDRWindowRecord{Op: op, WindowSeconds: window, End: end.UTC()}
	limit := r.filled
	if limit > window {
		limit = window
	}
	record.CoverageValid = limit == window
	for i := 0; i < limit; i++ {
		slot := &r.slots[(r.next-1-i+len(r.slots))%len(r.slots)]
		record.Dropped += slot.dropped + agg.Merge(slot.hist)
		record.CoveredSeconds += slot.durationS
		if slot.durationS < 0.5 || slot.durationS > 1.5 {
			record.CoverageValid = false
		}
	}
	if record.CoveredSeconds < float64(window)-0.5 || record.CoveredSeconds > float64(window)+0.5 {
		record.CoverageValid = false
	}
	if record.Dropped != 0 {
		record.CoverageValid = false
	}
	record.Count = agg.TotalCount()
	if record.Count == 0 {
		return record
	}
	record.P50Us = agg.ValueAtPercentile(50)
	record.P90Us = agg.ValueAtPercentile(90)
	record.P95Us = agg.ValueAtPercentile(95)
	record.P99Us = agg.ValueAtPercentile(99)
	record.P999Us = agg.ValueAtPercentile(99.9)
	return record
}

// hdrWithBuckets copies cached records and attaches their full HDR distribution
// only when a JSON consumer requests it. Call with intervals.mu held so the
// reporter cannot reset the aggregate while it is read.
func (h *histograms) hdrWithBuckets(records []HDRWindowRecord) []HDRWindowRecord {
	out := make([]HDRWindowRecord, len(records))
	copy(out, records)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := range out {
		op := h.histograms[out[i].Op]
		if op == nil || op.roll == nil {
			continue
		}
		agg := op.roll.agg30
		if out[i].WindowSeconds == 60 {
			agg = op.roll.agg60
		}
		agg.ForEachBucket(func(value, count int64) bool {
			out[i].Buckets = append(out[i].Buckets, HDRBucket{ValueUs: value, Count: count})
			return true
		})
	}
	return out
}

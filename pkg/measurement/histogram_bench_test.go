package measurement

import (
	"testing"
	"time"
)

// Measure without an interval output file (the cumulative histogram only).
func BenchmarkHistogramMeasure(b *testing.B) {
	h := newHistogram(false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Measure(time.Duration(i%10000) * time.Microsecond)
	}
}

// Measure with an interval output file (cumulative and window histograms).
func BenchmarkHistogramMeasureWindows(b *testing.B) {
	h := newHistogram(true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Measure(time.Duration(i%10000) * time.Microsecond)
	}
}

// The measurement goroutine's whole per-sample path, without an interval file.
func BenchmarkHistogramsMeasure(b *testing.B) {
	h := InitHistograms(nil)
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Measure("READ", now, time.Duration(i%10000)*time.Microsecond)
	}
}

// The same with an interval output file (windows and the cut lock).
func BenchmarkHistogramsMeasureWindows(b *testing.B) {
	h := InitHistograms(nil)
	h.windows = true
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Measure("READ", now, time.Duration(i%10000)*time.Microsecond)
	}
}

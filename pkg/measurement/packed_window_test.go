package measurement

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
)

func TestPackedWindowRollsThirtyAndSixtySlices(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows, h.packedWindows = true, true
	start := time.Unix(1000, 0)
	h.startIntervals(start)
	for i := 1; i <= 60; i++ {
		h.Measure("READ", start, time.Millisecond)
		h.writeInterval(start.Add(time.Duration(i) * time.Second))
	}
	snapshot := h.iv.latest.Load()
	if len(snapshot.hdr) != 2 {
		t.Fatalf("HDR windows = %d, want 2", len(snapshot.hdr))
	}
	h.iv.mu.Lock()
	full := h.hdrWithBuckets(snapshot.hdr)
	h.iv.mu.Unlock()
	for _, r := range full {
		if !r.CoverageValid || r.Count != int64(r.WindowSeconds) || r.CoveredSeconds != float64(r.WindowSeconds) ||
			r.P99Us != 1000 || len(r.Buckets) != 1 || r.Buckets[0].Count != r.Count {
			t.Errorf("unexpected %ds window: %+v", r.WindowSeconds, r)
		}
	}
	h.Measure("READ", start, 2*time.Millisecond)
	h.writeInterval(start.Add(61 * time.Second))
	snapshot = h.iv.latest.Load()
	h.iv.mu.Lock()
	full = h.hdrWithBuckets(snapshot.hdr)
	h.iv.mu.Unlock()
	for _, r := range full {
		if r.Count != int64(r.WindowSeconds) || len(r.Buckets) != 2 {
			t.Errorf("rolling %ds window = %+v", r.WindowSeconds, r)
		}
	}
	for i := 62; i <= 121; i++ {
		h.writeInterval(start.Add(time.Duration(i) * time.Second))
	}
	for _, r := range h.iv.latest.Load().hdr {
		if r.Count != 0 || len(r.Buckets) != 0 {
			t.Errorf("idle %ds window retained samples: %+v", r.WindowSeconds, r)
		}
	}
}

func TestPackedWindowReportsDelayedIntervalCoverage(t *testing.T) {
	h := InitHistograms(properties.NewProperties())
	h.windows, h.packedWindows = true, true
	start := time.Unix(1500, 0)
	h.startIntervals(start)
	for i := 1; i <= 29; i++ {
		h.Measure("READ", start, time.Millisecond)
		h.writeInterval(start.Add(time.Duration(i) * time.Second))
	}
	h.Measure("READ", start, 2*time.Millisecond)
	h.writeInterval(start.Add(31 * time.Second)) // the reporter missed one tick
	r := h.iv.latest.Load().hdr[0]
	if r.WindowSeconds != 30 || r.CoverageValid || r.CoveredSeconds != 31 || r.Count != 30 || r.P99Us != 2000 {
		t.Errorf("late reporter snapshot = %+v", r)
	}
}

func TestPackedMinuteFileAndHTTPEndpoint(t *testing.T) {
	p := properties.NewProperties()
	path := filepath.Join(t.TempDir(), "minutes.jsonl")
	p.Set(prop.MeasurementHDRMinuteOutputFile, path)
	h := InitHistograms(p)
	h.windows, h.packedWindows = true, true
	if err := h.openHDRMinutes(path); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(2000, 0)
	h.startIntervals(start)
	for i := 1; i <= 120; i++ {
		h.MeasureN("READ", start, 2*time.Millisecond, 2)
		h.writeInterval(start.Add(time.Duration(i) * time.Second))
	}
	if err := h.closeHDRMinutes(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("minute file has %d lines, want 2", len(lines))
	}
	for _, line := range lines {
		var minute HDRWindowRecord
		if err := json.Unmarshal([]byte(line), &minute); err != nil {
			t.Fatal(err)
		}
		if minute.Count != 120 || minute.WindowSeconds != 60 || minute.CoveredSeconds != 60 || len(minute.Buckets) != 1 || !minute.CoverageValid {
			t.Errorf("minute snapshot = %+v", minute)
		}
	}

	e := &promExporter{collector: newPromCollector(h, p)}
	handler, err := e.handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/hdr-windows", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("HDR endpoint: HTTP %d: %s", response.Code, response.Body)
	}
	var latest []HDRWindowRecord
	if err := json.Unmarshal(response.Body.Bytes(), &latest); err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 || latest[1].Count != 120 {
		t.Errorf("latest HDR windows = %+v", latest)
	}
	metricResponse := httptest.NewRecorder()
	handler.ServeHTTP(metricResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if got := metricValue(t, metricResponse.Body.String(), "ycsb_hdr_window_operations{op=\"READ\",window=\"60s\""); got != 120 {
		t.Errorf("60s operations = %v, want 120", got)
	}
}

func TestPackedMinuteFileReportsInvalidCoverageAndFinalPartialWindow(t *testing.T) {
	p := properties.NewProperties()
	path := filepath.Join(t.TempDir(), "minutes.jsonl")
	p.Set(prop.MeasurementHDRMinuteOutputFile, path)
	h := InitHistograms(p)
	h.windows, h.packedWindows = true, true
	if err := h.openHDRMinutes(path); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(3000, 0)
	h.IntervalStart(start)
	for i := 1; i <= 59; i++ {
		h.Measure("READ", start, time.Millisecond)
		h.IntervalTick(start.Add(time.Duration(i) * time.Second))
	}
	h.Measure("READ", start, time.Millisecond)
	h.IntervalTick(start.Add(61 * time.Second))
	h.Measure("READ", start, time.Millisecond)
	if err := h.IntervalClose(start.Add(61500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if got := h.iv.latest.Load().windowS; got != 0.5 {
		t.Errorf("final partial interval = %v, want 0.5", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("minute lines = %d, want 1", len(lines))
	}
	var minute HDRWindowRecord
	if err := json.Unmarshal([]byte(lines[0]), &minute); err != nil {
		t.Fatal(err)
	}
	if minute.CoverageValid || minute.CoveredSeconds != 61 || minute.Count != 60 {
		t.Errorf("drifted minute snapshot = %+v", minute)
	}
}

func TestPackedMinuteFileFailureIsReturned(t *testing.T) {
	p := properties.NewProperties()
	path := filepath.Join(t.TempDir(), "minutes.jsonl")
	p.Set(prop.MeasurementHDRMinuteOutputFile, path)
	h := InitHistograms(p)
	h.windows, h.packedWindows = true, true
	if err := h.openHDRMinutes(path); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(4000, 0)
	h.IntervalStart(start)
	h.Measure("READ", start, time.Millisecond)
	if err := h.iv.hdrMinuteFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.IntervalClose(start.Add(time.Minute)); err == nil {
		t.Fatal("closed minute file did not fail the run")
	}
}

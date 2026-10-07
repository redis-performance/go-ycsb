// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package measurement

import (
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/prometheus/client_golang/prometheus"
)

func TestParsePromConfig(t *testing.T) {
	for _, tc := range []struct {
		name, labels, linger string
		wantErr              bool
	}{
		{"valid", "phase=load,custom_label=a=b", "250ms", false},
		{"empty entry", "phase=load,", "", true},
		{"missing equals", "phase", "", true},
		{"empty value", "phase=", "", true},
		{"invalid name", "1phase=load", "", true},
		{"internal name", "__name__=load", "", true},
		{"duplicate", "phase=load,phase=run", "", true},
		{"reserved", "op=READ", "", true},
		{"histogram label", "le=0.001", "", true},
		{"bad UTF8", "phase=\xff", "", true},
		{"negative linger", "phase=run", "-1s", true},
		{"bad linger", "phase=run", "bad", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := properties.NewProperties()
			p.Set(prop.MeasurementPrometheusLabels, tc.labels)
			if tc.linger != "" {
				p.Set(prop.MeasurementPrometheusLinger, tc.linger)
			}
			cfg, err := parsePromConfig(p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parsePromConfig error = %v, want error %t", err, tc.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "measurement.prometheus_") {
					t.Errorf("error does not name the property: %v", err)
				}
				return
			}
			if tc.name == "valid" && (cfg.labels["custom_label"] != "a=b" || cfg.linger != 250*time.Millisecond) {
				t.Errorf("config = %+v", cfg)
			}
		})
	}
}

func metricValue(t *testing.T, body, prefix string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			parts := strings.Fields(line)
			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatalf("metric %q absent from:\n%s", prefix, body)
	return 0
}

func histogramValue(t *testing.T, body, name, op, le string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") || !strings.Contains(line, "op=\""+op+"\"") ||
			(le != "" && !strings.Contains(line, "le=\""+le+"\"")) {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Fatalf("histogram %s op=%s le=%s absent from:\n%s", name, op, le, body)
	return 0
}

func TestPrometheusScrapeUsesCompletedWindowAndFinalCounts(t *testing.T) {
	p := properties.NewProperties()
	p.Set(prop.Workload, "core")
	p.Set(prop.Command, "load")
	p.Set(prop.ThreadCount, "4")
	h := InitHistograms(p)
	h.windows = true
	h.prometheus = true
	start := time.Unix(1000, 0)
	h.startIntervals(start)
	h.MeasureN("READ", start, time.Millisecond, 2)
	h.MeasureN("READ_ERROR", start, 3*time.Millisecond, 1)
	h.MeasureN("INSERT_ERROR", start, 4*time.Millisecond, 2)
	h.MeasureN("BATCH_INSERT", start, 5*time.Millisecond, 1)
	h.MeasureN("TOTAL", start, time.Millisecond, 2)
	h.writeInterval(start.Add(time.Second))

	c := newPromCollector(h, p)
	c.phaseRunning.Store(1)
	e := &promExporter{collector: c}
	handler, err := e.handler(prometheus.Labels{"phase": "load"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		return r
	}
	if r := get("/debug/pprof/"); r.Code != http.StatusNotFound {
		t.Fatalf("private mux serves pprof: HTTP %d", r.Code)
	}
	r := get("/metrics")
	if r.Code != http.StatusOK {
		t.Fatalf("scrape: HTTP %d: %s", r.Code, r.Body)
	}
	body := r.Body.String()
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") && !strings.Contains(line, "phase=\"load\"") {
			t.Errorf("metric without the constant phase label: %s", line)
		}
	}
	for _, expected := range []string{"phase=\"load\"", "ycsb_info{", "command=\"load\"", "version=\"", "ycsb_phase_running{"} {
		if !strings.Contains(body, expected) {
			t.Errorf("scrape missing %q", expected)
		}
	}
	for prefix, want := range map[string]float64{
		"ycsb_operations_total{op=\"READ\"":         2,
		"ycsb_operations_total{op=\"INSERT\"":       0,
		"ycsb_operations_total{op=\"BATCH_INSERT\"": 1,
		"ycsb_operations_total{op=\"TOTAL\"":        2,
		"ycsb_errors_total{op=\"READ\"":             1,
		"ycsb_errors_total{op=\"INSERT\"":           2,
		"ycsb_interval_operations{op=\"READ\"":      2,
		"ycsb_interval_window_seconds{":             1,
		"ycsb_interval_end_timestamp_seconds{":      1001,
		"ycsb_phase_running{":                       1,
	} {
		if got := metricValue(t, body, prefix); got != want {
			t.Errorf("%s = %v, want %v", prefix, got, want)
		}
	}
	if strings.Contains(body, "ycsb_errors_total{op=\"TOTAL\"") || strings.Contains(body, "ycsb_errors_total{op=\"BATCH_INSERT\"") {
		t.Error("TOTAL and BATCH_INSERT must not have error series")
	}
	if got := metricValue(t, body, "ycsb_interval_latency_avg_seconds{op=\"READ\""); got < 0.0009 || got > 0.0011 {
		t.Errorf("READ interval average = %v seconds", got)
	}
	if got := metricValue(t, body, "ycsb_interval_latency_seconds{op=\"READ\",phase=\"load\",quantile=\"0.99\""); got < 0.0009 || got > 0.0011 {
		t.Errorf("READ p99 = %v seconds", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_count", "READ", ""); got != 2 {
		t.Errorf("READ histogram count = %v, want 2", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_bucket", "READ", "0.001"); got != 2 {
		t.Errorf("READ <=1ms bucket = %v, want 2", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_sum", "READ", ""); math.Abs(got-0.002) > 1e-12 {
		t.Errorf("READ histogram sum = %v, want 0.002", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_count", "READ_ERROR", ""); got != 1 {
		t.Errorf("READ_ERROR histogram count = %v, want 1", got)
	}
	c.phaseRunning.Store(0)
	if got := metricValue(t, get("/metrics").Body.String(), "ycsb_phase_running{"); got != 0 {
		t.Errorf("phase running after output = %v", got)
	}
	h.MeasureN("UPDATE", start, 2*time.Millisecond, 1)
	h.writeInterval(start.Add(2 * time.Second))
	body = get("/metrics").Body.String()
	if strings.Contains(body, "ycsb_interval_operations{op=\"READ\",") {
		t.Error("old READ window remains after a new completed window")
	}
	if got := metricValue(t, body, "ycsb_interval_operations{op=\"UPDATE\""); got != 1 {
		t.Errorf("UPDATE in second window = %v", got)
	}
	if got := metricValue(t, body, "ycsb_operations_total{op=\"READ\""); got != 2 {
		t.Errorf("cumulative READ after second window = %v", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_count", "READ", ""); got != 2 {
		t.Errorf("READ histogram count after second window = %v, want 2", got)
	}
	h.writeInterval(start.Add(3 * time.Second)) // an idle interval still has a time marker
	body = get("/metrics").Body.String()
	if got := metricValue(t, body, "ycsb_interval_end_timestamp_seconds{"); got != 1003 {
		t.Errorf("idle interval end = %v", got)
	}
	if strings.Contains(body, "ycsb_interval_operations{") || strings.Contains(body, "ycsb_interval_latency_seconds{") {
		t.Error("idle interval retained an operation series from an earlier window")
	}
	h.MeasureN("READ", start, 4*time.Millisecond, 1)
	if err := h.IntervalClose(start.Add(3500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	body = get("/metrics").Body.String()
	if got := metricValue(t, body, "ycsb_interval_window_seconds{"); got != 0.5 {
		t.Errorf("final partial window = %v", got)
	}
	if got := metricValue(t, body, "ycsb_interval_operations{op=\"READ\""); got != 1 {
		t.Errorf("READ in final partial window = %v", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_count", "READ", ""); got != 3 {
		t.Errorf("READ histogram final count = %v, want 3", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_bucket", "READ", "0.001"); got != 2 {
		t.Errorf("READ <=1ms bucket after final window = %v, want 2", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_bucket", "READ", "0.004"); got != 3 {
		t.Errorf("READ <=4ms bucket = %v, want 3", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_bucket", "READ", "+Inf"); got != 3 {
		t.Errorf("READ +Inf bucket = %v, want 3", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_sum", "READ", ""); math.Abs(got-0.006) > 1e-12 {
		t.Errorf("READ histogram final sum = %v, want 0.006", got)
	}
}

func TestPrometheusHistogramBoundariesAndOverflow(t *testing.T) {
	p := properties.NewProperties()
	h := InitHistograms(p)
	h.prometheus = true
	h.MeasureN("READ", time.Now(), time.Millisecond, 2)
	h.MeasureN("READ", time.Now(), time.Millisecond+time.Nanosecond, 1)
	h.MeasureN("READ", time.Now(), 70*time.Second, 1)
	h.MeasureN("READ", time.Now(), 48*time.Hour, 1) // beyond HDR's range
	e := &promExporter{collector: newPromCollector(h, p)}
	handler, err := e.handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if r.Code != http.StatusOK {
		t.Fatalf("scrape: HTTP %d: %s", r.Code, r.Body)
	}
	body := r.Body.String()
	for le, want := range map[string]float64{"0.00075": 0, "0.001": 2, "0.00125": 3, "60": 3, "+Inf": 4} {
		if got := histogramValue(t, body, "ycsb_latency_seconds_bucket", "READ", le); got != want {
			t.Errorf("READ le=%s bucket = %v, want %v", le, got, want)
		}
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_count", "READ", ""); got != 4 {
		t.Errorf("READ count = %v, want 4", got)
	}
	if got := histogramValue(t, body, "ycsb_latency_seconds_sum", "READ", ""); math.Abs(got-70.003000001) > 1e-9 {
		t.Errorf("READ sum = %v, want 70.003000001", got)
	}
}

func TestPrometheusIdleIntervalWithoutAnyOperation(t *testing.T) {
	p := properties.NewProperties()
	h := InitHistograms(p)
	h.windows = true
	start := time.Unix(2000, 0)
	h.startIntervals(start)
	h.writeInterval(start.Add(time.Second))
	e := &promExporter{collector: newPromCollector(h, p)}
	handler, err := e.handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if got := metricValue(t, r.Body.String(), "ycsb_interval_end_timestamp_seconds "); got != 2001 {
		t.Errorf("idle interval end = %v", got)
	}
}

func TestPrometheusInfoUsesActualDefaultThreadCount(t *testing.T) {
	c := newPromCollector(InitHistograms(properties.NewProperties()), properties.NewProperties())
	if got := c.infoVals[2]; got != "1" {
		t.Errorf("default threadcount = %q, want Client.Run's default 1", got)
	}
}

func TestPrometheusListenAndClose(t *testing.T) {
	p := properties.NewProperties()
	h := InitHistograms(p)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if err := startPrometheus(promConfig{listen: busy.Addr().String()}, h, p); err == nil || !strings.Contains(err.Error(), prop.MeasurementPrometheusListen) {
		t.Fatalf("busy listen error = %v", err)
	}
	if err := startPrometheus(promConfig{listen: "127.0.0.1:0", linger: 0}, h, p); err != nil {
		t.Fatal(err)
	}
	e := activePrometheus.Load()
	if e == nil {
		t.Fatal("exporter did not start")
	}
	r, err := http.Get("http://" + e.listener.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("scrape: HTTP %d", r.StatusCode)
	}
	ClosePrometheus()
	if activePrometheus.Load() != nil {
		t.Error("exporter remains active after close")
	}
}

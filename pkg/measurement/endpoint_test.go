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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/prometheus/client_golang/prometheus"
)

func TestParsePromConfigEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, listen, enabled, labels string
		wantErr                       bool
	}{
		{"enabled", "127.0.0.1:9464", "true", "phase=load", false},
		{"off", "", "false", "", false},
		{"bad bool", "127.0.0.1:9464", "sure", "", true},
		{"no exporter", "", "true", "", true},
		{"endpoint label", "127.0.0.1:9464", "true", "endpoint=x", true},
		{"shard label", "127.0.0.1:9464", "true", "shard=x", true},
		{"endpoint label, option off", "127.0.0.1:9464", "false", "endpoint=x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := properties.NewProperties()
			p.Set(prop.MeasurementPrometheusListen, tc.listen)
			p.Set(prop.MeasurementPrometheusEndpoints, tc.enabled)
			p.Set(prop.MeasurementPrometheusLabels, tc.labels)
			cfg, err := parsePromConfig(p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parsePromConfig() = %+v, %v; wantErr %t", cfg, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), prop.MeasurementPrometheusEndpoints) {
				t.Errorf("error does not name the property: %v", err)
			}
			if err == nil && cfg.endpoints != (tc.enabled == "true") {
				t.Errorf("endpoints = %t", cfg.endpoints)
			}
		})
	}
}

// The endpoint series: a histogram per endpoint and operation outcome, error
// and redirect counters per operation (zero from the first request), and the
// endpoints' identities. Nothing is recorded without an operation, during a
// warm-up, or with the option off.
func TestEndpointSeries(t *testing.T) {
	endpoints.Store(newEndpointStats())
	t.Cleanup(func() { endpoints.Store(nil) })

	if _, ok := EndpointOpName(context.Background()); ok {
		t.Error("an operation on a bare context")
	}
	if op, _ := EndpointOpName(WithEndpointOp(context.Background(), NewEndpointOp("READ"))); op != "READ" {
		t.Errorf("EndpointOp = %q", op)
	}
	MeasureEndpoint(context.Background(), "10.0.0.1:6379", "READ", EndpointOK, time.Millisecond)
	MeasureEndpoint(context.Background(), "10.0.0.1:6379", "READ", EndpointOK, 70*time.Second) // past the last bound
	MeasureEndpoint(context.Background(), "10.0.0.1:6379", "READ", EndpointError, 2*time.Millisecond)
	MeasureEndpoint(context.Background(), "10.0.0.2:6379", "INSERT", EndpointRedirect, 3*time.Millisecond)
	EnableWarmUp(true)
	MeasureEndpoint(context.Background(), "10.0.0.1:6379", "READ", EndpointOK, time.Millisecond)
	// a batch the worker started after the warm-up is measured whole
	MeasureEndpoint(WithMeasured(context.Background(), true), "10.0.0.1:6379", "READ", EndpointOK, time.Millisecond)
	EnableWarmUp(false)
	// and one it started during the warm-up not at all
	MeasureEndpoint(WithMeasured(context.Background(), false), "10.0.0.1:6379", "READ", EndpointOK, time.Millisecond)
	MeasureEndpoint(context.Background(), "10.0.0.2:6379", "INSERT", EndpointCanceled, time.Millisecond)
	SetEndpointInfo([]EndpointInfo{
		{Endpoint: "10.0.0.1:6379", NodeID: "a", Role: "master", Shard: "a"},
		{Endpoint: "10.0.0.1:6379", NodeID: "a", Role: "master", Shard: "a"}, // listed twice: one series
	})

	c := newPromCollector(InitHistograms(properties.NewProperties()), properties.NewProperties())
	handler, err := (&promExporter{collector: c}).handler(prometheus.Labels{"phase": "run"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: HTTP %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for series, want := range map[string]float64{
		`ycsb_endpoint_latency_seconds_count{endpoint="10.0.0.1:6379",op="READ",phase="run"}`:             3,
		`ycsb_endpoint_latency_seconds_bucket{endpoint="10.0.0.1:6379",op="READ",phase="run",le="0.001"}`: 2,
		`ycsb_endpoint_latency_seconds_bucket{endpoint="10.0.0.1:6379",op="READ",phase="run",le="60"}`:    2,
		`ycsb_endpoint_latency_seconds_bucket{endpoint="10.0.0.1:6379",op="READ",phase="run",le="+Inf"}`:  3,
		`ycsb_endpoint_latency_seconds_sum{endpoint="10.0.0.1:6379",op="READ",phase="run"}`:               70.002,
		`ycsb_endpoint_latency_seconds_count{endpoint="10.0.0.1:6379",op="READ_ERROR",phase="run"}`:       1,
		`ycsb_endpoint_errors_total{endpoint="10.0.0.1:6379",op="READ",phase="run"}`:                      1,
		`ycsb_endpoint_redirects_total{endpoint="10.0.0.1:6379",op="READ",phase="run"}`:                   0,
		`ycsb_endpoint_redirects_total{endpoint="10.0.0.2:6379",op="INSERT",phase="run"}`:                 1,
		`ycsb_endpoint_info{endpoint="10.0.0.1:6379",node_id="a",phase="run",role="master",shard="a"}`:    1,
	} {
		if got := metricValue(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if got := metricValue(t, body, `ycsb_endpoint_info_refreshed_timestamp_seconds{phase="run"}`); got < float64(time.Now().Add(-time.Minute).Unix()) {
		t.Errorf("info refreshed at %v, want just now", got)
	}
	for series, want := range map[string]float64{
		// both counters exist from an endpoint's first request of the operation
		`ycsb_endpoint_errors_total{endpoint="10.0.0.2:6379",op="INSERT",phase="run"}`:                   0,
		`ycsb_endpoint_latency_seconds_count{endpoint="10.0.0.2:6379",op="INSERT_CANCELED",phase="run"}`: 1,
	} {
		if got := metricValue(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	// an address that isn't UTF-8 is repaired, not a failed scrape
	SetEndpointInfo([]EndpointInfo{{Endpoint: "h\xff:6379", NodeID: "a", Role: "master", Shard: "a"}})
	MeasureEndpoint(context.Background(), "h\xff:6379", "READ", EndpointOK, time.Millisecond)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ycsb_endpoint_info{endpoint=\"h\uFFFD:6379\"") {
		t.Errorf("invalid UTF-8: HTTP %d, info repaired %t", rec.Code, strings.Contains(rec.Body.String(), "h\uFFFD:6379"))
	}

	endpoints.Store(nil)
	MeasureEndpoint(context.Background(), "10.0.0.1:6379", "READ", EndpointOK, time.Millisecond) // a no-op
	if EndpointsEnabled() {
		t.Error("enabled after reset")
	}
}

// Past maxEndpoints, requests are recorded under otherEndpoint and each new
// endpoint counted once in the overflow.
func TestEndpointCap(t *testing.T) {
	s := newEndpointStats()
	endpoints.Store(s)
	t.Cleanup(func() { endpoints.Store(nil) })
	ctx := context.Background()
	for i := 0; i < maxEndpoints+5; i++ {
		ep := fmt.Sprintf("10.0.%d.%d:6379", i/256, i%256)
		MeasureEndpoint(ctx, ep, "READ", EndpointOK, time.Millisecond)
		MeasureEndpoint(ctx, ep, "READ", EndpointOK, time.Millisecond) // a known one again
	}
	samples, _, _, overflow := s.snapshot()
	if len(samples) != maxEndpoints+1 || overflow != 5 {
		t.Fatalf("%d series, overflow %d; want %d series and 5", len(samples), overflow, maxEndpoints+1)
	}
	for _, sample := range samples {
		if sample.endpoint == otherEndpoint && sample.count != 10 {
			t.Errorf("other: %d requests, want 10", sample.count)
		}
	}
}

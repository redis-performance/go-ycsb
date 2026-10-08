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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Version is set by the Makefile; direct go builds use their VCS build info.
var Version string

type promConfig struct {
	listen        string
	labels        prometheus.Labels
	linger        time.Duration
	hdrWindows    bool
	hdrMinuteFile string
	endpoints     bool
}

var promLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func parsePromLabels(value string) (prometheus.Labels, error) {
	labels := prometheus.Labels{}
	if value == "" {
		return labels, nil
	}
	reserved := map[string]bool{
		"op": true, "le": true, "quantile": true, "workload": true, "command": true,
		"threadcount": true, "batch_size": true, "target": true, "version": true,
	}
	for _, entry := range strings.Split(value, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(entry), "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok || !promLabelName.MatchString(key) || strings.HasPrefix(key, "__") || reserved[key] ||
			val == "" || !utf8.ValidString(val) {
			return nil, fmt.Errorf("%s=%q: invalid label %q", prop.MeasurementPrometheusLabels, value, entry)
		}
		if _, exists := labels[key]; exists {
			return nil, fmt.Errorf("%s=%q: duplicate label %q", prop.MeasurementPrometheusLabels, value, key)
		}
		labels[key] = val
	}
	return labels, nil
}

func parsePromConfig(p *properties.Properties) (promConfig, error) {
	var cfg promConfig
	cfg.listen = strings.TrimSpace(p.GetString(prop.MeasurementPrometheusListen, ""))
	labels, err := parsePromLabels(p.GetString(prop.MeasurementPrometheusLabels, ""))
	if err != nil {
		return cfg, err
	}
	cfg.labels = labels
	cfg.linger = time.Second
	if value, ok := p.Get(prop.MeasurementPrometheusLinger); ok {
		cfg.linger, err = time.ParseDuration(strings.TrimSpace(value))
		if err != nil || cfg.linger < 0 {
			return cfg, fmt.Errorf("%s=%q: want a non-negative Go duration", prop.MeasurementPrometheusLinger, value)
		}
	}
	if value, ok := p.Get(prop.MeasurementPrometheusHDRWindows); ok {
		cfg.hdrWindows, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return cfg, fmt.Errorf("%s=%q: want true or false", prop.MeasurementPrometheusHDRWindows, value)
		}
	}
	if cfg.hdrWindows && cfg.listen == "" {
		return cfg, fmt.Errorf("%s needs %s", prop.MeasurementPrometheusHDRWindows, prop.MeasurementPrometheusListen)
	}
	if cfg.hdrWindows {
		if _, exists := cfg.labels["window"]; exists {
			return cfg, fmt.Errorf("%s: window is reserved when %s=true", prop.MeasurementPrometheusLabels, prop.MeasurementPrometheusHDRWindows)
		}
	}
	if value, ok := p.Get(prop.MeasurementPrometheusEndpoints); ok {
		cfg.endpoints, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return cfg, fmt.Errorf("%s=%q: want true or false", prop.MeasurementPrometheusEndpoints, value)
		}
	}
	if cfg.endpoints {
		if cfg.listen == "" {
			return cfg, fmt.Errorf("%s needs %s", prop.MeasurementPrometheusEndpoints, prop.MeasurementPrometheusListen)
		}
		for _, name := range []string{"endpoint", "node_id", "role", "shard"} {
			if _, exists := cfg.labels[name]; exists {
				return cfg, fmt.Errorf("%s: %s is reserved when %s=true", prop.MeasurementPrometheusLabels, name, prop.MeasurementPrometheusEndpoints)
			}
		}
	}
	cfg.hdrMinuteFile = strings.TrimSpace(p.GetString(prop.MeasurementHDRMinuteOutputFile, ""))
	if cfg.hdrMinuteFile != "" && !cfg.hdrWindows {
		return cfg, fmt.Errorf("%s needs %s=true", prop.MeasurementHDRMinuteOutputFile, prop.MeasurementPrometheusHDRWindows)
	}
	return cfg, nil
}

func buildVersion() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				version := setting.Value
				for _, dirty := range info.Settings {
					if dirty.Key == "vcs.modified" && dirty.Value == "true" {
						version += "-dirty"
					}
				}
				return version
			}
		}
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return "unknown"
}

type promCollector struct {
	h            *histograms
	info         *prometheus.Desc
	running      *prometheus.Desc
	target       *prometheus.Desc
	planned      *prometheus.Desc
	queueDepth   *prometheus.Desc
	queueCap     *prometheus.Desc
	ops          *prometheus.Desc
	errors       *prometheus.Desc
	latencyHist  *prometheus.Desc
	latency      *prometheus.Desc
	avg          *prometheus.Desc
	max          *prometheus.Desc
	count        *prometheus.Desc
	window       *prometheus.Desc
	end          *prometheus.Desc
	hdrCount     *prometheus.Desc
	hdrDropped   *prometheus.Desc
	hdrLatency   *prometheus.Desc
	hdrCoverage  *prometheus.Desc
	hdrValid     *prometheus.Desc
	hdrEnd       *prometheus.Desc
	epLatency    *prometheus.Desc
	epErrors     *prometheus.Desc
	epRedirects  *prometheus.Desc
	epInfo       *prometheus.Desc
	infoVals     []string
	targetValue  float64
	plannedValue float64
	queue        <-chan measureEvent
	phaseRunning atomic.Int64
}

func newPromCollector(h *histograms, p *properties.Properties) *promCollector {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	c := &promCollector{
		h:           h,
		info:        desc("ycsb_info", "Configuration of this YCSB process.", "workload", "command", "threadcount", "batch_size", "target", "version"),
		running:     desc("ycsb_phase_running", "One while the load or run phase is active; zero after final counts are available."),
		target:      desc("ycsb_target_operations_per_second", "Configured operation target per second; zero means unlimited."),
		queueDepth:  desc("ycsb_measurement_queue_depth", "Measurement events waiting in the process queue at scrape time."),
		queueCap:    desc("ycsb_measurement_queue_capacity", "Maximum number of measurement events that can wait in the process queue."),
		targetValue: float64(p.GetInt64(prop.Target, 0)),
		queue:       measureChan,
		ops:         desc("ycsb_operations_total", "Cumulative successful operations or sent batches, as counted in the final summary.", "op"),
		errors:      desc("ycsb_errors_total", "Cumulative failed operations, as counted in the final summary.", "op"),
		latencyHist: desc("ycsb_latency_seconds", "Cumulative latency distribution for this operation, in seconds.", "op"),
		latency:     desc("ycsb_interval_latency_seconds", "Latency quantile in the last completed interval, in seconds.", "op", "quantile"),
		avg:         desc("ycsb_interval_latency_avg_seconds", "Mean latency in the last completed interval, in seconds.", "op"),
		max:         desc("ycsb_interval_latency_max_seconds", "Maximum latency in the last completed interval, in seconds.", "op"),
		count:       desc("ycsb_interval_operations", "Operations in the last completed interval.", "op"),
		window:      desc("ycsb_interval_window_seconds", "Length of the last completed interval, in seconds."),
		end:         desc("ycsb_interval_end_timestamp_seconds", "End of the last completed interval, Unix seconds."),
		infoVals: []string{
			p.GetString(prop.Workload, "core"), p.GetString(prop.Command, ""),
			p.GetString(prop.ThreadCount, "1"),
			p.GetString(prop.BatchSize, strconv.Itoa(prop.DefaultBatchSize)),
			p.GetString(prop.Target, "0"), buildVersion(),
		},
	}
	if p.GetString(prop.Command, "") == "load" {
		c.planned = desc("ycsb_planned_inserts", "Configured insert count for this load phase; zero when no count is configured.")
		c.plannedValue = float64(p.GetInt64(prop.InsertCount, p.GetInt64(prop.RecordCount, 0)))
	}
	if c.targetValue < 0 {
		c.targetValue = 0
	}
	if h.packedWindows {
		c.hdrCount = desc("ycsb_hdr_window_operations", "Operations in the last completed HDR window.", "op", "window")
		c.hdrDropped = desc("ycsb_hdr_window_dropped_operations", "Operations omitted from the HDR window due to count overflow.", "op", "window")
		c.hdrLatency = desc("ycsb_hdr_window_latency_seconds", "HDR latency quantile in the last completed window, in seconds.", "op", "window", "quantile")
		c.hdrCoverage = desc("ycsb_hdr_window_covered_seconds", "Actual duration covered by the completed HDR slices.", "op", "window")
		c.hdrValid = desc("ycsb_hdr_window_coverage_valid", "One when all expected HDR slices are present, each lasts 0.5s to 1.5s, total duration is within 0.5s of the target, and no samples were dropped; zero otherwise.", "op", "window")
		c.hdrEnd = desc("ycsb_hdr_window_end_timestamp_seconds", "End of the last completed HDR window, Unix seconds.", "op", "window")
	}
	if endpoints.Load() != nil {
		c.epLatency = desc("ycsb_endpoint_latency_seconds", "Cumulative latency of the requests sent to one server endpoint, in seconds: one network round trip each (a pipeline is one), not an operation. *_ERROR and *_REDIRECT ops are failed and redirected requests.", "endpoint", "op")
		c.epErrors = desc("ycsb_endpoint_errors_total", "Cumulative failed requests to one server endpoint, redirects excluded.", "endpoint", "op")
		c.epRedirects = desc("ycsb_endpoint_redirects_total", "Cumulative requests one server endpoint answered with a redirect (MOVED or ASK).", "endpoint", "op")
		c.epInfo = desc("ycsb_endpoint_info", "One per endpoint the server reported (CLUSTER NODES): its node ID, role and shard (its master's node ID).", "endpoint", "node_id", "role", "shard")
	}
	return c
}

func (c *promCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.info, c.running, c.target, c.planned, c.queueDepth, c.queueCap, c.ops, c.errors, c.latencyHist, c.latency,
		c.avg, c.max, c.count, c.window, c.end, c.hdrCount, c.hdrDropped, c.hdrLatency, c.hdrCoverage, c.hdrValid, c.hdrEnd,
		c.epLatency, c.epErrors, c.epRedirects, c.epInfo} {
		if d != nil {
			ch <- d
		}
	}
}

func (c *promCollector) Collect(ch chan<- prometheus.Metric) {
	metric := func(d *prometheus.Desc, typ prometheus.ValueType, value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, typ, value, labels...)
	}
	metric(c.info, prometheus.GaugeValue, 1, c.infoVals...)
	metric(c.running, prometheus.GaugeValue, float64(c.phaseRunning.Load()))
	metric(c.target, prometheus.GaugeValue, c.targetValue)
	if c.planned != nil {
		metric(c.planned, prometheus.GaugeValue, c.plannedValue)
	}
	metric(c.queueDepth, prometheus.GaugeValue, float64(len(c.queue)))
	metric(c.queueCap, prometheus.GaugeValue, float64(cap(c.queue)))

	c.h.mu.RLock()
	counts := make(map[string]int64, len(c.h.histograms))
	type latencySample struct {
		count   uint64
		sum     float64
		buckets map[float64]uint64
	}
	latencies := make(map[string]latencySample, len(c.h.histograms))
	for op, hist := range c.h.histograms {
		hist.mu.Lock()
		count := hist.hist.TotalCount()
		counts[op] = count
		if hist.promBuckets != nil {
			buckets := make(map[float64]uint64, len(promLatencyBucketsUs))
			var cumulative uint64
			for i, upperUs := range promLatencyBucketsUs {
				cumulative += hist.promBuckets[i]
				buckets[float64(upperUs)/1e6] = cumulative
			}
			latencies[op] = latencySample{uint64(count), hist.promSumSeconds, buckets}
		}
		hist.mu.Unlock()
	}
	c.h.mu.RUnlock()
	ops := make(map[string]int64, len(counts))
	errCounts := make(map[string]int64, len(counts))
	for op, n := range counts {
		if strings.HasSuffix(op, "_ERROR") {
			base := strings.TrimSuffix(op, "_ERROR")
			errCounts[base] = n
			if _, seen := ops[base]; !seen {
				ops[base] = counts[base]
			}
		} else {
			ops[op] = n
			if op != "TOTAL" && !strings.HasPrefix(op, "BATCH_") {
				if _, seen := errCounts[op]; !seen {
					errCounts[op] = counts[op+"_ERROR"]
				}
			}
		}
	}
	for op, n := range ops {
		metric(c.ops, prometheus.CounterValue, float64(n), op)
	}
	for op, n := range errCounts {
		metric(c.errors, prometheus.CounterValue, float64(n), op)
	}
	for op, sample := range latencies {
		ch <- prometheus.MustNewConstHistogram(c.latencyHist, sample.count, sample.sum, sample.buckets, op)
	}

	c.collectEndpoints(ch)

	snapshot := c.h.iv.latest.Load()
	if snapshot == nil {
		return
	}
	metric(c.window, prometheus.GaugeValue, snapshot.windowS)
	metric(c.end, prometheus.GaugeValue, float64(snapshot.end.UnixNano())/1e9)
	for _, r := range snapshot.records {
		if r.Count == 0 {
			continue
		}
		metric(c.count, prometheus.GaugeValue, float64(r.Count), r.Op)
		metric(c.avg, prometheus.GaugeValue, *r.AvgUs/1e6, r.Op)
		metric(c.max, prometheus.GaugeValue, float64(*r.MaxUs)/1e6, r.Op)
		for _, q := range []struct {
			label string
			value *int64
		}{{"0.5", r.P50Us}, {"0.9", r.P90Us}, {"0.95", r.P95Us}, {"0.99", r.P99Us}, {"0.999", r.P999Us}} {
			metric(c.latency, prometheus.GaugeValue, float64(*q.value)/1e6, r.Op, q.label)
		}
	}
	for _, r := range snapshot.hdr {
		window := strconv.Itoa(r.WindowSeconds) + "s"
		metric(c.hdrCount, prometheus.GaugeValue, float64(r.Count), r.Op, window)
		metric(c.hdrDropped, prometheus.GaugeValue, float64(r.Dropped), r.Op, window)
		metric(c.hdrCoverage, prometheus.GaugeValue, r.CoveredSeconds, r.Op, window)
		if r.CoverageValid {
			metric(c.hdrValid, prometheus.GaugeValue, 1, r.Op, window)
		} else {
			metric(c.hdrValid, prometheus.GaugeValue, 0, r.Op, window)
		}
		metric(c.hdrEnd, prometheus.GaugeValue, float64(r.End.UnixNano())/1e9, r.Op, window)
		if r.Count == 0 {
			continue
		}
		for _, q := range []struct {
			label string
			value int64
		}{{"0.5", r.P50Us}, {"0.9", r.P90Us}, {"0.95", r.P95Us}, {"0.99", r.P99Us}, {"0.999", r.P999Us}} {
			metric(c.hdrLatency, prometheus.GaugeValue, float64(q.value)/1e6, r.Op, window, q.label)
		}
	}
}

func (c *promCollector) collectEndpoints(ch chan<- prometheus.Metric) {
	s := endpoints.Load()
	if s == nil || c.epLatency == nil {
		return
	}
	samples, info := s.snapshot()
	errs := make(map[endpointKey]uint64)
	redirects := make(map[endpointKey]uint64)
	for _, sample := range samples {
		ch <- prometheus.MustNewConstHistogram(c.epLatency, sample.count, sample.sum, sample.buckets, sample.endpoint, sample.op)
		if op, ok := strings.CutSuffix(sample.op, EndpointError); ok {
			errs[endpointKey{sample.endpoint, op}] += sample.count
		} else if op, ok := strings.CutSuffix(sample.op, EndpointRedirect); ok {
			redirects[endpointKey{sample.endpoint, op}] += sample.count
		} else {
			// a zero, so that the rate of errors exists from the first request
			errs[endpointKey{sample.endpoint, sample.op}] += 0
			redirects[endpointKey{sample.endpoint, sample.op}] += 0
		}
	}
	for k, n := range errs {
		ch <- prometheus.MustNewConstMetric(c.epErrors, prometheus.CounterValue, float64(n), k.endpoint, k.op)
	}
	for k, n := range redirects {
		ch <- prometheus.MustNewConstMetric(c.epRedirects, prometheus.CounterValue, float64(n), k.endpoint, k.op)
	}
	seen := make(map[string]bool, len(info))
	for _, e := range info {
		if seen[e.Endpoint] {
			continue // a registry refuses a duplicate series
		}
		seen[e.Endpoint] = true
		ch <- prometheus.MustNewConstMetric(c.epInfo, prometheus.GaugeValue, 1, e.Endpoint, e.NodeID, e.Role, e.Shard)
	}
}

type promExporter struct {
	collector *promCollector
	server    *http.Server
	listener  net.Listener
	linger    time.Duration
}

var activePrometheus atomic.Pointer[promExporter]

func (e *promExporter) handler(labels prometheus.Labels) (http.Handler, error) {
	registry := prometheus.NewRegistry()
	if err := prometheus.WrapRegistererWith(labels, registry).Register(e.collector); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	if e.collector.h.packedWindows {
		mux.HandleFunc("/hdr-windows", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			h := e.collector.h
			h.iv.mu.Lock()
			var records []HDRWindowRecord
			if snapshot := h.iv.latest.Load(); snapshot != nil {
				records = h.hdrWithBuckets(snapshot.hdr)
			}
			h.iv.mu.Unlock()
			if records == nil {
				records = []HDRWindowRecord{}
			}
			_ = json.NewEncoder(w).Encode(records)
		})
	}
	return mux, nil
}

func startPrometheus(cfg promConfig, h *histograms, p *properties.Properties) error {
	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("%s=%q: %w", prop.MeasurementPrometheusListen, cfg.listen, err)
	}
	e := &promExporter{collector: newPromCollector(h, p), listener: listener, linger: cfg.linger}
	handler, err := e.handler(cfg.labels)
	if err != nil {
		listener.Close()
		return fmt.Errorf("%s: %w", prop.MeasurementPrometheusLabels, err)
	}
	e.server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	activePrometheus.Store(e)
	fmt.Fprintf(os.Stderr, "Prometheus exporter listening on %s\n", listener.Addr())
	go func() {
		if err := e.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "Prometheus exporter serve: %v\n", err)
		}
	}()
	return nil
}

// SetPhaseRunning marks the start of a run and the completion of its summary.
func SetPhaseRunning(running bool) {
	if e := activePrometheus.Load(); e != nil {
		if running {
			e.collector.phaseRunning.Store(1)
		} else {
			e.collector.phaseRunning.Store(0)
		}
	}
}

// ClosePrometheus leaves the final counts available for one last scrape.
func ClosePrometheus() {
	e := activePrometheus.Load()
	if e == nil {
		return
	}
	time.Sleep(e.linger)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.server.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Prometheus exporter shutdown: %v\n", err)
	}
	activePrometheus.CompareAndSwap(e, nil)
}

// PrometheusLinger is the configured final-scrape delay used by the signal
// watchdog's teardown budget.
func PrometheusLinger() time.Duration {
	if e := activePrometheus.Load(); e != nil {
		return e.linger
	}
	return 0
}

// PrometheusAddr returns the bound address, including the chosen port when
// measurement.prometheus_listen requested port zero.
func PrometheusAddr() string {
	if e := activePrometheus.Load(); e != nil {
		return e.listener.Addr().String()
	}
	return ""
}

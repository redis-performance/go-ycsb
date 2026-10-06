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
	listen string
	labels prometheus.Labels
	linger time.Duration
}

var promLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func parsePromLabels(value string) (prometheus.Labels, error) {
	labels := prometheus.Labels{}
	if value == "" {
		return labels, nil
	}
	reserved := map[string]bool{
		"op": true, "quantile": true, "workload": true, "command": true,
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
	ops          *prometheus.Desc
	errors       *prometheus.Desc
	latency      *prometheus.Desc
	avg          *prometheus.Desc
	max          *prometheus.Desc
	count        *prometheus.Desc
	window       *prometheus.Desc
	end          *prometheus.Desc
	infoVals     []string
	phaseRunning atomic.Int64
}

func newPromCollector(h *histograms, p *properties.Properties) *promCollector {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	return &promCollector{
		h:       h,
		info:    desc("ycsb_info", "Configuration of this YCSB process.", "workload", "command", "threadcount", "batch_size", "target", "version"),
		running: desc("ycsb_phase_running", "One while the load or run phase is active; zero after final counts are available."),
		ops:     desc("ycsb_operations_total", "Cumulative successful operations or sent batches, as counted in the final summary.", "op"),
		errors:  desc("ycsb_errors_total", "Cumulative failed operations, as counted in the final summary.", "op"),
		latency: desc("ycsb_interval_latency_seconds", "Latency quantile in the last completed interval, in seconds.", "op", "quantile"),
		avg:     desc("ycsb_interval_latency_avg_seconds", "Mean latency in the last completed interval, in seconds.", "op"),
		max:     desc("ycsb_interval_latency_max_seconds", "Maximum latency in the last completed interval, in seconds.", "op"),
		count:   desc("ycsb_interval_operations", "Operations in the last completed interval.", "op"),
		window:  desc("ycsb_interval_window_seconds", "Length of the last completed interval, in seconds."),
		end:     desc("ycsb_interval_end_timestamp_seconds", "End of the last completed interval, Unix seconds."),
		infoVals: []string{
			p.GetString(prop.Workload, "core"), p.GetString(prop.Command, ""),
			p.GetString(prop.ThreadCount, "1"),
			p.GetString(prop.BatchSize, strconv.Itoa(prop.DefaultBatchSize)),
			p.GetString(prop.Target, "0"), buildVersion(),
		},
	}
}

func (c *promCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.info, c.running, c.ops, c.errors, c.latency,
		c.avg, c.max, c.count, c.window, c.end} {
		ch <- d
	}
}

func (c *promCollector) Collect(ch chan<- prometheus.Metric) {
	metric := func(d *prometheus.Desc, typ prometheus.ValueType, value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, typ, value, labels...)
	}
	metric(c.info, prometheus.GaugeValue, 1, c.infoVals...)
	metric(c.running, prometheus.GaugeValue, float64(c.phaseRunning.Load()))

	c.h.mu.RLock()
	counts := make(map[string]int64, len(c.h.histograms))
	for op, hist := range c.h.histograms {
		hist.mu.Lock()
		counts[op] = hist.hist.TotalCount()
		hist.mu.Unlock()
	}
	c.h.mu.RUnlock()
	ops := make(map[string]int64, len(counts))
	errors := make(map[string]int64, len(counts))
	for op, n := range counts {
		if strings.HasSuffix(op, "_ERROR") {
			base := strings.TrimSuffix(op, "_ERROR")
			errors[base] = n
			if _, seen := ops[base]; !seen {
				ops[base] = counts[base]
			}
		} else {
			ops[op] = n
			if op != "TOTAL" && !strings.HasPrefix(op, "BATCH_") {
				if _, seen := errors[op]; !seen {
					errors[op] = counts[op+"_ERROR"]
				}
			}
		}
	}
	for op, n := range ops {
		metric(c.ops, prometheus.CounterValue, float64(n), op)
	}
	for op, n := range errors {
		metric(c.errors, prometheus.CounterValue, float64(n), op)
	}

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

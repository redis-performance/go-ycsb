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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	"github.com/spf13/cobra"
)

type prometheusFakeDB struct {
	ycsb.DB
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (d *prometheusFakeDB) Close() error                                             { return nil }
func (d *prometheusFakeDB) InitThread(ctx context.Context, _, _ int) context.Context { return ctx }
func (d *prometheusFakeDB) CleanupThread(context.Context)                            {}
func (d *prometheusFakeDB) BatchInsert(_ context.Context, _ string, keys []string, _ []map[string][]byte) error {
	if d.calls.Add(1) == 1 {
		close(d.started)
		<-d.release
		errs := make([]error, len(keys))
		errs[len(errs)-1] = errors.New("fake record failure")
		return ycsb.NewBatchError(errs)
	}
	return nil
}

type prometheusFakeCreator struct{ db *prometheusFakeDB }

func (c prometheusFakeCreator) Create(*properties.Properties) (ycsb.DB, error) { return c.db, nil }

func scrapeFake(t *testing.T, addr string) string {
	t.Helper()
	r, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("scrape: HTTP %d", r.StatusCode)
	}
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func fakeMetric(t *testing.T, body, name, op string) int64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") || (op != "" && !strings.Contains(line, "op=\""+op+"\"")) {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Fatalf("metric %s op %s absent from:\n%s", name, op, body)
	return 0
}

func TestPrometheusFakeCommandCountsMatchSummary(t *testing.T) {
	// This package's other tests never initialise measurement. Keep this test
	// serial: Output closes the process-wide measurement channel once.
	db := &prometheusFakeDB{started: make(chan struct{}), release: make(chan struct{})}
	ycsb.RegisterDBCreator("prometheus-fake", prometheusFakeCreator{db})
	output := t.TempDir() + "/summary.json"
	propertyFiles = nil
	propertyValues = []string{
		"workload=core", "insertcount=4", "threadcount=1", "batch.size=2", "fieldcount=1",
		"measurement.prometheus_listen=127.0.0.1:0", "measurement.prometheus_labels=phase=load",
		"measurement.prometheus_linger=0s", "measurement.interval=1s",
		"measurement.output_file=" + output, "outputstyle=json", "debug.pprof=127.0.0.1:0",
	}
	globalContext = context.Background()
	globalExitCode = 0
	done := make(chan struct{})
	defer func() {
		select {
		case <-db.release:
		default:
			close(db.release)
		}
		measurement.ClosePrometheus()
	}()
	go func() {
		runClientCommandFunc(&cobra.Command{}, []string{"prometheus-fake"}, false, "load")
		close(done)
	}()
	select {
	case <-db.started:
	case <-time.After(5 * time.Second):
		t.Fatal("fake batch was not called")
	}
	addr := measurement.PrometheusAddr()
	if addr == "" {
		t.Fatal("exporter has no bound address")
	}
	if got := fakeMetric(t, scrapeFake(t, addr), "ycsb_phase_running", ""); got != 1 {
		t.Errorf("phase_running during fake run = %d", got)
	}
	close(db.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake command did not finish")
	}
	if globalExitCode != 0 {
		t.Errorf("command exit code = %d", globalExitCode)
	}
	body := scrapeFake(t, addr)
	if got := fakeMetric(t, body, "ycsb_phase_running", ""); got != 0 {
		t.Errorf("phase_running after Output = %d", got)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var summary []map[string]string
	if err := json.Unmarshal(content, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary) == 0 {
		t.Fatal("empty final summary")
	}
	for _, row := range summary {
		op := row["Operation"]
		want, err := strconv.ParseInt(row["Count"], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		name := "ycsb_operations_total"
		if strings.HasSuffix(op, "_ERROR") {
			name, op = "ycsb_errors_total", strings.TrimSuffix(op, "_ERROR")
		}
		if got := fakeMetric(t, body, name, op); got != want {
			t.Errorf("%s %s = %d, summary = %d", name, op, got, want)
		}
	}
	for _, expected := range []struct {
		name, op string
		count    int64
	}{
		{"ycsb_operations_total", "INSERT", 3},
		{"ycsb_errors_total", "INSERT", 1},
		{"ycsb_operations_total", "TOTAL", 3},
		{"ycsb_operations_total", "BATCH_INSERT", 2},
	} {
		if got := fakeMetric(t, body, expected.name, expected.op); got != expected.count {
			t.Errorf("%s %s = %d, want %d", expected.name, expected.op, got, expected.count)
		}
	}
	if db.calls.Load() != 2 {
		t.Errorf("fake batches = %d, want 2", db.calls.Load())
	}
}

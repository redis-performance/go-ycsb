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

package redis

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/prop"
	goredis "github.com/redis/go-redis/v9"
)

var endpointExporter sync.Once

// startEndpointExporter starts the measurement with the per-endpoint
// exporter on, as go-ycsb does before it creates the DB: once, as go-ycsb
// does, so the series add up across tests and each test reads what it added
// (scrape's delta from the previous scrape).
func startEndpointExporter(t *testing.T) func() map[string]float64 {
	t.Helper()
	endpointExporter.Do(func() {
		p := properties.NewProperties()
		p.Set(prop.MeasurementPrometheusListen, "127.0.0.1:0")
		p.Set(prop.MeasurementPrometheusLinger, "0s")
		p.Set(prop.MeasurementPrometheusEndpoints, "true")
		measurement.InitMeasure(p)
	})
	if !measurement.EndpointsEnabled() {
		t.Fatal("endpoints not enabled")
	}
	read := func() map[string]float64 {
		t.Helper()
		resp, err := http.Get("http://" + measurement.PrometheusAddr() + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		series := map[string]float64{}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "ycsb_endpoint_") {
				continue
			}
			i := strings.LastIndexByte(line, ' ')
			v, err := strconv.ParseFloat(line[i+1:], 64)
			if err != nil {
				t.Fatalf("%q: %v", line, err)
			}
			series[line[:i]] = v
		}
		return series
	}
	before := read()
	return func() map[string]float64 {
		t.Helper()
		after := read()
		delta := map[string]float64{}
		for k, v := range after {
			if strings.HasPrefix(k, "ycsb_endpoint_info") {
				delta[k] = v
			} else if d := v - before[k]; d != 0 || !strings.Contains(k, "_count") {
				delta[k] = d
			}
		}
		return delta
	}
}

func epSeries(name, endpoint, op string) string {
	return name + `{endpoint="` + endpoint + `",op="` + op + `"}`
}

// Each request is timed on the endpoint it went to, under the operation it
// is part of: a batch's pipeline once per master it touched, a redirect on
// the endpoint that answered MOVED, a failure on the one that failed it.
func TestEndpointMetricsCluster(t *testing.T) {
	scrape := startEndpointExporter(t)
	ctx := context.Background()
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3")
	r.endpoints = true

	keys, values := testRecords(60)
	if err := r.BatchInsert(ctx, "usertable", keys, values); err != nil {
		t.Fatal(err)
	}
	moved := "usertable/" + keys[7]
	owner := slotOwner(moved)
	other := clusterSlots[0].Nodes[0].Addr
	if other == owner {
		other = clusterSlots[1].Nodes[0].Addr
	}
	once := true
	nodes.moved = func(addr string, args []string) string {
		if args[1] == moved && addr == owner && once {
			once = false
			return other
		}
		return ""
	}
	if err := r.Insert(ctx, "usertable", keys[7], values[7]); err != nil {
		t.Fatal(err)
	}
	nodes.moved = nil
	failed := "usertable/" + keys[8]
	nodes.fail = func(addr string, args []string) string {
		if args[1] == failed {
			return "ERR refused"
		}
		return ""
	}
	if err := r.Insert(ctx, "usertable", keys[8], values[8]); err == nil {
		t.Fatal("the failing insert succeeded")
	}
	if _, err := r.Read(ctx, "usertable", keys[1], nil); err != nil {
		t.Fatal(err)
	}

	got := scrape()
	want := map[string]float64{}
	for _, s := range clusterSlots {
		want[epSeries("ycsb_endpoint_latency_seconds_count", s.Nodes[0].Addr, "BATCH_INSERT")] = 1
	}
	want[epSeries("ycsb_endpoint_latency_seconds_count", owner, "INSERT_REDIRECT")] = 1
	want[epSeries("ycsb_endpoint_redirects_total", owner, "INSERT")] = 1
	want[epSeries("ycsb_endpoint_latency_seconds_count", other, "INSERT")] = 1
	want[epSeries("ycsb_endpoint_latency_seconds_count", slotOwner(failed), "INSERT_ERROR")] = 1
	want[epSeries("ycsb_endpoint_errors_total", slotOwner(failed), "INSERT")] = 1
	want[epSeries("ycsb_endpoint_latency_seconds_count", slotOwner("usertable/"+keys[1]), "READ")] = 1
	want[epSeries("ycsb_endpoint_errors_total", slotOwner("usertable/"+keys[1]), "READ")] = 0
	for series, v := range want {
		if got[series] != v {
			t.Errorf("%s = %v, want %v", series, got[series], v)
		}
	}
	for series := range got {
		if strings.HasPrefix(series, "ycsb_endpoint_latency_seconds_count") {
			if _, ok := want[series]; !ok {
				t.Errorf("unexpected %s = %v", series, got[series])
			}
		}
	}
}

// A single endpoint is timed the same way, under its address.
func TestEndpointMetricsSingle(t *testing.T) {
	scrape := startEndpointExporter(t)
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	r.endpoints = true
	r.client.(*goredis.Client).AddHook(endpointHook{singleAddr})
	keys, values := testRecords(2)
	if err := r.Insert(context.Background(), "usertable", keys[0], values[0]); err != nil {
		t.Fatal(err)
	}
	if got := scrape()[epSeries("ycsb_endpoint_latency_seconds_count", singleAddr, "INSERT")]; got != 1 {
		t.Errorf("single INSERT count = %v, want 1", got)
	}
}

// Without the option the binding labels nothing, so no endpoint is timed.
func TestEndpointOpOff(t *testing.T) {
	r := &redis{}
	ctx := context.Background()
	if got := r.withEndpointOp(ctx, "READ"); got != ctx {
		t.Error("withEndpointOp wrapped the context with the option off")
	}
}

func TestParseClusterNodes(t *testing.T) {
	reply := strings.Join([]string{
		"07c37dfeb235213a872192d90877d0cd55635b91 127.0.0.1:30004@31004,node-4.example slave e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca 0 1426238317239 4 connected",
		"67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1 127.0.0.1:30002@31002 master - 0 1426238316232 2 connected 5461-10922",
		"e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca 127.0.0.1:30001@31001 myself,master - 0 0 1 connected 0-5460",
		"6ec23923021cf3ffec47632106199cb7f496ce01 :0@0 master,fail,noaddr - 1426238316232 1426238315228 5 disconnected",
		"",
	}, "\n")
	got := parseClusterNodes(reply)
	want := []measurement.EndpointInfo{
		{Endpoint: "127.0.0.1:30004", NodeID: "07c37dfeb235213a872192d90877d0cd55635b91", Role: "replica", Shard: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"},
		{Endpoint: "node-4.example:30004", NodeID: "07c37dfeb235213a872192d90877d0cd55635b91", Role: "replica", Shard: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"},
		{Endpoint: "127.0.0.1:30002", NodeID: "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1", Role: "master", Shard: "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1"},
		{Endpoint: "127.0.0.1:30001", NodeID: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca", Role: "master", Shard: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseClusterNodes:\n got %+v\nwant %+v", got, want)
	}
}

// The endpoints' identities are exported as an info series to join on.
func TestEndpointInfoExported(t *testing.T) {
	scrape := startEndpointExporter(t)
	measurement.SetEndpointInfo(parseClusterNodes(
		"67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1 10.0.0.2:6379@16379 master - 0 1 2 connected 0-16383\n"))
	series := `ycsb_endpoint_info{endpoint="10.0.0.2:6379",node_id="67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1",role="master",shard="67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1"}`
	if got := scrape()[series]; got != 1 {
		t.Errorf("%s = %v, want 1", series, got)
	}
}

// A batch record redirected from its master is sent again, alone, to the node
// the redirect names: that second pipeline is an answered request there, not
// a redirect (the record's earlier MOVED isn't its outcome).
func TestEndpointMetricsBatchRedirect(t *testing.T) {
	scrape := startEndpointExporter(t)
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3")
	r.endpoints = true
	keys, values := testRecords(60)
	moved := "usertable/" + keys[7]
	owner := slotOwner(moved)
	other := clusterSlots[0].Nodes[0].Addr
	if other == owner {
		other = clusterSlots[1].Nodes[0].Addr
	}
	once := true
	nodes.moved = func(addr string, args []string) string {
		if args[1] == moved && addr == owner && once {
			once = false
			return other
		}
		return ""
	}
	if err := r.BatchInsert(context.Background(), "usertable", keys, values); err != nil {
		t.Fatal(err)
	}
	got := scrape()
	for _, s := range clusterSlots {
		addr := s.Nodes[0].Addr
		for _, op := range []string{"BATCH_INSERT", "BATCH_INSERT_REDIRECT", "BATCH_INSERT_ERROR"} {
			t.Logf("%s %s = %v", addr, op, got[epSeries("ycsb_endpoint_latency_seconds_count", addr, op)])
		}
	}
	if n := got[epSeries("ycsb_endpoint_latency_seconds_count", owner, "BATCH_INSERT_REDIRECT")]; n != 1 {
		t.Errorf("owner's redirected pipeline = %v, want 1", n)
	}
	if n := got[epSeries("ycsb_endpoint_latency_seconds_count", other, "BATCH_INSERT_REDIRECT")]; n != 0 {
		t.Errorf("the redirect's target counted %v redirects, want 0", n)
	}
	if n := got[epSeries("ycsb_endpoint_latency_seconds_count", other, "BATCH_INSERT")]; n != 2 {
		t.Errorf("the redirect's target answered %v pipelines, want 2 (its own and the redirected record)", n)
	}
}

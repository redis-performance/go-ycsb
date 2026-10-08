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
	"errors"
	"fmt"
	"io"
	"net"
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

// The binding's own Create times a single endpoint under its address, and
// Close ends it.
func TestEndpointMetricsCreateSingle(t *testing.T) {
	scrape := startEndpointExporter(t)
	nodes := newFakeNodes(t)
	conn, err := nodes.dial(context.Background(), "tcp", singleAddr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	nodes.mu.Lock()
	addr := nodes.listeners[singleAddr].Addr().String()
	nodes.mu.Unlock()
	p := properties.NewProperties()
	p.Set("threadcount", "2")
	p.Set(redisMode, "single")
	p.Set(redisAddr, addr)
	db, err := redisCreator{}.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	keys, values := testRecords(2)
	if err := db.Insert(context.Background(), "usertable", keys[0], values[0]); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(context.Background(), "usertable", keys[0]); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got := scrape()
	for _, op := range []string{"INSERT", "DELETE"} {
		if n := got[epSeries("ycsb_endpoint_latency_seconds_count", addr, op)]; n != 1 {
			t.Errorf("single %s count = %v, want 1", op, n)
		}
	}
}

// A record a master fails makes that master's pipeline an error; a redirect
// alone, a redirect. ASK is a redirect like MOVED. Update is labelled UPDATE.
func TestEndpointMetricsOutcomes(t *testing.T) {
	scrape := startEndpointExporter(t)
	ctx := context.Background()
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3")
	r.endpoints = true
	keys, values := testRecords(60)
	failed := "usertable/" + keys[3]
	nodes.fail = func(addr string, args []string) string {
		if args[1] == failed {
			return "ERR refused"
		}
		return ""
	}
	if err := r.BatchInsert(ctx, "usertable", keys, values); err == nil {
		t.Fatal("the batch with a refused record succeeded")
	}
	nodes.fail = nil
	got := scrape()
	for _, s := range clusterSlots {
		addr, want := s.Nodes[0].Addr, "BATCH_INSERT"
		if addr == slotOwner(failed) {
			want = "BATCH_INSERT_ERROR"
		}
		if n := got[epSeries("ycsb_endpoint_latency_seconds_count", addr, want)]; n != 1 {
			t.Errorf("%s %s = %v, want 1", addr, want, n)
		}
	}

	asked := "usertable/" + keys[9]
	owner := slotOwner(asked)
	other := clusterSlots[0].Nodes[0].Addr
	if other == owner {
		other = clusterSlots[1].Nodes[0].Addr
	}
	once := true
	nodes.moved = func(addr string, args []string) string {
		if args[1] == asked && addr == owner && once {
			once = false
			return "ask:" + other
		}
		return ""
	}
	if err := r.Insert(ctx, "usertable", keys[9], values[9]); err != nil {
		t.Fatal(err)
	}
	nodes.moved = nil
	if err := r.Update(ctx, "usertable", keys[9], values[9]); err != nil {
		t.Fatal(err)
	}
	got = scrape()
	for series, want := range map[string]float64{
		epSeries("ycsb_endpoint_latency_seconds_count", owner, "INSERT_REDIRECT"): 1,
		epSeries("ycsb_endpoint_latency_seconds_count", other, "INSERT"):          1,
		epSeries("ycsb_endpoint_latency_seconds_count", owner, "UPDATE"):          1,
	} {
		if got[series] != want {
			t.Errorf("%s = %v, want %v", series, got[series], want)
		}
	}
}

func TestEndpointOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs []error
		want string
	}{
		{"ok", []error{nil}, measurement.EndpointOK},
		{"missing key", []error{goredis.Nil}, measurement.EndpointOK},
		{"failed", []error{errors.New("ERR refused")}, measurement.EndpointError},
		{"client closed", []error{goredis.ErrClosed}, measurement.EndpointCanceled},
		{"closed conn", []error{fmt.Errorf("read: %w", net.ErrClosed)}, measurement.EndpointCanceled},
		{"canceled", []error{context.Canceled}, measurement.EndpointCanceled},
		{"timeout", []error{context.DeadlineExceeded}, measurement.EndpointError},
		// a pipeline: the worst of its errors, in any order
		{"redirect then canceled", []error{nil, movedErr(t), context.Canceled}, measurement.EndpointCanceled},
		{"canceled then redirect", []error{context.Canceled, movedErr(t)}, measurement.EndpointCanceled},
		{"redirect then failed", []error{movedErr(t), errors.New("ERR refused")}, measurement.EndpointError},
		{"redirect alone", []error{nil, movedErr(t)}, measurement.EndpointRedirect},
	} {
		if got := endpointOutcome(tc.errs...); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Without the option the binding labels nothing, so no endpoint is timed.
func TestEndpointOpOff(t *testing.T) {
	r := &redis{}
	ctx := context.Background()
	if got := r.withEndpointOp(ctx, opRead); got != ctx {
		t.Error("withEndpointOp wrapped the context with the option off")
	}
}

func TestParseClusterNodes(t *testing.T) {
	reply := strings.Join([]string{
		"07c37dfeb235213a872192d90877d0cd55635b91 127.0.0.1:30004@31004,node-4.example slave e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca 0 1426238317239 4 connected",
		"67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1 127.0.0.1:30002@31002 master - 0 1426238316232 2 connected 5461-10922",
		"e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca 127.0.0.1:30001@31001 myself,master - 0 0 1 connected 0-5460",
		"6ec23923021cf3ffec47632106199cb7f496ce01 :0@0 master,fail,noaddr - 1426238316232 1426238315228 5 disconnected",
		// IPv6, printed bare; an auxiliary field after the hostname
		"aaaa 2001:db8::5:7001@17001,node-6.example,shard-id=xyz master - 0 0 6 connected 10923-16383",
		// a ghost a restarted node left at the same address, before the live node
		"bbbb 10.0.0.9:7003@17003 master,fail - 0 0 7 disconnected",
		"cccc 10.0.0.9:7003@17003 master - 0 0 8 connected 16000-16100",
		"dddd 10.0.0.10:7004@17004 handshake - 0 0 0 connected",
		// a ghost with a hostname alias, replaced by a live node without one
		"eeee 10.0.0.11:7005@17005,ghost.example master,fail - 0 0 9 disconnected",
		"ffff 10.0.0.11:7005@17005 master - 0 0 10 connected 16101-16200",
		"",
	}, "\n")
	got := parseClusterNodes(reply)
	want := []measurement.EndpointInfo{
		{Endpoint: "127.0.0.1:30004", NodeID: "07c37dfeb235213a872192d90877d0cd55635b91", Role: "replica", Shard: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"},
		{Endpoint: "node-4.example:30004", NodeID: "07c37dfeb235213a872192d90877d0cd55635b91", Role: "replica", Shard: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"},
		{Endpoint: "127.0.0.1:30002", NodeID: "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1", Role: "master", Shard: "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1"},
		{Endpoint: "127.0.0.1:30001", NodeID: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca", Role: "master", Shard: "e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca"},
		{Endpoint: "[2001:db8::5]:7001", NodeID: "aaaa", Role: "master", Shard: "aaaa"},
		{Endpoint: "node-6.example:7001", NodeID: "aaaa", Role: "master", Shard: "aaaa"},
		{Endpoint: "10.0.0.9:7003", NodeID: "cccc", Role: "master", Shard: "cccc"},
		{Endpoint: "10.0.0.11:7005", NodeID: "ffff", Role: "master", Shard: "ffff"},
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

// movedErr is a MOVED reply as go-redis returns it, from a fake node.
func movedErr(t *testing.T) error {
	t.Helper()
	r, nodes := newFakeRedis(t, "single", HASH_DATATYPE, redisMaxRetries, "-1")
	nodes.moved = func(string, []string) string { return "fake-node-2:7002" }
	keys, values := testRecords(1)
	err := r.Insert(context.Background(), "usertable", keys[0], values[0])
	if !isRedirect(err) {
		t.Fatalf("fake MOVED: %v is not a redirect", err)
	}
	return err
}

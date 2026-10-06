// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package redis

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

// TestRedisBatchLoad loads the feature-store workload with the go-ycsb binary
// against a real Redis (single) and Redis Cluster with batch.size 1, 7 and
// 100, in two insertstart/insertcount steps that aren't a multiple of
// threadcount x batch.size, and checks that every batch size leaves exactly the
// same keys with exactly the same values (dataintegrity=true makes them
// deterministic) as batch.size=1, and that go-ycsb counted every record once.
//
// It needs a go-ycsb binary and the databases, and is skipped without them;
// test/integration/redis_batch.sh (make test-integration-redis-batch) starts
// them in docker and runs it:
//
//	GO_YCSB_BIN               the go-ycsb binary
//	REDIS_BATCH_IT_SINGLE     redis.addr of a single Redis
//	REDIS_BATCH_IT_CLUSTER    redis.addr of a Redis Cluster (";"-separated)
//
// Both databases are flushed.
func TestRedisBatchLoad(t *testing.T) {
	bin := os.Getenv("GO_YCSB_BIN")
	if bin == "" {
		t.Skip("GO_YCSB_BIN not set (see test/integration/redis_batch.sh)")
	}
	targets := []struct{ mode, addr string }{
		{"single", os.Getenv("REDIS_BATCH_IT_SINGLE")},
		{"cluster", os.Getenv("REDIS_BATCH_IT_CLUSTER")},
	}
	for _, target := range targets {
		if target.addr == "" {
			t.Errorf("REDIS_BATCH_IT_%s not set", strings.ToUpper(target.mode))
			continue
		}
		t.Run(target.mode, func(t *testing.T) {
			db := newITClient(t, target.mode, target.addr)
			for _, datatype := range []string{HASH_DATATYPE, STRING_DATATYPE, JSON_DATATYPE} {
				t.Run(datatype, func(t *testing.T) {
					if datatype == JSON_DATATYPE && !db.hasJSON(t) {
						t.Skip("no JSON.SET on this Redis")
					}
					var want map[string]string
					for _, batch := range []int{1, 7, 100} {
						got := db.loadSteps(t, bin, target, datatype, batch, true)
						if want == nil {
							want = got
							continue
						}
						if !reflect.DeepEqual(got, want) {
							t.Errorf("batch.size=%d: %s", batch, diff(got, want))
						}
					}
				})
			}
			// Records that fail inside a pipeline (here OOM, past maxmemory)
			// count as INSERT_ERROR, one each; the rest as INSERT, which is
			// then what the database holds.
			t.Run("oom", func(t *testing.T) {
				// each master's eviction policy, put back after the test
				var policies sync.Map
				db.eachMaster(t, func(m *goredis.Client) error {
					ctx := context.Background()
					policy, err := m.ConfigGet(ctx, "maxmemory-policy").Result()
					if err != nil {
						return err
					}
					policies.Store(m.Options().Addr, policy["maxmemory-policy"])
					return m.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err()
				})
				t.Cleanup(func() {
					db.eachMaster(t, func(m *goredis.Client) error {
						ctx := context.Background()
						if err := m.ConfigSet(ctx, "maxmemory", "0").Err(); err != nil {
							return err
						}
						if policy, ok := policies.Load(m.Options().Addr); ok && policy.(string) != "" {
							return m.ConfigSet(ctx, "maxmemory-policy", policy.(string)).Err()
						}
						return nil
					})
				})
				for _, batch := range []int{1, 100} {
					db.eachMaster(t, func(m *goredis.Client) error {
						ctx := context.Background()
						if err := m.ConfigSet(ctx, "maxmemory", "0").Err(); err != nil {
							return err
						}
						if err := m.FlushAll(ctx).Err(); err != nil {
							return err
						}
						// room for some of the records: ~1.5 MB over what the
						// empty node uses (more in cluster mode)
						used, err := usedMemory(ctx, m)
						if err != nil {
							return err
						}
						return m.ConfigSet(ctx, "maxmemory", fmt.Sprint(used+1500000)).Err()
					})
					const records = 8000
					got := summaryCounts(t, runLoad(t, bin, target, []string{
						"-p", fmt.Sprintf("recordcount=%d", records), "-p", fmt.Sprintf("insertcount=%d", records),
						"-p", fmt.Sprintf("threadcount=%d", itThreads), "-p", fmt.Sprintf("batch.size=%d", batch),
						"-p", "silence=true"}))
					dbsize := db.dbsize(t)
					t.Logf("batch.size=%d: %v, DBSIZE %d", batch, got, dbsize)
					if got["INSERT_ERROR"] == 0 || got["INSERT"] == 0 {
						t.Fatalf("batch.size=%d: want both inserted and failed records, got %v", batch, got)
					}
					if got["INSERT"]+got["INSERT_ERROR"] != records || int64(got["INSERT"]) != dbsize || got["TOTAL"] != got["INSERT"] {
						t.Errorf("batch.size=%d: INSERT %d + INSERT_ERROR %d (TOTAL %d) for %d records, DBSIZE %d",
							batch, got["INSERT"], got["INSERT_ERROR"], got["TOTAL"], records, dbsize)
					}
					for op := range got {
						if strings.HasSuffix(op, "_ERROR") && op != "INSERT_ERROR" {
							t.Errorf("batch.size=%d: %s counted (%v): the failed records are INSERT_ERROR alone", batch, op, got)
						}
					}
				}
			})
			// The workload as the benchmarks run it, random values and all:
			// batched, the same keys with the same fields.
			t.Run("feature-store", func(t *testing.T) {
				want := fieldNames(db.loadSteps(t, bin, target, HASH_DATATYPE, 1, false))
				got := fieldNames(db.loadSteps(t, bin, target, HASH_DATATYPE, 100, false))
				if !reflect.DeepEqual(got, want) {
					t.Errorf("batch.size=100: %s", diff(got, want))
				}
			})
		})
	}
}

const (
	itThreads = 16
	itTable   = "feature_store"
)

// The load steps, like a benchmark's ladder: insertstart, insertcount.
var itSteps = [][2]int{{0, 1003}, {1003, 2357}}

type itClient struct {
	mode    string
	single  *goredis.Client
	cluster *goredis.ClusterClient
}

func newITClient(t *testing.T, mode, addr string) *itClient {
	c := &itClient{mode: mode}
	if mode == "cluster" {
		c.cluster = goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: strings.Split(addr, ";")})
		t.Cleanup(func() { c.cluster.Close() })
	} else {
		c.single = goredis.NewClient(&goredis.Options{Addr: addr})
		t.Cleanup(func() { c.single.Close() })
	}
	return c
}

// eachMaster runs fn on every master (the single Redis, or every cluster
// master).
func (c *itClient) eachMaster(t *testing.T, fn func(*goredis.Client) error) {
	t.Helper()
	ctx := context.Background()
	var err error
	if c.cluster != nil {
		err = c.cluster.ForEachMaster(ctx, func(_ context.Context, m *goredis.Client) error { return fn(m) })
	} else {
		err = fn(c.single)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (c *itClient) hasJSON(t *testing.T) bool {
	ctx := context.Background()
	var do func(args ...interface{}) error
	if c.cluster != nil {
		do = func(args ...interface{}) error { return c.cluster.Do(ctx, args...).Err() }
	} else {
		do = func(args ...interface{}) error { return c.single.Do(ctx, args...).Err() }
	}
	ok := do("JSON.SET", "go-ycsb-it-probe", ".", "{}") == nil
	if err := do("DEL", "go-ycsb-it-probe"); err != nil {
		t.Fatal(err)
	}
	return ok
}

// loadSteps flushes the database, loads itSteps with batch.size=batch and
// returns every key's value.
func (c *itClient) loadSteps(t *testing.T, bin string, target struct{ mode, addr string }, datatype string, batch int, deterministic bool) map[string]string {
	t.Helper()
	c.eachMaster(t, func(m *goredis.Client) error { return m.FlushAll(context.Background()).Err() })
	total := 0
	for _, step := range itSteps {
		total = step[0] + step[1]
	}
	for _, step := range itSteps {
		args := []string{"-p", "redis.datatype=" + datatype,
			"-p", fmt.Sprintf("recordcount=%d", total),
			"-p", fmt.Sprintf("insertstart=%d", step[0]), "-p", fmt.Sprintf("insertcount=%d", step[1]),
			"-p", fmt.Sprintf("threadcount=%d", itThreads), "-p", fmt.Sprintf("batch.size=%d", batch),
			"-p", "insertorder=ordered"}
		if deterministic {
			args = append(args, "-p", "dataintegrity=true", "-p", "fieldlengthdistribution=constant")
		}
		checkSummary(t, runLoad(t, bin, target, args), step[1], batch)
	}
	values := c.dump(t, datatype)
	if len(values) != total {
		t.Fatalf("batch.size=%d: %d keys after loading %d records", batch, len(values), total)
	}
	if dbsize := c.dbsize(t); dbsize != int64(total) {
		t.Fatalf("batch.size=%d: DBSIZE %d after loading %d records", batch, dbsize, total)
	}
	return values
}

func usedMemory(ctx context.Context, m *goredis.Client) (int64, error) {
	info, err := m.Info(ctx, "memory").Result()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "used_memory:"); ok {
			return strconv.ParseInt(v, 10, 64)
		}
	}
	return 0, fmt.Errorf("no used_memory in INFO memory")
}

// runLoad runs a feature-store load into target with the extra args and
// returns its output.
func runLoad(t *testing.T, bin string, target struct{ mode, addr string }, extra []string) string {
	t.Helper()
	args := append([]string{"load", "redis", "-P", workloadFile(t),
		"-p", "redis.mode=" + target.mode, "-p", "redis.addr=" + target.addr}, extra...)
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("go-ycsb %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// dbsize sums DBSIZE over the masters.
func (c *itClient) dbsize(t *testing.T) int64 {
	t.Helper()
	var dbsize int64
	var mu sync.Mutex
	c.eachMaster(t, func(m *goredis.Client) error {
		n, err := m.DBSize(context.Background()).Result()
		mu.Lock()
		dbsize += n
		mu.Unlock()
		return err
	})
	return dbsize
}

func workloadFile(t *testing.T) string {
	p, err := filepath.Abs("../../workloads/workload_feature_store")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var summaryLine = regexp.MustCompile(`^([A-Z_]+)\s+- Takes\(s\): [0-9.]+, Count: ([0-9]+),`)

// checkSummary checks go-ycsb's final summary: count records counted as
// INSERT (and TOTAL), none failed, and with batches each thread's share in
// ceil(share/batch) BATCH_INSERTs.
func checkSummary(t *testing.T, out string, count, batch int) {
	t.Helper()
	got := summaryCounts(t, out)
	want := map[string]int{"INSERT": count, "TOTAL": count}
	if batch > 1 {
		batches := 0
		for i := 0; i < itThreads; i++ {
			share := count / itThreads
			if i < count%itThreads {
				share++
			}
			batches += (share + batch - 1) / batch
		}
		want["BATCH_INSERT"] = batches
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary counts %v, want %v:\n%s", got, want, out)
	}
}

// summaryCounts returns the Count of every operation in go-ycsb's final
// summary.
func summaryCounts(t *testing.T, out string) map[string]int {
	t.Helper()
	_, summary, ok := strings.Cut(out, "Run finished")
	if !ok {
		t.Fatalf("no final summary:\n%s", out)
	}
	got := map[string]int{}
	for _, line := range strings.Split(summary, "\n") {
		if m := summaryLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			got[m[1]], _ = strconv.Atoi(m[2])
		}
	}
	return got
}

// dump returns every key's value, as text.
func (c *itClient) dump(t *testing.T, datatype string) map[string]string {
	t.Helper()
	ctx := context.Background()
	values := map[string]string{}
	var mu sync.Mutex
	c.eachMaster(t, func(m *goredis.Client) error {
		iter := m.Scan(ctx, 0, itTable+"/*", 1000).Iterator()
		for iter.Next(ctx) {
			key := iter.Val()
			var v string
			switch datatype {
			case HASH_DATATYPE:
				h, err := m.HGetAll(ctx, key).Result()
				if err != nil {
					return err
				}
				fields := make([]string, 0, len(h))
				for f, fv := range h {
					fields = append(fields, f+"="+fv)
				}
				sort.Strings(fields)
				v = strings.Join(fields, "\n")
			case STRING_DATATYPE:
				s, err := m.Get(ctx, key).Result()
				if err != nil {
					return err
				}
				v = s
			case JSON_DATATYPE:
				s, err := m.Do(ctx, "JSON.GET", key).Text()
				if err != nil {
					return err
				}
				v = s
			}
			mu.Lock()
			values[key] = v
			mu.Unlock()
		}
		return iter.Err()
	})
	return values
}

// fieldNames keeps the field names of dump's hash values.
func fieldNames(values map[string]string) map[string]string {
	names := make(map[string]string, len(values))
	for k, v := range values {
		var fs []string
		for _, fv := range strings.Split(v, "\n") {
			f, _, _ := strings.Cut(fv, "=")
			fs = append(fs, f)
		}
		names[k] = strings.Join(fs, ",")
	}
	return names
}

func diff(got, want map[string]string) string {
	var missing, extra, changed []string
	for k, v := range want {
		g, ok := got[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case g != v:
			changed = append(changed, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(changed)
	cut := func(s []string) []string {
		if len(s) > 5 {
			return append(s[:5:5], "...")
		}
		return s
	}
	return fmt.Sprintf("%d keys vs %d: %d missing %v, %d extra %v, %d with other values %v",
		len(got), len(want), len(missing), cut(missing), len(extra), cut(extra), len(changed), cut(changed))
}

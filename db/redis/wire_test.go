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
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	goredis "github.com/redis/go-redis/v9"
)

// On the wire, by default: no CLIENT MAINT_NOTIFICATIONS on connecting, and
// no COMMAND (cluster routing policies) per command. Turned on, they are sent.
func TestClientBehaviourOnTheWire(t *testing.T) {
	ctx := context.Background()
	keys, values := testRecords(20)
	for _, c := range []struct {
		mode  string
		props []string
		want  map[string]bool // command -> sent
	}{
		{"single", nil, map[string]bool{"client maint_notifications": false, "command": false}},
		{"cluster", nil, map[string]bool{"client maint_notifications": false, "command": false}},
		{"single", []string{redisMaintNotifications, "auto"}, map[string]bool{"client maint_notifications": true}},
		{"cluster", []string{redisMaintNotifications, "auto"}, map[string]bool{"client maint_notifications": true}},
		{"cluster", []string{redisRoutingPolicies, "true"}, map[string]bool{"command": true}},
	} {
		r, nodes := newFakeRedis(t, c.mode, HASH_DATATYPE, c.props...)
		for i := range keys {
			if err := r.Insert(ctx, "usertable", keys[i], values[i]); err != nil {
				t.Fatalf("%s %v: Insert: %v", c.mode, c.props, err)
			}
		}
		if err := r.BatchInsert(ctx, "usertable", keys, values); err != nil {
			t.Fatalf("%s %v: BatchInsert: %v", c.mode, c.props, err)
		}
		names := nodes.sentNames()
		for cmd, want := range c.want {
			if got := names[cmd] > 0; got != want {
				t.Errorf("%s %v: %q sent %d times, want sent: %v (all: %v)", c.mode, c.props, cmd, names[cmd], want, names)
			}
		}
	}
}

// A MOVED (or ASK) reply sends the record to the node it names, where it goes
// in: the batch succeeds. A key the nodes bounce between them for good fails
// alone, once the redirects (redis.max_redirects) run out.
func TestBatchInsertRedirects(t *testing.T) {
	ctx := context.Background()
	keys, values := testRecords(60)
	key := "usertable/" + keys[7]
	owner := slotOwner(key)
	other := clusterSlots[0].Nodes[0].Addr
	if other == owner {
		other = clusterSlots[1].Nodes[0].Addr
	}
	for _, kind := range []string{"", "ask:"} {
		r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3")
		once := true
		nodes.moved = func(addr string, args []string) string {
			if args[1] == key && addr == owner && once {
				once = false
				return kind + other
			}
			return ""
		}
		if err := r.BatchInsert(ctx, "usertable", keys, values); err != nil {
			t.Fatalf("%s one redirect: %v", kind, err)
		}
		landed := ""
		for _, c := range nodes.take() {
			if c.args[1] == key {
				landed = c.addr
			}
		}
		if landed != other {
			t.Errorf("%s one redirect: %s went to %q, want %s", kind, key, landed, other)
		}
	}

	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3")
	bounces := 0
	nodes.moved = func(addr string, args []string) string {
		if args[1] != key {
			return ""
		}
		bounces++
		if addr == owner {
			return other
		}
		return owner
	}
	err := r.BatchInsert(ctx, "usertable", keys, values)
	var be *ycsb.BatchError
	if !errors.As(err, &be) || be.Failed() != 1 || be.Errs[7] == nil {
		t.Fatalf("ping-pong: %v, want %s alone failed", err, key)
	}
	if got := len(nodes.take()); got != len(keys)-1 {
		t.Errorf("ping-pong: %d records went in, want %d", got, len(keys)-1)
	}
	// the first try and redis.max_redirects=3 more
	if bounces != 4 {
		t.Errorf("ping-pong: the key was sent %d times, want 4", bounces)
	}

	r, nodes = newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "1")
	bounces = 0
	nodes.moved = func(addr string, args []string) string {
		if args[1] != key {
			return ""
		}
		bounces++
		if addr == owner {
			return other
		}
		return owner
	}
	if err := r.BatchInsert(ctx, "usertable", keys, values); err == nil {
		t.Fatal("ping-pong with max_redirects=1: no error")
	}
	if bounces != 2 {
		t.Errorf("ping-pong with max_redirects=1: the key was sent %d times, want 2", bounces)
	}
}

// A batch the run's stop came before (here before redis's own check) fails
// every record with an error wrapping ycsb.ErrNotRun and the stop's, which
// the client counts as nothing; nothing is sent.
func TestBatchInsertCanceled(t *testing.T) {
	keys, values := testRecords(30)
	for _, mode := range []string{"single", "cluster"} {
		r, nodes := newFakeRedis(t, mode, HASH_DATATYPE)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := r.BatchInsert(ctx, "usertable", keys, values)
		var be *ycsb.BatchError
		if !errors.As(err, &be) {
			t.Fatalf("%s: %v, want a BatchError", mode, err)
		}
		for i, recErr := range be.Errs {
			if !errors.Is(recErr, ycsb.ErrNotRun) || !errors.Is(recErr, context.Canceled) {
				t.Errorf("%s: record %d failed with %v, want ycsb.ErrNotRun and context.Canceled", mode, i, recErr)
			}
		}
		if sent := nodes.take(); len(sent) != 0 {
			t.Errorf("%s: %d records sent after the stop", mode, len(sent))
		}
	}
}

// The slot map is reloaded every redis.cluster_state_reload_interval (10 s
// by default, as go-redis v9.8.0 did). The bounds are loose, for a slow
// runner: by default at most a load and a lazy reload in 1 s, at 100 ms
// at least two loads in 2 s.
func TestClusterStateReloadCadence(t *testing.T) {
	ctx := context.Background()
	keys, values := testRecords(5)
	for _, c := range []struct {
		interval string
		window   time.Duration
		min, max int32
	}{{"", time.Second, 1, 2}, {"100ms", 2 * time.Second, 2, 1000}} {
		var props []string
		if c.interval != "" {
			props = []string{redisClusterStateReloadInterval, c.interval}
		}
		r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, props...)
		deadline := time.Now().Add(c.window)
		for time.Now().Before(deadline) {
			if err := r.BatchInsert(ctx, "usertable", keys, values); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if got := nodes.slotLoads.Load(); got < c.min || got > c.max {
			t.Errorf("interval %q: %d slot map loads in %v, want %d..%d", c.interval, got, c.window, c.min, c.max)
		}
	}
}

// The run stopping while a batch's retry backs off (one node lost its
// connection mid-batch) doesn't turn the batch's outcome into "stopped": the
// retry runs, and every record, the ones written in the first try too,
// reports what happened to it. Here all go in, so the client counts them all
// as inserted, as the database holds them.
func TestStopDuringRetryKeepsOutcome(t *testing.T) {
	keys, values := testRecords(60)
	slow := clusterSlots[1].Nodes[0].Addr
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3", redisMinRetryBackoff, "100ms", redisMaxRetryBackoff, "100ms")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	once := true
	nodes.drop = func(addr string, _ []string) bool {
		if addr == slow && once {
			once = false
			cancel() // the run stops while the client backs off to retry
			return true
		}
		return false
	}
	if err := r.BatchInsert(ctx, "usertable", keys, values); err != nil {
		t.Fatalf("BatchInsert = %v, want every record in", err)
	}
	got := map[string]bool{}
	for _, c := range nodes.take() {
		got[c.args[1]] = true
	}
	if len(got) != len(keys) {
		t.Fatalf("%d distinct records written, want %d", len(got), len(keys))
	}

	// the same for a record sent on its own
	key := keys[0]
	for slotOwner("usertable/"+key) != slow {
		key += "x"
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	once = true
	if err := r.Insert(ctx, "usertable", key, values[0]); err != nil {
		t.Fatalf("Insert = %v, want it in", err)
	}
	if sent := nodes.take(); len(sent) != 1 || sent[0].args[1] != "usertable/"+key {
		t.Fatalf("sent %v, want the record once", sent)
	}
}

// A stop waits for the operations in flight up to stopGrace, then closes the
// client, which ends even a read in progress: a batch stuck on a stalled node
// doesn't hold the run until it is force-exited, 10 s after the stop, without
// its summary, whatever redis.read_timeout. What the grace ends fails (the
// client counts it as failed): the stalled node's records never succeed.
func TestStopGraceBoundsTheWait(t *testing.T) {
	orig := stopGrace
	stopGrace = 500 * time.Millisecond
	t.Cleanup(func() { stopGrace = orig })
	keys, values := testRecords(30)
	for _, readTimeout := range []string{"300ms", "30s"} {
		// the default 3 retries: after the close they fail at once, but each
		// waits its back-off (8..512 ms, growing)
		r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisReadTimeout, readTimeout, redisMaxRedirects, "0")
		nodes.mu.Lock()
		nodes.hang = map[string]bool{clusterSlots[1].Nodes[0].Addr: true}
		nodes.mu.Unlock()
		run, stop := context.WithCancel(context.Background())
		ctx := r.InitThread(run, 0, 1)
		var stopAt time.Time
		go func() { time.Sleep(stopAfter); stopAt = time.Now(); stop() }()
		err := r.BatchInsert(ctx, "usertable", keys, values)
		ended := time.Now()
		if after := ended.Sub(stopAt); after < stopGrace-50*time.Millisecond || after > stopGrace+pastTheGrace {
			t.Errorf("read_timeout %s: the batch ended %v after the stop, want the %v grace", readTimeout, after, stopGrace)
		}
		var be *ycsb.BatchError
		if !errors.As(err, &be) {
			t.Fatalf("read_timeout %s: err %v, want the stalled node's records failed", readTimeout, err)
		}
		for i, key := range keys {
			onStalled := slotOwner("usertable/"+key) == clusterSlots[1].Nodes[0].Addr
			if onStalled && be.Errs[i] == nil {
				t.Errorf("read_timeout %s: %s, on the stalled node, didn't fail", readTimeout, key)
			}
			// the close ends the stalled node's part; the others' replies stand
			if !onStalled && be.Errs[i] != nil {
				t.Errorf("read_timeout %s: %s, on a node that answered, failed: %v", readTimeout, key, be.Errs[i])
			}
			if errors.Is(be.Errs[i], context.Canceled) {
				t.Errorf("read_timeout %s: %s failed with the stop's error, not the close's: %v", readTimeout, key, be.Errs[i])
			}
		}
		if !graceEnded(r, ctx) {
			t.Errorf("read_timeout %s: the client wasn't closed at the end of the grace", readTimeout)
		}
		if err := r.Close(); err != nil {
			t.Errorf("read_timeout %s: Close after the grace's close: %v", readTimeout, err)
		}
		r.CleanupThread(ctx)
	}
}

// The InitThread variant of TestStopDuringRetryKeepsOutcome: the thread's
// context without its cancellation carries the batch through its retry.
func TestStopDuringRetryKeepsOutcomeOnTheThread(t *testing.T) {
	keys, values := testRecords(60)
	slow := clusterSlots[1].Nodes[0].Addr
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3", redisMinRetryBackoff, "100ms", redisMaxRetryBackoff, "100ms")
	run, stop := context.WithCancel(context.Background())
	ctx := r.InitThread(run, 0, 1)
	defer r.CleanupThread(ctx)
	once := true
	nodes.drop = func(addr string, _ []string) bool {
		if addr == slow && once {
			once = false
			stop()
			return true
		}
		return false
	}
	if err := r.BatchInsert(ctx, "usertable", keys, values); err != nil {
		t.Fatalf("BatchInsert = %v, want every record in", err)
	}
	if got := len(nodes.take()); got != len(keys) {
		t.Fatalf("%d records written, want %d", got, len(keys))
	}
}

// The same for every operation sent on its own: one in flight at the stop
// (its connection lost, retried after a back-off) runs to its outcome, not to
// the stop's error.
func TestStopDuringRetryPerOperation(t *testing.T) {
	slow := clusterSlots[1].Nodes[0].Addr
	key := "k"
	for slotOwner("usertable/"+key) != slow {
		key += "x"
	}
	_, values := testRecords(1)
	for _, c := range []struct {
		name string
		op   func(r *redis, ctx context.Context) error
	}{
		{"Insert", func(r *redis, ctx context.Context) error { return r.Insert(ctx, "usertable", key, values[0]) }},
		{"Update", func(r *redis, ctx context.Context) error { return r.Update(ctx, "usertable", key, values[0]) }},
		{"Read", func(r *redis, ctx context.Context) error { _, err := r.Read(ctx, "usertable", key, nil); return err }},
		{"Delete", func(r *redis, ctx context.Context) error { return r.Delete(ctx, "usertable", key) }},
	} {
		r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3", redisMinRetryBackoff, "100ms", redisMaxRetryBackoff, "100ms")
		run, stop := context.WithCancel(context.Background())
		ctx := r.InitThread(run, 0, 1)
		once := true
		nodes.drop = func(addr string, _ []string) bool {
			if addr == slow && once {
				once = false
				stop()
				return true
			}
			return false
		}
		if err := c.op(r, ctx); err != nil {
			t.Errorf("%s in flight at the stop: %v, want its own outcome (nil)", c.name, err)
		}
		r.CleanupThread(ctx)
	}
}

// The run's stop arms one close of the client, stopGrace later; a Close
// before then (the run ended in time) disarms it, and a stop that came
// before any thread started still arms it.
func TestStopClose(t *testing.T) {
	orig := stopGrace
	stopGrace = 100 * time.Millisecond
	t.Cleanup(func() { stopGrace = orig })

	// stop, then the grace closes the client, once, for every thread (a
	// longer grace here, so that a slow runner can't pass its end before the
	// check that it hasn't come yet)
	stopGrace = time.Second
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	run, stop := context.WithCancel(context.Background())
	var ctx context.Context
	for i := 0; i < 50; i++ {
		ctx = r.InitThread(run, i, 50)
	}
	stop()
	time.Sleep(stopGrace / 10)
	if clientClosed(r) {
		t.Fatal("closed before the grace ended")
	}
	if !eventually(5*time.Second, func() bool { return graceEnded(r, ctx) }) {
		t.Fatal("the grace didn't close the client")
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close after the grace's close: %v", err)
	}

	// stop, then the run's own Close before the grace ends: nothing later
	stopGrace = 100 * time.Millisecond
	r, _ = newFakeRedis(t, "single", HASH_DATATYPE)
	run, stop = context.WithCancel(context.Background())
	ctx = r.InitThread(run, 0, 1)
	stop()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * stopGrace)
	if graceEnded(r, ctx) {
		t.Fatal("the grace's close ran after Close disarmed it")
	}

	// a run stopped before its threads started
	r, _ = newFakeRedis(t, "single", HASH_DATATYPE)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = r.InitThread(stopped, 0, 1)
	if !eventually(5*time.Second, func() bool { return graceEnded(r, ctx) }) {
		t.Fatal("a stop before InitThread armed no close")
	}
}

func TestStopGraceValue(t *testing.T) {
	if stopGrace != 5*time.Second {
		t.Fatalf("stopGrace %v, want 5s: well inside go-ycsb's 10 s force-exit", stopGrace)
	}
}

type ctxKey struct{}

// started: an operation after the stop isn't sent; one sent keeps its own
// context's values, and the stop doesn't cancel it.
func TestStarted(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKey{}, "v")
	r, nodes := newFakeRedis(t, "single", HASH_DATATYPE)
	run, stop := context.WithCancel(context.Background())
	tctx := r.InitThread(run, 0, 1)
	defer r.CleanupThread(tctx)
	opCtx, err := started(context.WithValue(tctx, ctxKey{}, "op"))
	if err != nil || opCtx.Value(ctxKey{}) != "op" {
		t.Fatalf("started: %v, value %v", err, opCtx.Value(ctxKey{}))
	}
	if sent, _ := started(tctx); sent != tctx.Value(threadKey{}).(*thread).sent {
		t.Error("an operation on the thread's own context doesn't reuse the thread's sent context")
	}
	stop()
	if opCtx.Err() != nil {
		t.Fatal("the run's stop canceled an operation in flight")
	}
	if _, err := started(tctx); !errors.Is(err, context.Canceled) || !errors.Is(err, ycsb.ErrNotRun) {
		t.Fatalf("started after the stop: %v, want ycsb.ErrNotRun and the stop", err)
	}
	if _, err := started(ctx); err != nil {
		t.Fatalf("started on a context InitThread didn't make: %v", err)
	}
	// every operation: not sent after the stop
	keys, values := testRecords(1)
	if _, err := r.Read(tctx, "usertable", keys[0], nil); !errors.Is(err, ycsb.ErrNotRun) {
		t.Errorf("Read after the stop: %v, want ycsb.ErrNotRun", err)
	}
	if err := r.Update(tctx, "usertable", keys[0], values[0]); !errors.Is(err, ycsb.ErrNotRun) {
		t.Errorf("Update after the stop: %v, want ycsb.ErrNotRun", err)
	}
	if err := r.Delete(tctx, "usertable", keys[0]); !errors.Is(err, ycsb.ErrNotRun) {
		t.Errorf("Delete after the stop: %v, want ycsb.ErrNotRun", err)
	}
	if err := r.Insert(tctx, "usertable", keys[0], values[0]); !errors.Is(err, ycsb.ErrNotRun) {
		t.Errorf("Insert after the stop: %v, want ycsb.ErrNotRun", err)
	}
	for _, name := range []string{"hset", "hgetall", "del", "hmget"} {
		if n := nodes.sentNames()[name]; n != 0 {
			t.Errorf("%s sent %d times after the stop", name, n)
		}
	}
}

// A node that is slow (but within the grace) when the run stops: the batch
// completes, every record reports its real outcome, and all are counted.
func TestStopWithASlowNode(t *testing.T) {
	keys, values := testRecords(60)
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE)
	nodes.mu.Lock()
	nodes.delay = map[string]time.Duration{clusterSlots[1].Nodes[0].Addr: 50 * time.Millisecond} // per record: some 1 s per batch
	nodes.mu.Unlock()
	run, stop := context.WithCancel(context.Background())
	ctx := r.InitThread(run, 0, 1)
	defer r.CleanupThread(ctx)
	go func() { time.Sleep(stopAfter); stop() }()
	if err := r.BatchInsert(ctx, "usertable", keys, values); err != nil {
		t.Fatalf("BatchInsert = %v, want every record in", err)
	}
	if got := len(nodes.take()); got != len(keys) {
		t.Fatalf("%d records written, want %d", got, len(keys))
	}
	// a batch after the stop isn't sent
	err := r.BatchInsert(ctx, "usertable", keys, values)
	var be *ycsb.BatchError
	if !errors.As(err, &be) || be.Failed() != len(keys) || !errors.Is(be.Errs[0], context.Canceled) {
		t.Fatalf("after the stop: %v, want every record failed with the stop", err)
	}
	if got := len(nodes.take()); got != 0 {
		t.Fatalf("%d records sent after the stop", got)
	}
}

// The startup check pings a master, as go-redis v9.8.0's Ping did; go-redis
// v9.22.0's own Ping, without routing policies, can go to a replica.
func TestStartupPingGoesToAMaster(t *testing.T) {
	orig := clusterSlots
	t.Cleanup(func() { clusterSlots = orig })
	masters := map[string]bool{}
	var withReplicas []goredis.ClusterSlot
	for i, s := range orig {
		masters[s.Nodes[0].Addr] = true
		s.Nodes = append([]goredis.ClusterNode{}, s.Nodes[0], goredis.ClusterNode{Addr: fmt.Sprintf("fake-replica-%d:7101", i)})
		withReplicas = append(withReplicas, s)
	}
	clusterSlots = withReplicas
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE)
	c := r.client.(*goredis.ClusterClient)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		if err := pingMaster(ctx, c, 1); err != nil {
			t.Fatal(err)
		}
	}
	pingsAt := func() map[string]int {
		nodes.mu.Lock()
		defer nodes.mu.Unlock()
		at := map[string]int{}
		for _, c := range nodes.allAt {
			if addr, name, _ := strings.Cut(c, " "); name == "ping" {
				at[addr]++
			}
		}
		nodes.allAt = nil
		return at
	}
	for addr := range pingsAt() {
		if !masters[addr] {
			t.Errorf("the startup ping went to %s, not a master", addr)
		}
	}
	for i := 0; i < 30; i++ {
		_ = c.Ping(ctx).Err()
	}
	replicas := 0
	for addr, n := range pingsAt() {
		if !masters[addr] {
			replicas += n
		}
	}
	if replicas == 0 {
		t.Log("go-redis's own Ping hit no replica in 30 tries: this check may be no longer needed")
	}
}

// newClusterClient pings a master, max_redirects + 1 tries of failing
// pings; a slot map that can't load fails at once, without a try each.
func TestNewClusterClientPing(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		redirects string
		pings     int
	}{{"-1", 1}, {"0", 4}, {"1", 2}} {
		_, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE)
		p := properties.NewProperties()
		p.Set("threadcount", "1")
		p.Set(redisMinIdleConns, "0")
		p.Set(redisMaxRedirects, c.redirects)
		opts, err := getOptionsCluster(p)
		if err != nil {
			t.Fatal(err)
		}
		opts.Addrs = []string{clusterSlots[0].Nodes[0].Addr}
		opts.ClusterSlots = func(context.Context) ([]goredis.ClusterSlot, error) { return clusterSlots, nil }
		var open atomic.Int32 // this client's connections still open
		opts.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := nodes.dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			open.Add(1)
			return &countedConn{Conn: conn, open: &open}, nil
		}
		nodes.mu.Lock()
		nodes.failPing = true
		nodes.mu.Unlock()
		if _, err := newClusterClient(ctx, opts); err == nil {
			t.Fatalf("max_redirects=%s: no error with every ping failing", c.redirects)
		}
		if n := open.Load(); n != 0 {
			t.Errorf("max_redirects=%s: %d connections left open by the failed client", c.redirects, n)
		}
		if got := nodes.sentNames()["ping"]; got != c.pings {
			t.Errorf("max_redirects=%s: %d pings, want %d", c.redirects, got, c.pings)
		}
	}

	var loads atomic.Int32
	p := properties.NewProperties()
	p.Set("threadcount", "1")
	opts, err := getOptionsCluster(p)
	if err != nil {
		t.Fatal(err)
	}
	opts.Addrs = []string{"fake-node-1:7001"}
	opts.ClusterSlots = func(context.Context) ([]goredis.ClusterSlot, error) {
		loads.Add(1)
		return nil, errors.New("cluster down")
	}
	if _, err := newClusterClient(ctx, opts); err == nil {
		t.Fatal("no error with the slot map failing to load")
	}
	// ReloadState's own load and the one try's
	if n := loads.Load(); n > 2 {
		t.Errorf("%d slot map loads, want at most 2: the tries mustn't each load it", n)
	}
}

// Single mode: the one server stalls; at the end of the grace the close
// fails the whole pipeline in flight (every record of the batch, whatever
// the server ran), and an operation sent on its own fails too; neither
// with the stop's context error, so the client counts them as failed.
func TestStopGraceSingleMode(t *testing.T) {
	orig := stopGrace
	stopGrace = 300 * time.Millisecond
	t.Cleanup(func() { stopGrace = orig })
	keys, values := testRecords(20)
	for _, batched := range []bool{true, false} {
		r, nodes := newFakeRedis(t, "single", HASH_DATATYPE, redisReadTimeout, "30s")
		nodes.mu.Lock()
		nodes.hang = map[string]bool{singleAddr: true}
		nodes.mu.Unlock()
		run, stop := context.WithCancel(context.Background())
		ctx := r.InitThread(run, 0, 1)
		go func() { time.Sleep(stopAfter); stop() }()
		begin := time.Now()
		var errs []error
		if batched {
			err := r.BatchInsert(ctx, "usertable", keys, values)
			var be *ycsb.BatchError
			if !errors.As(err, &be) {
				t.Fatalf("batched: %v, want a BatchError", err)
			}
			errs = be.Errs
		} else {
			errs = []error{r.Insert(ctx, "usertable", keys[0], values[0])}
		}
		if took := time.Since(begin); took > stopAfter+stopGrace+pastTheGrace {
			t.Errorf("batched=%v: took %v, want about the %v + %v grace", batched, took, stopAfter, stopGrace)
		}
		for i, err := range errs {
			if err == nil || errors.Is(err, context.Canceled) {
				t.Errorf("batched=%v: record %d: %v, want the close's error", batched, i, err)
			}
		}
		if !graceEnded(r, ctx) {
			t.Errorf("batched=%v: the grace didn't close the client", batched)
		}
	}
}

// countedConn counts itself out of open when closed.
type countedConn struct {
	net.Conn
	open *atomic.Int32
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}

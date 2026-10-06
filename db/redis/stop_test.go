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
	"net"
	"sync"
	"testing"
	"time"

	"github.com/magiconair/properties"
	goredis "github.com/redis/go-redis/v9"
)

// The end of a stop's grace must end every operation still in flight at
// once, whatever it is waiting for: a read (the client's close ends it), but
// also a retry's back-off, a pool turn or a dial, which go-redis waits for on
// the operation's context, not on the client. Otherwise the run can pass
// go-ycsb's force-exit, 10 s after the stop, without its summary. Each test
// allows the grace plus 2 s, for a slow shared runner under -race: what they
// end would otherwise run for 30 s (a read or dial timeout) or more.
const pastTheGrace = 2 * time.Second

// stopAfter is how long a test lets an operation get sent before it stops
// the run.
const stopAfter = 300 * time.Millisecond

// eventually polls cond for up to within.
func eventually(within time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(within); ; time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// clientClosed says whether r's client is closed.
func clientClosed(r *redis) bool {
	return errors.Is(r.client.Do(context.Background(), "ping").Err(), goredis.ErrClosed)
}

// graceEnded says whether the end of the grace of ctx's run (ctx is a
// thread's context) closed the client and canceled the run's operations'
// context (closeToCancel after the close), waiting for the cancel up to
// closeToCancel and a second.
func graceEnded(r *redis, ctx context.Context) bool {
	s := ctx.Value(threadKey{}).(*thread).run
	if !clientClosed(r) {
		return false
	}
	select {
	case <-s.done.Done():
		return true
	case <-time.After(closeToCancel + time.Second):
		return false
	}
}

// keyOn returns a key, starting with prefix, whose slot is on addr.
func keyOn(prefix, addr string) string {
	k := prefix
	for slotOwner("usertable/"+k) != addr {
		k += "x"
	}
	return k
}

func withGrace(t *testing.T, grace time.Duration) {
	orig := stopGrace
	stopGrace = grace
	t.Cleanup(func() { stopGrace = orig })
}

// An operation in flight on a stalled master and one waiting for the pool's
// only connection: after the close, the cluster client retries the closed
// connection's error through every redirect, each after a back-off.
func TestStopEndsRetriesAndPoolWaitsAtTheGrace(t *testing.T) {
	for _, redirects := range []string{"0", "16"} {
		withGrace(t, 300*time.Millisecond)
		slow := clusterSlots[1].Nodes[0].Addr
		r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE,
			redisReadTimeout, "30s", redisPoolSize, "1", redisMaxRedirects, redirects)
		nodes.mu.Lock()
		nodes.hang = map[string]bool{slow: true}
		nodes.mu.Unlock()
		_, values := testRecords(1)
		run, stop := context.WithCancel(context.Background())
		inFlight, waiting := r.InitThread(run, 0, 2), r.InitThread(run, 1, 2)
		var wg sync.WaitGroup
		var endInFlight, endWaiting time.Time
		var errInFlight, errWaiting error
		wg.Add(2)
		go func() {
			defer wg.Done()
			errInFlight = r.Insert(inFlight, "usertable", keyOn("a", slow), values[0])
			endInFlight = time.Now()
		}()
		go func() {
			defer wg.Done()
			time.Sleep(stopAfter / 3) // after the other took the connection
			errWaiting = r.Insert(waiting, "usertable", keyOn("b", slow), values[0])
			endWaiting = time.Now()
		}()
		time.Sleep(stopAfter)
		closeAt := time.Now().Add(stopGrace)
		stop()
		wg.Wait()
		t.Logf("max_redirects=%s: in flight ended %v after the close (%v), waiting for a turn %v (%v)",
			redirects, endInFlight.Sub(closeAt), errInFlight, endWaiting.Sub(closeAt), errWaiting)
		for name, end := range map[string]time.Time{"in flight": endInFlight, "waiting for a turn": endWaiting} {
			if over := end.Sub(closeAt); over > pastTheGrace {
				t.Errorf("max_redirects=%s: the operation %s ran %v past the grace", redirects, name, over)
			}
		}
		if errInFlight == nil || errWaiting == nil {
			t.Errorf("max_redirects=%s: an operation the grace ended succeeded", redirects)
		}
	}
}

// A dial in progress at the end of the grace (a server that doesn't answer
// the SYN): go-redis's pool close doesn't end it.
func TestStopEndsADialAtTheGrace(t *testing.T) {
	withGrace(t, 300*time.Millisecond)
	p := properties.NewProperties()
	p.Set("threadcount", "1")
	p.Set(redisDialTimeout, "30s")
	opts, err := getOptionsSingle(p)
	if err != nil {
		t.Fatal(err)
	}
	opts.Addr = singleAddr
	opts.Dialer = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done() // no SYN-ACK
		return nil, ctx.Err()
	}
	r := &redis{client: goredis.NewClient(opts), mode: "single", datatype: HASH_DATATYPE, fieldcount: 3}
	t.Cleanup(func() { r.Close() })
	keys, values := testRecords(1)
	run, stop := context.WithCancel(context.Background())
	ctx := r.InitThread(run, 0, 1)
	go func() { time.Sleep(stopAfter); stop() }()
	begin := time.Now()
	err = r.Insert(ctx, "usertable", keys[0], values[0])
	over := time.Since(begin) - stopAfter - stopGrace
	t.Logf("a dial in progress ended %v after the close: %v", over, err)
	if over > pastTheGrace {
		t.Errorf("a dial in progress ran %v past the grace", over)
	}
}

// A retry's back-off (large min/max retry backoffs) under way at the end of
// the grace: the master keeps dropping the connection, so the operation backs
// off before each new try.
func TestStopEndsABackoffAtTheGrace(t *testing.T) {
	withGrace(t, 300*time.Millisecond)
	slow := clusterSlots[1].Nodes[0].Addr
	r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, redisMaxRedirects, "3",
		redisMinRetryBackoff, "30s", redisMaxRetryBackoff, "30s")
	nodes.drop = func(addr string, _ []string) bool { return addr == slow }
	_, values := testRecords(1)
	run, stop := context.WithCancel(context.Background())
	ctx := r.InitThread(run, 0, 1)
	go func() { time.Sleep(stopAfter); stop() }()
	begin := time.Now()
	err := r.Insert(ctx, "usertable", keyOn("k", slow), values[0])
	over := time.Since(begin) - stopAfter - stopGrace
	t.Logf("an operation backing off ended %v after the close: %v", over, err)
	if over > pastTheGrace {
		t.Errorf("an operation backing off ran %v past the grace", over)
	}
	if err == nil {
		t.Error("the operation the grace ended succeeded")
	}
}

// One instance, two runs in one process (an embedder): the first run, ended
// normally after its threads, mustn't close the client during the second,
// and the second run's stop must be hooked.
func TestReuseAcrossRuns(t *testing.T) {
	withGrace(t, 100*time.Millisecond)
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	keys, values := testRecords(1)
	run1, end1 := context.WithCancel(context.Background())
	r.CleanupThread(r.InitThread(run1, 0, 1)) // as Client.Run does before returning
	end1()
	time.Sleep(3 * stopGrace) // run 1's end, its threads done, starts no grace
	run2, stop2 := context.WithCancel(context.Background())
	ctx2 := r.InitThread(run2, 0, 1)
	if err := r.Insert(ctx2, "usertable", keys[0], values[0]); err != nil {
		t.Fatalf("run 2's first insert: %v", err)
	}
	time.Sleep(3 * stopGrace)
	if err := r.Insert(ctx2, "usertable", keys[0], values[0]); err != nil {
		t.Fatalf("run 2, still running after run 1's grace: %v", err)
	}
	stop2()
	if !eventually(5*time.Second, func() bool { return clientClosed(r) }) {
		t.Error("run 2's stop didn't close the client at the end of its grace")
	}
}

// Concurrent InitThread, CleanupThread, the stop, the grace and Close: for
// the race detector.
func TestConcurrentLifecycle(t *testing.T) {
	withGrace(t, time.Millisecond)
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	run, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := r.InitThread(run, i, 64)
			_, _ = started(ctx)
			r.CleanupThread(ctx)
		}(i)
	}
	go stop()
	time.Sleep(time.Millisecond)
	go r.Close()
	wg.Wait()
	time.Sleep(5 * time.Millisecond)
	_ = r.Close()
}

// The grace a run waits is stopGrace as it was when the run's first thread
// started, not as it is at the stop.
func TestGraceTakenAtTheRunsStart(t *testing.T) {
	withGrace(t, 100*time.Millisecond)
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	run, stop := context.WithCancel(context.Background())
	ctx := r.InitThread(run, 0, 1)
	stopGrace = 5 * time.Second
	stop()
	// the 5 s grace would end well after eventually gives up
	if !eventually(2500*time.Millisecond, func() bool { return graceEnded(r, ctx) }) {
		t.Fatal("the grace taken at the run's start (100 ms) didn't end in 2.5 s")
	}
}

// Many threads of one run start at once: one stop hook, one grace, every
// thread counted (and, under -race, no race).
func TestManyThreadsOneRun(t *testing.T) {
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	run, stop := context.WithCancel(context.Background())
	defer stop()
	const n = 64
	ctxs := make([]context.Context, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctxs[i] = r.InitThread(run, i, n)
		}(i)
	}
	wg.Wait()
	r.mu.Lock()
	s := r.runs[run.Done()]
	threads, runs := s.threads, len(r.runs)
	r.mu.Unlock()
	if runs != 1 {
		t.Fatalf("%d run stops, want 1", runs)
	}
	if threads != n {
		t.Fatalf("%d threads counted, want %d", threads, n)
	}
	for i, ctx := range ctxs {
		if got := ctx.Value(threadKey{}).(*thread).run; got != s {
			t.Fatalf("thread %d is on another run's stop", i)
		}
	}
}

// An operation on a thread's own context allocates no context: it gets the
// one InitThread made.
func TestStartedAllocatesNothing(t *testing.T) {
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	tctx := r.InitThread(context.Background(), 0, 1)
	defer r.CleanupThread(tctx)
	if allocs := testing.AllocsPerRun(100, func() { _, _ = started(tctx) }); allocs != 0 {
		t.Fatalf("started on the thread's context: %v allocations, want 0", allocs)
	}
	sent, _ := started(tctx)
	if _, ok := sent.(*opContext); !ok {
		t.Fatalf("started gave %T, want the thread's *opContext", sent)
	}
}

// Threads of several runs on one instance (an embedder), started
// interleaved, and threads whose contexts are their own children of one run:
// a run's stop is hooked whatever the others' threads, and ends its
// operations at its grace. Each case on an instance of its own: the first
// grace closes the shared client for every run on it (see
// TestStopEndsEveryRunOnTheInstance).
func TestRunsInterleavedOnOneInstance(t *testing.T) {
	withGrace(t, 200*time.Millisecond)
	keys, values := testRecords(1)
	for _, target := range []string{"B, started between A's threads", "C1, a child of run C"} {
		p := properties.NewProperties()
		p.Set("threadcount", "4")
		p.Set(redisDialTimeout, "30s")
		opts, err := getOptionsSingle(p)
		if err != nil {
			t.Fatal(err)
		}
		opts.Addr = singleAddr
		opts.Dialer = func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done() // no SYN-ACK: every operation waits on a dial
			return nil, ctx.Err()
		}
		r := &redis{client: goredis.NewClient(opts), mode: "single", datatype: HASH_DATATYPE, fieldcount: 3}

		runA, stopA := context.WithCancel(context.Background())
		defer stopA()
		runB, stopB := context.WithCancel(context.Background())
		defer stopB()
		threadA1 := r.InitThread(runA, 0, 2)
		threadB := r.InitThread(runB, 0, 1)
		threadA2 := r.InitThread(runA, 1, 2) // A, B, A
		// a run whose threads each get a child context of it
		runC, stopC := context.WithCancel(context.Background())
		defer stopC()
		childC1, cancelC1 := context.WithCancel(runC)
		childC2, cancelC2 := context.WithCancel(runC)
		threadC1, threadC2 := r.InitThread(childC1, 0, 2), r.InitThread(childC2, 1, 2)

		ctx, stop := threadB, stopB
		if target != "B, started between A's threads" {
			ctx, stop = threadC1, stopC
		}
		begin := time.Now()
		go func() { time.Sleep(stopAfter); stop() }()
		err = r.Insert(ctx, "usertable", keys[0], values[0])
		if over := time.Since(begin) - stopAfter - stopGrace; over > pastTheGrace {
			t.Errorf("%s: its operation ran %v past its run's grace (%v)", target, over, err)
		}
		if err == nil {
			t.Errorf("%s: the operation the grace ended succeeded", target)
		}
		_, _, _ = threadA1, threadA2, threadC2
		stopA()
		cancelC1()
		cancelC2()
		r.Close()
	}
}

// The deliberate limit: every run on an instance shares its client, so the
// end of one run's grace closes it for all of them. Run A, never stopped,
// works until run B's grace ends, and then fails ("redis: client is closed").
func TestStopEndsEveryRunOnTheInstance(t *testing.T) {
	withGrace(t, 100*time.Millisecond)
	r, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	keys, values := testRecords(1)
	runA, stopA := context.WithCancel(context.Background())
	defer stopA()
	runB, stopB := context.WithCancel(context.Background())
	threadA, threadB := r.InitThread(runA, 0, 1), r.InitThread(runB, 0, 1)
	defer r.CleanupThread(threadA)
	if err := r.Insert(threadA, "usertable", keys[0], values[0]); err != nil {
		t.Fatalf("run A before B's stop: %v", err)
	}
	stopB()
	if !eventually(5*time.Second, func() bool { return graceEnded(r, threadB) }) {
		t.Fatal("run B's grace didn't end")
	}
	if err := r.Insert(threadA, "usertable", keys[0], values[0]); !errors.Is(err, goredis.ErrClosed) {
		t.Fatalf("run A after B's grace: %v, want %v", err, goredis.ErrClosed)
	}
	if err := runA.Err(); err != nil {
		t.Fatalf("run A was stopped: %v", err)
	}
}

// slowCloseClient's Close blocks, as go-redis's can (it waits for its
// maintenance notifications handler, when they are on).
type slowCloseClient struct {
	*goredis.Client
}

func (c slowCloseClient) Close() error {
	time.Sleep(10 * time.Second)
	return c.Client.Close()
}

// The cancel at the end of the grace doesn't wait for a Close that blocks.
func TestCancelDoesNotWaitForClose(t *testing.T) {
	withGrace(t, 100*time.Millisecond)
	fake, _ := newFakeRedis(t, "single", HASH_DATATYPE)
	r := &redis{client: slowCloseClient{fake.client.(*goredis.Client)}, mode: "single", datatype: HASH_DATATYPE, fieldcount: 3}
	run, stop := context.WithCancel(context.Background())
	ctx := r.InitThread(run, 0, 1)
	sent, _ := started(ctx)
	begin := time.Now()
	stop()
	select {
	case <-sent.Done():
		if took := time.Since(begin); took > stopGrace+closeToCancel+pastTheGrace {
			t.Errorf("the operations' context was canceled %v after the stop, want about %v", took, stopGrace+closeToCancel)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the operations' context waited for the blocking Close")
	}
}

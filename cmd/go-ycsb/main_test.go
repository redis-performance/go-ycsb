package main

import (
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// stopHarness drives waitStop step by step, without sleeps: the signal channel
// is unbuffered, so a send returns only once waitStop has taken the signal, and
// the fake clock reports each reading, so the test knows when waitStop has
// looked at the time for that signal.
type stopHarness struct {
	sc        chan os.Signal
	closeDone chan struct{}
	force     chan time.Time
	forceFor  chan time.Duration // what waitStop asked the force-exit timer for
	clock     atomic.Int64       // fake time since the first signal, in ns
	read      chan struct{}      // one per clock reading
	exited    chan int
	done      chan struct{}
}

func startStop(t *testing.T, forceAfter time.Duration) *stopHarness {
	t.Helper()
	_, globalCancel = context.WithCancel(context.Background())
	h := &stopHarness{
		sc:        make(chan os.Signal),
		closeDone: make(chan struct{}, 1),
		force:     make(chan time.Time),
		forceFor:  make(chan time.Duration, 1),
		read:      make(chan struct{}, 8),
		exited:    make(chan int, 1),
		done:      make(chan struct{}),
	}
	now := func() time.Time {
		v := time.Unix(0, h.clock.Load())
		h.read <- struct{}{}
		return v
	}
	after := func(d time.Duration) <-chan time.Time {
		h.forceFor <- d
		return h.force
	}
	go func() {
		waitStop(h.sc, h.closeDone, forceAfter, now, after, func(code int) { h.exited <- code })
		close(h.done)
	}()
	return h
}

// signal delivers one signal at fake time at and waits until waitStop has read
// the clock for it.
func (h *stopHarness) signal(t *testing.T, at time.Duration) {
	t.Helper()
	h.clock.Store(int64(at))
	select {
	case h.sc <- syscall.SIGINT:
	case <-h.done:
		t.Fatalf("waitStop returned before the signal at %v", at)
	case <-time.After(5 * time.Second):
		t.Fatalf("waitStop did not take the signal at %v", at)
	}
	select {
	case <-h.read:
	case <-time.After(5 * time.Second):
		t.Fatal("waitStop did not read the clock for the signal")
	}
}

func (h *stopHarness) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitStop did not return")
	}
}

// waitStop under a fake clock: what each signal sequence does.
func TestWaitStop(t *testing.T) {
	for _, c := range []struct {
		name     string
		gaps     []time.Duration // after the first signal, when each later one arrives
		wantExit bool
	}{
		{name: "one signal: wait for the run to close", wantExit: false},
		{name: "the same stop delivered twice (timeout -s INT)", gaps: []time.Duration{0}, wantExit: false},
		{name: "delivered twice, within the window", gaps: []time.Duration{999 * time.Millisecond}, wantExit: false},
		{name: "a second Ctrl-C after the window", gaps: []time.Duration{1500 * time.Millisecond}, wantExit: true},
		{name: "duplicate, then a later signal", gaps: []time.Duration{0, 2 * time.Second}, wantExit: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := startStop(t, time.Hour)
			h.signal(t, 0)
			for _, g := range c.gaps {
				h.signal(t, g)
			}
			if c.wantExit {
				h.wait(t) // an exit returns without waiting for the run
				if code := <-h.exited; code != 1 {
					t.Fatalf("exited with %d, want 1", code)
				}
				return
			}
			h.closeDone <- struct{}{} // the run closed: waitStop returns
			h.wait(t)
			select {
			case code := <-h.exited:
				t.Fatalf("exited with %d, want no exit", code)
			default:
			}
		})
	}
}

// The run doesn't close within forceAfter: waitStop forces the exit and says
// how long it waited.
func TestWaitStopForceExit(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	h := startStop(t, 7*time.Second)
	h.signal(t, 0)
	if d := <-h.forceFor; d != 7*time.Second {
		t.Errorf("force-exit timer for %v, want 7s", d)
	}
	h.force <- time.Now()
	h.wait(t)
	if code := <-h.exited; code != 1 {
		t.Errorf("exited with %d, want 1", code)
	}
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "Wait 7s for closed, force exit") {
		t.Errorf("output %q does not say how long it waited", out)
	}
}

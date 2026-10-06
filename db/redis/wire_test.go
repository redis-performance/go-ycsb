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
	"testing"
	"time"
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
		names := nodes.sentNames()
		for cmd, want := range c.want {
			if got := names[cmd] > 0; got != want {
				t.Errorf("%s %v: %q sent %d times, want sent: %v (all: %v)", c.mode, c.props, cmd, names[cmd], want, names)
			}
		}
	}
}

// The slot map is reloaded every redis.cluster_state_reload_interval (10 s
// by default, as go-redis v9.8.0 did).
func TestClusterStateReloadCadence(t *testing.T) {
	ctx := context.Background()
	keys, values := testRecords(5)
	for _, c := range []struct {
		interval string
		min, max int32
	}{{"", 1, 1}, {"100ms", 3, 20}} {
		var props []string
		if c.interval != "" {
			props = []string{redisClusterStateReloadInterval, c.interval}
		}
		r, nodes := newFakeRedis(t, "cluster", HASH_DATATYPE, props...)
		deadline := time.Now().Add(700 * time.Millisecond)
		for time.Now().Before(deadline) {
			for i := range keys {
				if err := r.Insert(ctx, "usertable", keys[i], values[i]); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if got := nodes.slotLoads.Load(); got < c.min || got > c.max {
			t.Errorf("interval %q: %d slot map loads in 0.7 s, want %d..%d", c.interval, got, c.min, c.max)
		}
	}
}

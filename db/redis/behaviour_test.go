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
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/magiconair/properties"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// By default the client behaves as go-redis v9.8.0 did: RESP3, no
// maintenance notifications, 4 KiB buffers, one dial attempt, no
// concurrent-dial limit, and in cluster mode no routing policies.
func TestClientBehaviourDefaults(t *testing.T) {
	p := properties.NewProperties()
	p.Set("threadcount", "8")
	single, err := getOptionsSingle(p)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := getOptionsCluster(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []struct {
		mode                              string
		protocol, rbuf, wbuf, dials, conc int
		maint                             *maintnotifications.Config
	}{
		{"single", single.Protocol, single.ReadBufferSize, single.WriteBufferSize, single.DialerRetries, single.MaxConcurrentDials, single.MaintNotificationsConfig},
		{"cluster", cluster.Protocol, cluster.ReadBufferSize, cluster.WriteBufferSize, cluster.DialerRetries, cluster.MaxConcurrentDials, cluster.MaintNotificationsConfig},
	} {
		if o.protocol != 3 || o.rbuf != 4096 || o.wbuf != 4096 || o.dials != 1 || o.conc != 0 {
			t.Errorf("%s: protocol %d, buffers %d/%d, dialer retries %d, max concurrent dials %d; want 3, 4096/4096, 1, 0",
				o.mode, o.protocol, o.rbuf, o.wbuf, o.dials, o.conc)
		}
		if o.maint == nil || o.maint.Mode != maintnotifications.ModeDisabled {
			t.Errorf("%s: maintenance notifications %+v, want disabled", o.mode, o.maint)
		}
	}
	if !cluster.DisableRoutingPolicies {
		t.Error("cluster: routing policies on by default")
	}
	if cluster.ClusterStateReloadInterval != 10*time.Second {
		t.Errorf("cluster: slots reloaded every %v, want 10s", cluster.ClusterStateReloadInterval)
	}
	for _, b := range [][2]time.Duration{{single.MinRetryBackoff, single.MaxRetryBackoff}, {cluster.MinRetryBackoff, cluster.MaxRetryBackoff}} {
		if b != [2]time.Duration{8 * time.Millisecond, 512 * time.Millisecond} {
			t.Errorf("retry backoffs %v, want 8ms..512ms", b)
		}
	}
}

// An explicit 0 backoff is v9.8.0's default too, not v9.22.0's.
func TestRetryBackoffZero(t *testing.T) {
	p := properties.NewProperties()
	p.Set("threadcount", "8")
	p.Set(redisMinRetryBackoff, "0")
	p.Set(redisMaxRetryBackoff, "0")
	o, err := getOptionsCluster(p)
	if err != nil {
		t.Fatal(err)
	}
	if o.MinRetryBackoff != 8*time.Millisecond || o.MaxRetryBackoff != 512*time.Millisecond {
		t.Errorf("backoffs %v..%v, want 8ms..512ms", o.MinRetryBackoff, o.MaxRetryBackoff)
	}
	p.Set(redisMinRetryBackoff, "-1")
	if so, _ := getOptionsSingle(p); so.MinRetryBackoff != -1 {
		t.Errorf("min backoff -1: %v, want -1 (no backoff)", so.MinRetryBackoff)
	}
}

func TestClusterStateReloadInterval(t *testing.T) {
	p := properties.NewProperties()
	p.Set("threadcount", "8")
	p.Set(redisClusterStateReloadInterval, "1m")
	if o, err := getOptionsCluster(p); err != nil || o.ClusterStateReloadInterval != time.Minute {
		t.Errorf("1m: %v, %v", o, err)
	}
	for _, bad := range []string{"0", "-1s", "soon"} {
		p.Set(redisClusterStateReloadInterval, bad)
		if _, err := getOptionsCluster(p); err == nil {
			t.Errorf("%s=%s accepted", redisClusterStateReloadInterval, bad)
		}
	}
}

func TestClientBehaviourProperties(t *testing.T) {
	p := properties.NewProperties()
	p.Set("threadcount", "8")
	p.Set(redisProtocol, "2")
	p.Set(redisMaintNotifications, "auto")
	p.Set(redisReadBufferSize, "32768")
	p.Set(redisWriteBufferSize, "65536")
	p.Set(redisDialerRetries, "5")
	p.Set(redisMaxConcurrentDials, "3")
	p.Set(redisRoutingPolicies, "true")
	o, err := getOptionsCluster(p)
	if err != nil {
		t.Fatal(err)
	}
	if o.Protocol != 2 || o.MaintNotificationsConfig.Mode != maintnotifications.ModeAuto || o.ReadBufferSize != 32768 ||
		o.WriteBufferSize != 65536 || o.DialerRetries != 5 || o.MaxConcurrentDials != 3 || o.DisableRoutingPolicies {
		t.Errorf("options %+v don't follow the properties", o)
	}
	so, err := getOptionsSingle(p)
	if err != nil {
		t.Fatal(err)
	}
	if so.Protocol != 2 || so.MaintNotificationsConfig.Mode != maintnotifications.ModeAuto || so.ReadBufferSize != 32768 ||
		so.WriteBufferSize != 65536 || so.DialerRetries != 5 || so.MaxConcurrentDials != 3 {
		t.Errorf("single options %+v don't follow the properties", so)
	}
	q := properties.NewProperties()
	q.Set("threadcount", "8")
	q.Set(redisProtocol, "2")
	q.Set(redisMaintNotifications, "enabled")
	if _, err := getOptionsSingle(q); err == nil {
		t.Errorf("%s=enabled with %s=2 accepted", redisMaintNotifications, redisProtocol)
	}
	if _, err := getOptionsCluster(q); err == nil {
		t.Errorf("cluster: %s=enabled with %s=2 accepted", redisMaintNotifications, redisProtocol)
	}
	for key, bad := range map[string]string{
		redisProtocol: "4", redisMaintNotifications: "on", redisReadBufferSize: "0",
		redisWriteBufferSize: "0", redisDialerRetries: "0", redisMaxConcurrentDials: "-1",
	} {
		q := properties.NewProperties()
		q.Set("threadcount", "8")
		q.Set(key, bad)
		if _, err := getOptionsSingle(q); err == nil {
			t.Errorf("%s=%s accepted", key, bad)
		}
		if _, err := getOptionsCluster(q); err == nil {
			t.Errorf("cluster: %s=%s accepted", key, bad)
		}
	}
}

// redis.routing_policies does nothing in single mode, and says so.
func TestRoutingPoliciesSingleModeWarning(t *testing.T) {
	out := func(props ...string) string {
		p := properties.NewProperties()
		p.Set("threadcount", "4")
		for i := 0; i+1 < len(props); i += 2 {
			p.Set(props[i], props[i+1])
		}
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := os.Stdout
		os.Stdout = w
		_, optErr := getOptionsSingle(p)
		os.Stdout = orig
		w.Close()
		b, _ := io.ReadAll(r)
		if optErr != nil {
			t.Fatal(optErr)
		}
		return string(b)
	}
	if got := out(redisRoutingPolicies, "true"); !strings.Contains(got, redisRoutingPolicies+" has no effect in single mode") {
		t.Errorf("no warning for %s in single mode: %q", redisRoutingPolicies, got)
	}
	if got := out(); strings.Contains(got, redisRoutingPolicies) {
		t.Errorf("a warning without the property: %q", got)
	}
}

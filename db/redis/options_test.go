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
	"strings"
	"testing"
	"time"

	"github.com/magiconair/properties"
)

// Duration properties take a Go duration or, as before, integer nanoseconds;
// anything else fails rather than falling back to the default in silence.
func TestDurationProperties(t *testing.T) {
	for _, c := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{
		{value: "", want: 3 * time.Second},   // unset: the default
		{value: "  ", want: 3 * time.Second}, // blank: the default
		{value: " 30s ", want: 30 * time.Second},
		{value: "30s", want: 30 * time.Second},
		{value: "1m30s", want: 90 * time.Second},
		{value: "500ms", want: 500 * time.Millisecond},
		{value: "30000000000", want: 30 * time.Second},
		{value: "0", want: 3 * time.Second}, // go-redis's default: v9.8.0's, not v9.22.0's 5 s
		{value: "-1", want: -1},
		{value: "-2", want: -2},
		{value: "30", want: 30}, // nanoseconds, as before
		{value: "thirty", bad: true},
		{value: "30 s", bad: true},
	} {
		for _, mode := range []string{"single", "cluster"} {
			p := properties.NewProperties()
			p.Set("threadcount", "4")
			if c.value != "" {
				p.Set(redisReadTimeout, c.value)
			}
			var got time.Duration
			var err error
			if mode == "single" {
				o, e := getOptionsSingle(p)
				err = e
				if o != nil {
					got = o.ReadTimeout
				}
			} else {
				o, e := getOptionsCluster(p)
				err = e
				if o != nil {
					got = o.ReadTimeout
				}
			}
			if c.bad {
				if err == nil || !strings.Contains(err.Error(), redisReadTimeout) {
					t.Errorf("%s %s=%q: err %v, want one naming the property", mode, redisReadTimeout, c.value, err)
				}
				continue
			}
			if err != nil || got != c.want {
				t.Errorf("%s %s=%q: %v, %v, want %v", mode, redisReadTimeout, c.value, got, err, c.want)
			}
		}
	}
}

// Every duration property goes through the same parsing.
func TestAllDurationProperties(t *testing.T) {
	keys := []string{redisMinRetryBackoff, redisMaxRetryBackoff, redisDialTimeout, redisReadTimeout,
		redisWriteTimeout, redisMaxConnAge, redisPoolTimeout, redisIdleTimeout}
	for _, key := range keys {
		p := properties.NewProperties()
		p.Set("threadcount", "4")
		p.Set(key, "7s")
		o, err := getOptionsCluster(p)
		if err != nil {
			t.Fatalf("%s=7s: %v", key, err)
		}
		got := map[string]time.Duration{
			redisMinRetryBackoff: o.MinRetryBackoff, redisMaxRetryBackoff: o.MaxRetryBackoff,
			redisDialTimeout: o.DialTimeout, redisReadTimeout: o.ReadTimeout, redisWriteTimeout: o.WriteTimeout,
			redisMaxConnAge: o.ConnMaxLifetime, redisPoolTimeout: o.PoolTimeout, redisIdleTimeout: o.ConnMaxIdleTime,
		}[key]
		if got != 7*time.Second {
			t.Errorf("%s=7s: %v", key, got)
		}
		p.Set(key, "7 seconds")
		if _, err := getOptionsSingle(p); err == nil {
			t.Errorf("%s=%q accepted", key, "7 seconds")
		}
	}
}

// Every integer and boolean property fails on a value that doesn't parse,
// naming it, in both modes, rather than falling back to its default.
func TestIntBoolPropertiesStrict(t *testing.T) {
	ints := []string{redisMaxRetries, redisPoolSize, redisMinIdleConns, redisMaxIdleConns,
		redisProtocol, redisReadBufferSize, redisWriteBufferSize, redisDialerRetries, redisMaxConcurrentDials}
	clusterInts := []string{redisMaxRedirects}
	bools := []string{redisRoutingPolicies, redisTLSInsecureSkipVerify}
	clusterBools := []string{redisReadOnly, redisRouteByLatency, redisRouteRandomly}
	check := func(mode, key, bad string) {
		p := properties.NewProperties()
		p.Set("threadcount", "4")
		p.Set(key, bad)
		var err error
		if mode == "single" {
			_, err = getOptionsSingle(p)
		} else {
			_, err = getOptionsCluster(p)
		}
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s %s=%q: err %v, want one naming it", mode, key, bad, err)
		}
	}
	for _, mode := range []string{"single", "cluster"} {
		for _, key := range ints {
			check(mode, key, "five")
			check(mode, key, "32k")
		}
		for _, key := range bools {
			check(mode, key, "enabled")
		}
	}
	for _, key := range clusterInts {
		check("cluster", key, "three")
	}
	check("single", redisDB, "zero")
	for _, key := range clusterBools {
		check("cluster", key, "maybe")
	}
}

func TestIntBoolPropertyForms(t *testing.T) {
	r := &propReader{p: properties.NewProperties()}
	r.p.Set("a", " 42 ")
	r.p.Set("b", "")
	r.p.Set("c", "-1")
	if r.int("a", 0) != 42 || r.int("b", 7) != 7 || r.int("c", 0) != -1 || r.int("missing", 9) != 9 {
		t.Error("int forms")
	}
	for v, want := range map[string]bool{"1": true, "true": true, "Yes": true, "ON": true, "0": false, "false": false, "no": false, "Off": false} {
		r.p.Set("x", v)
		if got := r.bool("x", !want); got != want {
			t.Errorf("bool %q = %v", v, got)
		}
	}
	r.p.Set("x", " ")
	if !r.bool("x", true) {
		t.Error("blank bool isn't the default")
	}
	if r.err != nil {
		t.Errorf("good values failed: %v", r.err)
	}
}

// Defaults of the durations: write_timeout follows read_timeout, pool_timeout
// is read_timeout + 1 s, max_conn_age and idle_timeout -1.
func TestDurationDefaults(t *testing.T) {
	p := properties.NewProperties()
	p.Set("threadcount", "4")
	p.Set(redisReadTimeout, " 7s ")
	for _, mode := range []string{"single", "cluster"} {
		var read, write, pool, age, idle time.Duration
		if mode == "single" {
			o, err := getOptionsSingle(p)
			if err != nil {
				t.Fatal(err)
			}
			read, write, pool, age, idle = o.ReadTimeout, o.WriteTimeout, o.PoolTimeout, o.ConnMaxLifetime, o.ConnMaxIdleTime
		} else {
			o, err := getOptionsCluster(p)
			if err != nil {
				t.Fatal(err)
			}
			read, write, pool, age, idle = o.ReadTimeout, o.WriteTimeout, o.PoolTimeout, o.ConnMaxLifetime, o.ConnMaxIdleTime
		}
		if read != 7*time.Second || write != read || pool != 8*time.Second || age != -1 || idle != -1 {
			t.Errorf("%s: read %v write %v pool %v age %v idle %v", mode, read, write, pool, age, idle)
		}
	}
}

// Every valid integer and boolean value reaches go-redis's options.
func TestIntBoolPropertiesReachOptions(t *testing.T) {
	p := properties.NewProperties()
	p.Set("threadcount", "4")
	p.Set(redisDB, "5")
	p.Set(redisMaxRetries, "-1")
	p.Set(redisPoolSize, "17")
	p.Set(redisMinIdleConns, "3")
	p.Set(redisMaxIdleConns, "9")
	p.Set(redisMaxRedirects, "1")
	p.Set(redisReadOnly, "yes")
	p.Set(redisRouteByLatency, "on")
	p.Set(redisRouteRandomly, "1")
	p.Set(redisMaintNotifications, " Auto ")
	s, err := getOptionsSingle(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.DB != 5 || s.MaxRetries != -1 || s.PoolSize != 17 || s.MinIdleConns != 3 || s.MaxIdleConns != 9 ||
		string(s.MaintNotificationsConfig.Mode) != "auto" {
		t.Errorf("single: db %d retries %d pool %d idle %d..%d maint %q", s.DB, s.MaxRetries, s.PoolSize,
			s.MinIdleConns, s.MaxIdleConns, s.MaintNotificationsConfig.Mode)
	}
	c, err := getOptionsCluster(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRetries != -1 || c.PoolSize != 17 || c.MinIdleConns != 3 || c.MaxIdleConns != 9 || c.MaxRedirects != 1 ||
		!c.ReadOnly || !c.RouteByLatency || !c.RouteRandomly {
		t.Errorf("cluster: retries %d pool %d idle %d..%d redirects %d read-only %v by-latency %v randomly %v",
			c.MaxRetries, c.PoolSize, c.MinIdleConns, c.MaxIdleConns, c.MaxRedirects, c.ReadOnly, c.RouteByLatency, c.RouteRandomly)
	}
	p.Set(redisReadOnly, "off")
	if c, err = getOptionsCluster(p); err != nil {
		t.Fatal(err)
	}
	if c.ReadOnly {
		t.Error("read_only=off: read-only")
	}
}

// A retry count below -1 would make go-redis run no try at all, every
// command "succeeding" unsent: rejected.
func TestRetriesBelowMinusOne(t *testing.T) {
	for _, c := range []struct{ mode, key string }{
		{"single", redisMaxRetries}, {"cluster", redisMaxRetries}, {"cluster", redisMaxRedirects},
	} {
		for _, v := range []string{"-2", "-10"} {
			p := properties.NewProperties()
			p.Set("threadcount", "4")
			p.Set(c.key, v)
			var err error
			if c.mode == "single" {
				_, err = getOptionsSingle(p)
			} else {
				_, err = getOptionsCluster(p)
			}
			if err == nil || !strings.Contains(err.Error(), c.key) {
				t.Errorf("%s %s=%s: err %v, want one naming it", c.mode, c.key, v, err)
			}
		}
		p := properties.NewProperties()
		p.Set("threadcount", "4")
		p.Set(c.key, "-1")
		if _, err := getOptionsCluster(p); err != nil {
			t.Errorf("%s=-1 (no retries) rejected: %v", c.key, err)
		}
	}
}

// With several bad properties the error names the first one read: within
// the options (a duration before a later integer), then the client
// behaviour properties.
func TestFirstBadPropertyReported(t *testing.T) {
	for _, c := range []struct {
		bad  [][2]string
		want string
	}{
		{[][2]string{{redisReadTimeout, "soon"}, {redisPoolSize, "many"}}, redisReadTimeout},
		{[][2]string{{redisPoolSize, "many"}, {redisMaxIdleConns, "lots"}}, redisPoolSize},
		{[][2]string{{redisProtocol, "resp2"}, {redisPoolSize, "many"}}, redisPoolSize},
	} {
		for _, mode := range []string{"single", "cluster"} {
			p := properties.NewProperties()
			p.Set("threadcount", "4")
			for _, kv := range c.bad {
				p.Set(kv[0], kv[1])
			}
			var err error
			if mode == "single" {
				_, err = getOptionsSingle(p)
			} else {
				_, err = getOptionsCluster(p)
			}
			if err == nil || !strings.HasPrefix(err.Error(), c.want+"=") {
				t.Errorf("%s %v: err %v, want the first, %s", mode, c.bad, err, c.want)
			}
		}
	}
}

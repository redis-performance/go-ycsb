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

package measurement

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Per-endpoint latency (measurement.prometheus_endpoints): a DB binding times
// each request it sends to one server endpoint (a node of CLUSTER NODES, say)
// and records it here with MeasureEndpoint. These are network round trips to
// one endpoint, not the operations the summary counts: an operation that
// redirects or retries is several of them, and client queueing is not in
// them. They are exported on their own series (ycsb_endpoint_*) and never
// enter the summary, the interval output or ycsb_latency_seconds.

// EndpointInfo describes one endpoint as the server reports it.
type EndpointInfo struct {
	Endpoint string // host:port, as the client dials it
	NodeID   string
	Role     string // "master" or "replica"
	Shard    string // the node ID of the master the endpoint belongs to
}

// endpointKey is one endpoint's series for one operation; op carries the
// outcome suffix (READ, READ_ERROR, READ_REDIRECT).
type endpointKey struct {
	endpoint string
	op       string
}

// endpointHist is lock-free: every request thread records into it at once.
// The last bucket is +Inf.
type endpointHist struct {
	buckets []atomic.Uint64
	sumNs   atomic.Int64
}

func newEndpointHist() *endpointHist {
	return &endpointHist{buckets: make([]atomic.Uint64, len(promLatencyBucketsUs)+1)}
}

func (h *endpointHist) record(lan time.Duration) {
	i := sort.Search(len(promLatencyBucketsUs), func(i int) bool {
		return lan <= time.Duration(promLatencyBucketsUs[i])*time.Microsecond
	})
	h.buckets[i].Add(1)
	h.sumNs.Add(int64(lan))
}

type endpointStats struct {
	mu    sync.RWMutex
	hists map[endpointKey]*endpointHist
	info  []EndpointInfo
}

// endpoints is nil unless measurement.prometheus_endpoints is on; set by
// InitMeasure before any DB is created.
var endpoints atomic.Pointer[endpointStats]

// EndpointsEnabled says whether a binding should time requests per endpoint.
func EndpointsEnabled() bool {
	return endpoints.Load() != nil
}

type endpointOpKey struct{}

// WithEndpointOp labels the requests sent on ctx with the operation they are
// part of (READ, INSERT, BATCH_INSERT, ...). A request without one, such as a
// client's own topology refresh, is not recorded.
func WithEndpointOp(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, endpointOpKey{}, op)
}

// EndpointOp returns the operation WithEndpointOp put on ctx.
func EndpointOp(ctx context.Context) (string, bool) {
	op, ok := ctx.Value(endpointOpKey{}).(string)
	return op, ok && op != ""
}

// Endpoint outcomes, appended to the operation's name.
const (
	EndpointOK       = ""
	EndpointError    = "_ERROR"
	EndpointRedirect = "_REDIRECT"
)

// MeasureEndpoint records one request of op to endpoint that took lan, with
// its outcome (EndpointOK, EndpointError or EndpointRedirect). Like the
// operations, nothing is recorded during a warm-up.
func MeasureEndpoint(endpoint, op, outcome string, lan time.Duration) {
	s := endpoints.Load()
	if s == nil || !IsWarmUpFinished() {
		return
	}
	k := endpointKey{endpoint, op + outcome}
	s.mu.RLock()
	h, ok := s.hists[k]
	s.mu.RUnlock()
	if !ok {
		s.mu.Lock()
		if h, ok = s.hists[k]; !ok {
			h = newEndpointHist()
			s.hists[k] = h
		}
		s.mu.Unlock()
	}
	h.record(lan)
}

// SetEndpointInfo replaces what is known of the endpoints (the last CLUSTER
// NODES reply, say).
func SetEndpointInfo(info []EndpointInfo) {
	s := endpoints.Load()
	if s == nil {
		return
	}
	info = append([]EndpointInfo(nil), info...)
	s.mu.Lock()
	s.info = info
	s.mu.Unlock()
}

// endpointSample is one series' cumulative histogram at a scrape.
type endpointSample struct {
	endpoint, op string
	count        uint64
	sum          float64
	buckets      map[float64]uint64
}

func (s *endpointStats) snapshot() ([]endpointSample, []EndpointInfo) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	samples := make([]endpointSample, 0, len(s.hists))
	for k, h := range s.hists {
		// the count is the buckets' total, read once, so that it is never
		// below the last finite bucket's cumulative count
		buckets := make(map[float64]uint64, len(promLatencyBucketsUs))
		var cumulative uint64
		for i, upperUs := range promLatencyBucketsUs {
			cumulative += h.buckets[i].Load()
			buckets[float64(upperUs)/1e6] = cumulative
		}
		cumulative += h.buckets[len(promLatencyBucketsUs)].Load()
		samples = append(samples, endpointSample{k.endpoint, k.op, cumulative,
			float64(h.sumNs.Load()) / 1e9, buckets})
	}
	return samples, s.info
}

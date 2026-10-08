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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Per-endpoint latency (measurement.prometheus_endpoints): a DB binding times
// each request it sends to one server endpoint (a cluster node the client
// dials, say) and records it here with MeasureEndpoint. These are requests to
// one endpoint, not the operations the summary counts: an operation that is
// redirected is several of them, and the client's queueing for a thread is
// not in them (see the binding for what one request covers). They are
// exported on their own series (ycsb_endpoint_*) and never enter the summary,
// the interval output or ycsb_latency_seconds.

// EndpointInfo describes one endpoint as the server reports it.
type EndpointInfo struct {
	Endpoint string // host:port, as the client dials it
	NodeID   string
	Role     string // "master", "replica" or "unknown"
	Shard    string // the node ID of the master the endpoint belongs to
}

// endpointKey is one endpoint's series for one operation and outcome
// (EndpointOK, EndpointError, ...): kept apart, so recording a failure
// builds no string.
type endpointKey struct {
	endpoint, op, outcome string
}

// The last bucket is +Inf.
type endpointHist struct {
	buckets []atomic.Uint64
	sumNs   atomic.Int64
}

func newEndpointHist() *endpointHist {
	return &endpointHist{buckets: make([]atomic.Uint64, len(promLatencyBucketsUs)+1)}
}

// endpointBounds are promLatencyBucketsUs as durations, for record.
var endpointBounds = func() []time.Duration {
	b := make([]time.Duration, len(promLatencyBucketsUs))
	for i, us := range promLatencyBucketsUs {
		b[i] = time.Duration(us) * time.Microsecond
	}
	return b
}()

func (h *endpointHist) record(lan time.Duration) {
	i, _ := slices.BinarySearch(endpointBounds, lan) // the first bound >= lan
	h.buckets[i].Add(1)
	h.sumNs.Add(int64(lan))
}

// endpointStats: two levels of copy-on-write maps, each read with one atomic
// load per request (a read lock's shared counter would be written by every
// request thread): the endpoints, then each endpoint's few operation and
// outcome series. A new endpoint copies the first, a new series of an
// endpoint only that endpoint's; both are rare after a run's first seconds.
type endpointStats struct {
	eps atomic.Pointer[map[string]*endpointSeries]
	mu  sync.Mutex // guards writes of eps, and info
	// overflow counts the endpoints past maxEndpoints, recorded as
	// otherEndpoint: the server names them (redirect targets, topology
	// changes), so their number is not the client's to bound.
	overflow atomic.Int64
	seen     map[string]bool // the endpoints past maxEndpoints, counted once
	info     []EndpointInfo
}

// endpointSeries are one endpoint's series, by operation and outcome.
type endpointSeries struct {
	hists atomic.Pointer[map[opOutcome]*endpointHist]
	mu    sync.Mutex // guards writes of hists
}

type opOutcome struct{ op, outcome string }

// maxEndpoints bounds the endpoints with series of their own; requests to
// endpoints past it are recorded under otherEndpoint.
const (
	maxEndpoints  = 1024
	otherEndpoint = "other"
)

func newEndpointStats() *endpointStats {
	s := &endpointStats{seen: map[string]bool{}}
	s.eps.Store(&map[string]*endpointSeries{})
	return s
}

func newEndpointSeries() *endpointSeries {
	e := &endpointSeries{}
	e.hists.Store(&map[opOutcome]*endpointHist{})
	return e
}

// series returns endpoint's series, created if new.
func (s *endpointStats) series(endpoint string) *endpointSeries {
	if e, ok := (*s.eps.Load())[endpoint]; ok {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := *s.eps.Load()
	if e, ok := old[endpoint]; ok {
		return e
	}
	if len(old) >= maxEndpoints {
		if !s.seen[endpoint] {
			s.seen[endpoint] = true
			s.overflow.Add(1)
		}
		if e, ok := old[otherEndpoint]; ok {
			return e
		}
		endpoint = otherEndpoint
	}
	m := make(map[string]*endpointSeries, len(old)+1)
	for k, v := range old {
		m[k] = v
	}
	e := newEndpointSeries()
	m[endpoint] = e
	s.eps.Store(&m)
	return e
}

// hist returns the histogram of op with outcome, created if new.
func (e *endpointSeries) hist(k opOutcome) *endpointHist {
	if h, ok := (*e.hists.Load())[k]; ok {
		return h
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	old := *e.hists.Load()
	if h, ok := old[k]; ok {
		return h
	}
	m := make(map[opOutcome]*endpointHist, len(old)+1)
	for k, v := range old {
		m[k] = v
	}
	h := newEndpointHist()
	m[k] = h
	e.hists.Store(&m)
	return h
}

// endpoints is nil unless measurement.prometheus_endpoints is on; set by
// InitMeasure before any DB is created.
var endpoints atomic.Pointer[endpointStats]

// EndpointsEnabled says whether a binding should time requests per endpoint.
func EndpointsEnabled() bool {
	return endpoints.Load() != nil
}

type endpointOpKey struct{}

// EndpointOp is an operation's label for its requests (READ, INSERT,
// BATCH_INSERT, ...), made once (NewEndpointOp) so that labelling a request
// allocates no copy of it.
type EndpointOp struct{ name string }

// NewEndpointOp is the label of the operation named name.
func NewEndpointOp(name string) *EndpointOp { return &EndpointOp{name} }

// WithEndpointOp labels the requests sent on ctx with the operation they are
// part of. A request without one, such as a client's own topology refresh,
// is not recorded.
func WithEndpointOp(ctx context.Context, op *EndpointOp) context.Context {
	return context.WithValue(ctx, endpointOpKey{}, op)
}

// EndpointOpName returns the name of the operation WithEndpointOp put on ctx.
func EndpointOpName(ctx context.Context) (string, bool) {
	op, ok := ctx.Value(endpointOpKey{}).(*EndpointOp)
	if !ok || op == nil || op.name == "" {
		return "", false
	}
	return op.name, true
}

// Endpoint outcomes, appended to the operation's name.
const (
	EndpointOK       = ""
	EndpointError    = "_ERROR"
	EndpointRedirect = "_REDIRECT"
	// EndpointCanceled is a request the client itself ended: the run's stop
	// closed the client or canceled the request. No endpoint failed it.
	EndpointCanceled = "_CANCELED"
)

type measuredKey struct{}

// WithMeasured marks ctx with whether the operations run on it are measured:
// the worker decides it once when a batch starts, so that a warm-up ending
// while the batch runs can't leave some of its records (or requests)
// measured and the rest not.
func WithMeasured(ctx context.Context, measured bool) context.Context {
	return context.WithValue(ctx, measuredKey{}, measured)
}

// Measured says whether the operations on ctx are measured: as WithMeasured
// marked it, else whether the warm-up is over.
func Measured(ctx context.Context) bool {
	if measured, ok := ctx.Value(measuredKey{}).(bool); ok {
		return measured
	}
	return IsWarmUpFinished()
}

// MeasureEndpoint records one request of op to endpoint that took lan, with
// its outcome (EndpointOK, EndpointError, EndpointRedirect or
// EndpointCanceled). Like the operations, nothing is recorded during a
// warm-up (Measured(ctx)).
func MeasureEndpoint(ctx context.Context, endpoint, op, outcome string, lan time.Duration) {
	s := endpoints.Load()
	if s == nil || !Measured(ctx) {
		return
	}
	h := s.series(endpoint).hist(opOutcome{op, outcome})
	h.record(lan)
}

// SetEndpointInfo replaces what is known of the endpoints (the last CLUSTER
// NODES reply, say). Values that aren't UTF-8 are repaired: the server
// supplies them, and an invalid label value would fail every scrape.
func SetEndpointInfo(info []EndpointInfo) {
	s := endpoints.Load()
	if s == nil {
		return
	}
	info = append([]EndpointInfo(nil), info...)
	for i, e := range info {
		info[i] = EndpointInfo{validUTF8(e.Endpoint), validUTF8(e.NodeID), validUTF8(e.Role), validUTF8(e.Shard)}
	}
	s.mu.Lock()
	s.info = info
	s.mu.Unlock()
}

// endpointSample is one series' cumulative histogram at a scrape.
type endpointSample struct {
	endpoint, op, outcome string
	count                 uint64
	sum                   float64
	buckets               map[float64]uint64
}

// EndpointLabel is endpoint as a valid label value, for a binding to label
// its requests with: an address the server announced need not be UTF-8.
func EndpointLabel(endpoint string) string { return validUTF8(endpoint) }

func validUTF8(v string) string { return strings.ToValidUTF8(v, "\uFFFD") }

func (s *endpointStats) snapshot() ([]endpointSample, []EndpointInfo, int64) {
	var samples []endpointSample
	for endpoint, e := range *s.eps.Load() {
		for k, h := range *e.hists.Load() {
			// the count is the buckets' total, read once, so that it is never
			// below the last finite bucket's cumulative count
			buckets := make(map[float64]uint64, len(promLatencyBucketsUs))
			var cumulative uint64
			for i, upperUs := range promLatencyBucketsUs {
				cumulative += h.buckets[i].Load()
				buckets[float64(upperUs)/1e6] = cumulative
			}
			cumulative += h.buckets[len(promLatencyBucketsUs)].Load()
			samples = append(samples, endpointSample{endpoint, k.op, k.outcome, cumulative,
				float64(h.sumNs.Load()) / 1e9, buckets})
		}
	}
	s.mu.Lock()
	info := s.info
	s.mu.Unlock()
	return samples, info, s.overflow.Load()
}

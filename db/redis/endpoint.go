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
	"net"
	"os"
	"strings"
	"time"

	"github.com/pingcap/go-ycsb/pkg/measurement"
	goredis "github.com/redis/go-redis/v9"
)

// endpointHook times every request a node client sends, for
// measurement.prometheus_endpoints: in cluster mode go-redis makes one node
// client per endpoint it learns from the cluster's topology, and this hook,
// added to each, labels the requests with that endpoint. A request is one
// call on the node client: a redirect or a cluster-level retry is another
// one, a pipeline (a batch's share of one master) is one, and the node
// client's own retries (redis.max_retries; none in cluster mode by default)
// are inside it, backoff included. The time includes the wait for a pool
// connection and a new connection's set-up. Requests on a context with no
// operation (the client's own topology reads) are not recorded. A MULTI/EXEC
// transaction's redirects and command errors are set only after the hooks
// return, so they count as answered.
type endpointHook struct {
	endpoint string
}

// newEndpointHook times the requests of the node client that dials addr.
func newEndpointHook(addr string) endpointHook {
	return endpointHook{measurement.EndpointLabel(addr)}
}

func (h endpointHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h endpointHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		op, ok := measurement.EndpointOpName(ctx)
		if !ok || connSetup[cmd.Name()] {
			return next(ctx, cmd)
		}
		start := time.Now()
		err := next(ctx, cmd)
		measurement.MeasureEndpoint(ctx, h.endpoint, op, endpointOutcome(err), time.Since(start))
		return err
	}
}

func (h endpointHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		op, ok := measurement.EndpointOpName(ctx)
		if !ok || allConnSetup(cmds) {
			return next(ctx, cmds)
		}
		start := time.Now()
		err := next(ctx, cmds)
		lan := time.Since(start)
		outcome := endpointOutcome(err)
		for _, cmd := range cmds {
			if o := endpointOutcome(cmd.Err()); outcomeRank(o) > outcomeRank(outcome) {
				outcome = o
			}
		}
		measurement.MeasureEndpoint(ctx, h.endpoint, op, outcome, lan)
		return err
	}
}

// connSetup are the commands go-redis sends to set up a new connection
// (HELLO, AUTH, SELECT, CLIENT SETNAME/SETINFO/MAINT_NOTIFICATIONS,
// READONLY): they run through the hooks on the context of the operation that
// needed the connection, but aren't its requests. The time of that setup is
// in the request that waited for it.
var connSetup = map[string]bool{"hello": true, "auth": true, "select": true, "client": true, "readonly": true}

func allConnSetup(cmds []goredis.Cmder) bool {
	for _, cmd := range cmds {
		if !connSetup[cmd.Name()] {
			return false
		}
	}
	return len(cmds) > 0
}

// endpointOutcome classifies a request by its errors (a pipeline's: its own
// and its commands'), the worst of them winning: a failure, then an end the
// client caused (the run's stop), then a redirect (MOVED, ASK: the endpoint
// answering that another one owns the slot, not a failure), else answered.
// A nil reply (a missing key) is an answer.
func endpointOutcome(errs ...error) string {
	outcome := measurement.EndpointOK
	for _, err := range errs {
		o := measurement.EndpointOK
		switch {
		case err == nil || errors.Is(err, goredis.Nil):
		case isRedirect(err):
			o = measurement.EndpointRedirect
		case clientEnded(err):
			o = measurement.EndpointCanceled
		default:
			return measurement.EndpointError
		}
		if outcomeRank(o) > outcomeRank(outcome) {
			outcome = o
		}
	}
	return outcome
}

func outcomeRank(o string) int {
	switch o {
	case measurement.EndpointRedirect:
		return 1
	case measurement.EndpointCanceled:
		return 2
	case measurement.EndpointError:
		return 3
	}
	return 0
}

// clientEnded says whether the client, not the endpoint, ended a request:
// the run's stop closed the client (go-redis's ErrClosed, or the closed
// connection under a read) or canceled the request's context. go-redis also
// closes the client of a node that left the topology a minute after it left;
// a request still in flight there is counted here too.
func clientEnded(err error) bool {
	return errors.Is(err, goredis.ErrClosed) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled)
}

func isRedirect(err error) bool {
	var rerr goredis.Error
	if !errors.As(err, &rerr) {
		return false
	}
	msg := rerr.Error()
	return strings.HasPrefix(msg, "MOVED ") || strings.HasPrefix(msg, "ASK ")
}

// withEndpointOp labels ctx's requests with op, only when they are timed per
// endpoint: the label is one allocation per operation. An operation's
// opContext stays the outermost context, so that the contexts go-redis
// derives from it (a dial, a pool wait) keep following the grace through its
// AfterFunc rather than a goroutine each.
// The operations' labels for their requests.
var (
	opRead        = measurement.NewEndpointOp("READ")
	opUpdate      = measurement.NewEndpointOp("UPDATE")
	opInsert      = measurement.NewEndpointOp("INSERT")
	opBatchInsert = measurement.NewEndpointOp("BATCH_INSERT")
	opDelete      = measurement.NewEndpointOp("DELETE")
)

func (r *redis) withEndpointOp(ctx context.Context, op *measurement.EndpointOp) context.Context {
	if !r.endpoints { // inlined: nothing but this test when the option is off
		return ctx
	}
	return labelEndpointOp(ctx, op)
}

func labelEndpointOp(ctx context.Context, op *measurement.EndpointOp) context.Context {
	if c, ok := ctx.(*opContext); ok {
		return &opContext{measurement.WithEndpointOp(c.Context, op), c.run}
	}
	return measurement.WithEndpointOp(ctx, op)
}

// endpointInfoRefresh is how often the endpoints' identities are read again
// (CLUSTER NODES), so that a failover's new roles show within it.
const endpointInfoRefresh = 30 * time.Second

// refreshEndpointInfo publishes the cluster's CLUSTER NODES reply now and
// then every endpointInfoRefresh until stop is closed. A failed read keeps
// the last reply; only the first failure is logged (a proxy that refuses
// CLUSTER NODES would otherwise log every 30 s).
func refreshEndpointInfo(c *goredis.ClusterClient, stop <-chan struct{}) {
	logged := false
	read := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reply, err := c.ClusterNodes(ctx).Result()
		if err != nil {
			if !logged {
				logged = true
				fmt.Fprintf(os.Stderr, "redis: CLUSTER NODES for the endpoint metrics (no ycsb_endpoint_info until it answers): %v\n", err)
			}
			return
		}
		measurement.SetEndpointInfo(parseClusterNodes(reply))
	}
	read()
	go func() {
		t := time.NewTicker(endpointInfoRefresh)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				read()
			}
		}
	}()
}

// parseClusterNodes reads a CLUSTER NODES reply: per line, the node ID,
// ip:port@cport[,hostname[,...]], the flags and, for a replica, its master's
// node ID. Addresses are formatted as go-redis dials them (net.JoinHostPort:
// an IPv6 address in brackets; Redis prints it bare). A node with a hostname
// is listed under both hostname:port and ip:port, as the client may dial
// either. Nodes with no address, or in a handshake, are left out; when two
// lines share an address (a node restarted with a new ID leaves a failed
// ghost), a node not flagged fail wins. go-redis rewrites a loopback address
// to the seed's host and a port 0 to the seed's port; such endpoints get no
// info series.
func parseClusterNodes(reply string) []measurement.EndpointInfo {
	var info []measurement.EndpointInfo
	at := map[string]int{}      // endpoint -> index in info
	failed := map[string]bool{} // endpoint -> its entry is flagged fail
	ghosts := map[string]bool{} // failed node IDs a live node replaced
	add := func(e measurement.EndpointInfo, fail bool) {
		if i, ok := at[e.Endpoint]; ok {
			if failed[e.Endpoint] && !fail {
				ghosts[info[i].NodeID] = true
				info[i], failed[e.Endpoint] = e, false
			}
			return
		}
		at[e.Endpoint], failed[e.Endpoint] = len(info), fail
		info = append(info, e)
	}
	for _, line := range strings.Split(reply, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		id, addr, flags, master := f[0], f[1], f[2], f[3]
		addr, extra, _ := strings.Cut(addr, ",")
		hostname, _, _ := strings.Cut(extra, ",")
		if strings.Contains(hostname, "=") { // an auxiliary field (shard-id=...), not a hostname
			hostname = ""
		}
		addr, _, _ = strings.Cut(addr, "@")
		i := strings.LastIndexByte(addr, ':')
		if i < 0 {
			continue
		}
		host, port := strings.Trim(addr[:i], "[]"), addr[i+1:]
		if port == "" || port == "0" {
			continue
		}
		e := measurement.EndpointInfo{NodeID: id, Role: "unknown"}
		fail := false
		for _, flag := range strings.Split(flags, ",") {
			switch flag {
			case "master":
				e.Role, e.Shard = "master", id
			case "slave", "replica":
				e.Role, e.Shard = "replica", master
			case "handshake", "noaddr":
				e.Role = ""
			case "fail":
				fail = true
			}
		}
		if e.Role == "" {
			continue
		}
		if e.Shard == "-" {
			e.Shard = ""
		}
		if host != "" {
			e.Endpoint = net.JoinHostPort(host, port)
			add(e, fail)
		}
		if hostname != "" && hostname != host {
			e.Endpoint = net.JoinHostPort(hostname, port)
			add(e, fail)
		}
	}
	if len(ghosts) == 0 {
		return info
	}
	// a replaced ghost's other addresses (its hostname alias) go with it
	live := info[:0]
	for _, e := range info {
		if !ghosts[e.NodeID] {
			live = append(live, e)
		}
	}
	return live
}

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
// try on one endpoint: a command redirected or retried is several, and a
// pipeline (a batch's share of one master) is one. The time includes the
// wait for a pool connection. Requests on a context with no operation (the
// client's own topology refreshes) are not recorded.
type endpointHook struct {
	endpoint string
}

func (h endpointHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h endpointHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		op, ok := measurement.EndpointOp(ctx)
		if !ok || connSetup[cmd.Name()] {
			return next(ctx, cmd)
		}
		start := time.Now()
		err := next(ctx, cmd)
		measurement.MeasureEndpoint(h.endpoint, op, endpointOutcome(err), time.Since(start))
		return err
	}
}

func (h endpointHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		op, ok := measurement.EndpointOp(ctx)
		if !ok || allConnSetup(cmds) {
			return next(ctx, cmds)
		}
		start := time.Now()
		err := next(ctx, cmds)
		lan := time.Since(start)
		outcome := endpointOutcome(err)
		for _, cmd := range cmds {
			if o := endpointOutcome(cmd.Err()); o == measurement.EndpointError {
				outcome = o
				break
			} else if o == measurement.EndpointRedirect {
				outcome = o
			}
		}
		measurement.MeasureEndpoint(h.endpoint, op, outcome, lan)
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

// endpointOutcome classifies a request by its errors: a redirect (MOVED, ASK)
// is the endpoint answering that another one owns the slot, not a failure,
// and a nil reply (a missing key) is an answer.
func endpointOutcome(errs ...error) string {
	outcome := measurement.EndpointOK
	for _, err := range errs {
		switch {
		case err == nil || errors.Is(err, goredis.Nil):
		case isRedirect(err):
			outcome = measurement.EndpointRedirect
		default:
			return measurement.EndpointError
		}
	}
	return outcome
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
// endpoint: the label is one allocation per operation.
func (r *redis) withEndpointOp(ctx context.Context, op string) context.Context {
	if !r.endpoints {
		return ctx
	}
	return measurement.WithEndpointOp(ctx, op)
}

// endpointInfoRefresh is how often the endpoints' identities are read again
// (CLUSTER NODES), so that a failover's new roles show within it.
const endpointInfoRefresh = 30 * time.Second

// refreshEndpointInfo publishes the cluster's CLUSTER NODES reply now and
// then every endpointInfoRefresh until stop is closed. A failed read keeps
// the last reply.
func refreshEndpointInfo(c *goredis.ClusterClient, stop <-chan struct{}) {
	read := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reply, err := c.ClusterNodes(ctx).Result()
		if err != nil {
			fmt.Fprintf(os.Stderr, "redis: CLUSTER NODES for the endpoint metrics: %v\n", err)
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
// ip:port@cport[,hostname], the flags and, for a replica, its master's node
// ID. A node with a hostname is listed under both host:port and ip:port, as
// the client may dial either. Nodes with no address (a failed node no longer
// known, say) are left out.
func parseClusterNodes(reply string) []measurement.EndpointInfo {
	var info []measurement.EndpointInfo
	for _, line := range strings.Split(reply, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		id, addr, flags, master := f[0], f[1], f[2], f[3]
		addr, hostname, _ := strings.Cut(addr, ",")
		addr, _, _ = strings.Cut(addr, "@")
		host, port, err := net.SplitHostPort(addr)
		if err != nil || port == "0" {
			continue
		}
		e := measurement.EndpointInfo{NodeID: id, Role: "replica", Shard: master}
		for _, flag := range strings.Split(flags, ",") {
			if flag == "master" {
				e.Role, e.Shard = "master", id
			}
		}
		if e.Shard == "-" {
			e.Shard = ""
		}
		if host != "" {
			e.Endpoint = addr
			info = append(info, e)
		}
		if hostname != "" && hostname != host {
			e.Endpoint = net.JoinHostPort(hostname, port)
			info = append(info, e)
		}
	}
	return info
}

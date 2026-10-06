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
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/magiconair/properties"
	goredis "github.com/redis/go-redis/v9"
)

// fakeNodes are Redis nodes in the test process: a TCP server per node
// address that speaks RESP and answers what the client sends (inserts with
// OK), so the client's real code runs and the tests see every command the
// client sent, its own (HELLO, CLIENT, COMMAND, ...) included.
type fakeNodes struct {
	mu        sync.Mutex
	all       []string     // every command received, lowercased name [arg]
	slotLoads atomic.Int32 // cluster slot map loads
	listeners map[string]net.Listener
}

func newFakeNodes(t *testing.T) *fakeNodes {
	n := &fakeNodes{listeners: map[string]net.Listener{}}
	t.Cleanup(func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, ln := range n.listeners {
			ln.Close()
		}
	})
	return n
}

// dial is the clients' Dialer: addr is a fake node's address.
func (n *fakeNodes) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	ln, ok := n.listeners[addr]
	if !ok {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			n.mu.Unlock()
			return nil, err
		}
		n.listeners[addr] = ln
		go n.accept(ln)
	}
	n.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", ln.Addr().String())
}

func (n *fakeNodes) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go n.serve(conn)
	}
}

// serve answers a connection's commands, flushing the replies once its input
// is drained (so a pipeline is answered as one).
func (n *fakeNodes) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	w := bufio.NewWriter(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		w.WriteString(n.reply(args))
		if r.Buffered() == 0 && w.Flush() != nil {
			return
		}
	}
}

func (n *fakeNodes) reply(args []string) string {
	name := strings.ToLower(args[0])
	n.mu.Lock()
	n.all = append(n.all, strings.ToLower(strings.Join(args[:min(2, len(args))], " ")))
	n.mu.Unlock()
	switch name {
	case "ping":
		return "+PONG\r\n"
	case "hello":
		if len(args) > 1 && args[1] == "3" {
			return "%1\r\n$6\r\nserver\r\n$5\r\nredis\r\n"
		}
	case "command":
		return "*0\r\n"
	case "hset":
		return fmt.Sprintf(":%d\r\n", (len(args)-2)/2)
	case "set", "json.set":
		return "+OK\r\n"
	}
	return "-ERR unknown command '" + args[0] + "'\r\n"
}

// sentNames returns how many of every command (name, and subcommand for
// CLIENT) the nodes got, the client's own included.
func (n *fakeNodes) sentNames() map[string]int {
	n.mu.Lock()
	defer n.mu.Unlock()
	names := map[string]int{}
	for _, c := range n.all {
		if !strings.HasPrefix(c, "client ") {
			c, _, _ = strings.Cut(c, " ")
		}
		names[c]++
	}
	return names
}

// readCommand reads one RESP array of bulk strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("not an array: %q", line)
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, count)
	for i := range args {
		if line, err = r.ReadString('\n'); err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

const singleAddr = "fake-single:6379"

// The fake cluster: three masters, the default 3-shard slot split.
var clusterSlots = []goredis.ClusterSlot{
	{Start: 0, End: 5460, Nodes: []goredis.ClusterNode{{Addr: "fake-node-1:7001"}}},
	{Start: 5461, End: 10922, Nodes: []goredis.ClusterNode{{Addr: "fake-node-2:7002"}}},
	{Start: 10923, End: 16383, Nodes: []goredis.ClusterNode{{Addr: "fake-node-3:7003"}}},
}

func newFakeRedis(t *testing.T, mode, datatype string, props ...string) (*redis, *fakeNodes) {
	t.Helper()
	nodes := newFakeNodes(t)
	p := properties.NewProperties()
	p.Set("threadcount", "2")
	for i := 0; i+1 < len(props); i += 2 {
		p.Set(props[i], props[i+1])
	}
	var client redisClient
	switch mode {
	case "single":
		opts, err := getOptionsSingle(p)
		if err != nil {
			t.Fatal(err)
		}
		opts.Addr, opts.Dialer = singleAddr, nodes.dial
		client = goredis.NewClient(opts)
	case "cluster":
		opts, err := getOptionsCluster(p)
		if err != nil {
			t.Fatal(err)
		}
		opts.Addrs = []string{clusterSlots[0].Nodes[0].Addr}
		opts.ClusterSlots = func(context.Context) ([]goredis.ClusterSlot, error) {
			nodes.slotLoads.Add(1)
			return clusterSlots, nil
		}
		opts.Dialer = nodes.dial
		client = goredis.NewClusterClient(opts)
	}
	t.Cleanup(func() { client.Close() })
	return &redis{client: client, mode: mode, datatype: datatype, fieldcount: 3}, nodes
}

func testRecords(n int) ([]string, []map[string][]byte) {
	keys := make([]string, n)
	values := make([]map[string][]byte, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("user%d", i)
		values[i] = map[string][]byte{
			"field0": []byte(fmt.Sprintf("a%d", i)),
			"field1": []byte(fmt.Sprintf("b%d", i)),
			"field2": []byte(fmt.Sprintf("c%d", i)),
		}
	}
	return keys, values
}

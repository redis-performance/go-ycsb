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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/magiconair/properties"
	goredis "github.com/redis/go-redis/v9"
)

// sentCmd is one command as a fake node received it.
type sentCmd struct {
	addr  string
	args  []string
	flush int // the reply flush it was answered in: one per round trip
}

// fakeNodes are Redis nodes in the test process: a TCP server per node
// address that speaks RESP, records the insert commands and answers them (OK,
// or the error reply fail returns), so the client's real pipeline code runs
// and the tests see what was sent where. A node answers what it has read once
// its input is drained, so a pipeline shares a flush, while commands sent one
// at a time each get their own.
type fakeNodes struct {
	mu        sync.Mutex
	sent      []sentCmd
	flushes   int
	fail      func(addr string, args []string) string // an error reply, or ""
	moved     func(addr string, args []string) string // the node to answer MOVED to, or ""
	all       []string                                // every command received, lowercased name [arg]
	slotLoads atomic.Int32                            // cluster slot map loads
	drop      func(addr string, args []string) bool   // drop the connection instead of answering
	hang      map[string]bool                         // nodes that stall: read, never answer
	delay     map[string]time.Duration                // nodes that answer each command late
	failPing  bool                                    // answer PING with an error
	allAt     []string                                // every command received, as addr + " " + lowercased name
	down      map[string]bool                         // nodes that refuse connections
	listeners map[string]net.Listener
}

func newFakeNodes(t *testing.T) *fakeNodes {
	n := &fakeNodes{down: map[string]bool{}, listeners: map[string]net.Listener{}}
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
	if n.down[addr] {
		n.mu.Unlock()
		return nil, fmt.Errorf("dial %s: connection refused", addr)
	}
	ln, ok := n.listeners[addr]
	if !ok {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			n.mu.Unlock()
			return nil, err
		}
		n.listeners[addr] = ln
		go n.accept(addr, ln)
	}
	n.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", ln.Addr().String())
}

func (n *fakeNodes) accept(addr string, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go n.serve(addr, conn)
	}
}

func (n *fakeNodes) serve(addr string, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	w := bufio.NewWriter(conn)
	flush := n.nextFlush()
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if n.dropped(addr, args) {
			return // closes the connection, with no reply
		}
		if n.hung(addr) {
			time.Sleep(10 * time.Second) // no reply: a stalled node
			return
		}
		n.mu.Lock()
		d := n.delay[addr]
		n.mu.Unlock()
		time.Sleep(d)
		w.WriteString(n.reply(addr, args, flush))
		if r.Buffered() == 0 {
			if w.Flush() != nil {
				return
			}
			flush = n.nextFlush()
		}
	}
}

func (n *fakeNodes) nextFlush() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.flushes++
	return n.flushes
}

// recorded are the commands an insert sends; the client's own (HELLO,
// CLIENT, COMMAND, ...) aren't recorded.
var recorded = map[string]bool{"hset": true, "set": true, "json.set": true}

// readsDeletes are the other data commands the fake nodes answer (and can
// drop), unrecorded.
var readsDeletes = map[string]bool{"hgetall": true, "del": true}

func (n *fakeNodes) reply(addr string, args []string, flush int) string {
	name := strings.ToLower(args[0])
	n.mu.Lock()
	n.all = append(n.all, strings.ToLower(strings.Join(args[:min(2, len(args))], " ")))
	n.allAt = append(n.allAt, addr+" "+name)
	n.mu.Unlock()
	switch {
	case name == "ping":
		n.mu.Lock()
		failPing := n.failPing
		n.mu.Unlock()
		if failPing {
			return "-ERR ping refused\r\n"
		}
		return "+PONG\r\n"
	case name == "hello" && len(args) > 1 && args[1] == "3":
		return "%1\r\n$6\r\nserver\r\n$5\r\nredis\r\n"
	case name == "command":
		return "*0\r\n"
	case name == "asking":
		return "+OK\r\n"
	case name == "hgetall":
		return "%1\r\n$6\r\nfield0\r\n$1\r\nx\r\n"
	case name == "del":
		return ":1\r\n"
	case !recorded[name]:
		return "-ERR unknown command '" + args[0] + "'\r\n"
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.moved != nil {
		if to := n.moved(addr, args); to != "" {
			kind := "MOVED"
			if strings.HasPrefix(to, "ask:") {
				kind, to = "ASK", strings.TrimPrefix(to, "ask:")
			}
			return fmt.Sprintf("-%s %d %s\r\n", kind, keySlot(args[1]), to)
		}
	}
	n.sent = append(n.sent, sentCmd{addr: addr, args: args, flush: flush})
	if n.fail != nil {
		if e := n.fail(addr, args); e != "" {
			return "-" + e + "\r\n"
		}
	}
	if name == "hset" {
		return fmt.Sprintf(":%d\r\n", (len(args)-2)/2)
	}
	return "+OK\r\n"
}

// hung says whether addr stalls instead of answering.
func (n *fakeNodes) hung(addr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hang[addr]
}

// dropped says whether to drop the connection instead of answering args.
func (n *fakeNodes) dropped(addr string, args []string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.drop != nil && (recorded[strings.ToLower(args[0])] || readsDeletes[strings.ToLower(args[0])]) && n.drop(addr, args)
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

// take returns the commands recorded so far, and forgets them.
func (n *fakeNodes) take() []sentCmd {
	n.mu.Lock()
	defer n.mu.Unlock()
	sent := n.sent
	n.sent = nil
	return sent
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
	// fail fast on a down node
	p.Set(redisMaxRedirects, "-1")
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

// normalized makes a command comparable: HSET's field/value pairs come in map
// order, so they are sorted.
func normalized(args []string) string {
	if len(args) > 2 && strings.EqualFold(args[0], HSET) {
		pairs := make([]string, 0, (len(args)-2)/2)
		for i := 2; i+1 < len(args); i += 2 {
			pairs = append(pairs, args[i]+"="+args[i+1])
		}
		sort.Strings(pairs)
		return fmt.Sprintf("%s %s %v", args[0], args[1], pairs)
	}
	return strings.Join(args, " ")
}

func keyOf(args []string) string { return args[1] }

// crc16 is the CRC16-XMODEM of Redis Cluster's key-to-slot mapping, written
// out here so the routing is checked against the spec, not against go-redis.
func crc16(b []byte) uint16 {
	var crc uint16
	for _, c := range b {
		crc ^= uint16(c) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func keySlot(key string) int {
	k := []byte(key)
	if s := indexByte(k, '{'); s >= 0 {
		if e := indexByte(k[s+1:], '}'); e > 0 {
			k = k[s+1 : s+1+e]
		}
	}
	return int(crc16(k) % 16384)
}

func slotOwner(key string) string {
	slot := keySlot(key)
	for _, s := range clusterSlots {
		if slot >= s.Start && slot <= s.End {
			return s.Nodes[0].Addr
		}
	}
	return ""
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func TestCRC16(t *testing.T) {
	// the Redis Cluster spec's test vector
	if got := crc16([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("crc16 = %#x, want 0x31c3", got)
	}
}

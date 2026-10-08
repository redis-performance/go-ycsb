package redis

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	json "github.com/segmentio/encoding/json"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/util"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	goredis "github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

const HASH_DATATYPE string = "hash"
const STRING_DATATYPE string = "string"
const JSON_DATATYPE string = "json"
const JSON_SET string = "JSON.SET"
const JSON_GET string = "JSON.GET"
const HSET string = "HSET"
const HMGET string = "HMGET"

type redisClient interface {
	Get(ctx context.Context, key string) *goredis.StringCmd
	HGetAll(ctx context.Context, key string) *goredis.MapStringStringCmd
	Do(ctx context.Context, args ...interface{}) *goredis.Cmd
	Pipeline() goredis.Pipeliner
	TxPipeline() goredis.Pipeliner
	Scan(ctx context.Context, cursor uint64, match string, count int64) *goredis.ScanCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *goredis.StatusCmd
	Del(ctx context.Context, keys ...string) *goredis.IntCmd
	FlushDB(ctx context.Context) *goredis.StatusCmd
	Close() error
}

type redis struct {
	client     redisClient
	mode       string
	datatype   string
	fieldcount int64
	// endpoints: requests are timed per endpoint (measurement.prometheus_endpoints)
	endpoints     bool
	stopEndpoints chan struct{} // ends the CLUSTER NODES refresh

	mu     sync.Mutex                   // guards runs and closed
	runs   map[<-chan struct{}]*runStop // the stops of the runs with threads
	closed bool
}

func (r *redis) Close() error {
	r.mu.Lock()
	if r.stopEndpoints != nil {
		close(r.stopEndpoints)
		r.stopEndpoints = nil
	}
	for key, s := range r.runs {
		s.retire()
		delete(r.runs, key)
	}
	if r.closed { // by a stop's grace
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	return r.client.Close()
}

// stopGrace is how long after the run's stop the operations already sent may
// still run to their outcome. At its end everything still in flight is ended
// at once (see runStop), so the run ends, and prints its summary, well before
// go-ycsb's force-exit 10 s after the stop, whatever redis.read_timeout,
// redis.dial_timeout or the retry settings.
var stopGrace = 5 * time.Second

// runStop is a run's stop, for the operations its threads sent: the threads
// of a run share their context's cancellation (its Done channel identifies
// the run). At the run's stop the grace starts; at its end, done is canceled,
// which ends whatever an operation waits for on its context (a retry's
// back-off, a pool turn, a dial), and the client is closed, which ends the
// reads in progress.
//
// A deliberate limit: the client is the instance's, shared by every run on
// it, so the end of one run's grace closes it for all of them. Runs one after
// the other on one instance are kept apart (a run whose threads are done
// arms no close: see retire), but runs at the same time are not: at the end
// of the first one's grace the others' operations fail ("redis: client is
// closed"), whether they stopped or not. go-ycsb's CLI has one run per
// process, so this only concerns an embedder running several at once, which
// needs an instance (a Create) per run.
type runStop struct {
	key     <-chan struct{} // the run's contexts' Done channel
	done    context.Context
	cancel  context.CancelFunc
	unhook  func() bool // stops waiting for the run's stop
	timer   *time.Timer // the grace, once the run stopped
	threads int         // threads started and not cleaned up
	retired bool        // no grace to come: its threads are done, or the client closed
}

// retire disarms the run's stop: its threads are done (or the client is
// closed), so its end mustn't close anything, during a later run say.
// Called with r.mu held.
func (s *runStop) retire() {
	s.retired = true
	s.unhook()
	if s.timer != nil {
		s.timer.Stop()
	}
}

// thread is a thread's context, and the context of its operations once sent:
// the thread's values, the run's grace for cancellation (opContext, made once
// here for operations on the thread's own context, so that an operation sent
// on its own allocates none; a batch, whose context carries one value more,
// gets an opContext of its own).
type thread struct {
	ctx  context.Context
	sent context.Context
	run  *runStop
}

type threadKey struct{}

// opContext is the context of an operation sent: its own context's values,
// and the end of the run's grace for cancellation.
type opContext struct {
	context.Context
	run *runStop
}

func (c *opContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *opContext) Done() <-chan struct{}       { return c.run.done.Done() }
func (c *opContext) Err() error                  { return c.run.done.Err() }

// AfterFunc lets contexts derived from an opContext follow the grace without
// a goroutine each (context.AfterFunc and context.WithCancel use it).
func (c *opContext) AfterFunc(f func()) func() bool { return context.AfterFunc(c.run.done, f) }

func (r *redis) InitThread(ctx context.Context, _ int, _ int) context.Context {
	r.mu.Lock()
	s := r.runs[ctx.Done()]
	if s == nil {
		s = &runStop{key: ctx.Done()}
		s.done, s.cancel = context.WithCancel(context.Background())
		grace := stopGrace
		s.unhook = context.AfterFunc(ctx, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if !s.retired {
				s.timer = time.AfterFunc(grace, func() { r.endGrace(s) })
			}
		})
		if r.runs == nil {
			r.runs = make(map[<-chan struct{}]*runStop)
		}
		r.runs[s.key] = s
	}
	s.threads++
	r.mu.Unlock()
	t := &thread{run: s}
	t.ctx = context.WithValue(ctx, threadKey{}, t)
	t.sent = &opContext{t.ctx, s}
	return t.ctx
}

func (r *redis) CleanupThread(ctx context.Context) {
	t, ok := ctx.Value(threadKey{}).(*thread)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.run.threads--; t.run.threads == 0 && !t.run.retired {
		t.run.retire()
		if r.runs[t.run.key] == t.run {
			delete(r.runs, t.run.key)
		}
	}
}

// closeToCancel is how long after closing the client at the end of the grace
// the operations' context is canceled: at the default redirects, long enough
// for tries already failing on the closed client to finish (each fails at
// once, after a back-off of some ms) with their records' real outcomes. If
// the cancel lands during a back-off or a dial (large redis.max_redirects, or
// a dial in progress, at any setting), go-redis marks every record of that
// batch failed, the ones the healthy masters wrote too, and INSERT undercounts
// what was written; with redis.max_redirects=-1 only the records of the
// master that didn't answer fail. Short enough to end at once what would
// otherwise wait on: a long back-off, a pool turn, a dial.
const closeToCancel = 200 * time.Millisecond

// endGrace ends what the run still has in flight: it closes the client (unless
// the run closed it already), which ends the reads in progress, and then
// cancels the operations' context, which ends what they wait for on it.
func (r *redis) endGrace(s *runStop) {
	r.mu.Lock()
	if s.retired {
		r.mu.Unlock()
		return
	}
	// armed before the close, which can block (go-redis waits for its
	// maintenance notifications handler, when they are on)
	time.AfterFunc(closeToCancel, s.cancel)
	closed := r.closed // by another run's grace: this run's waits still end
	r.closed = true
	r.mu.Unlock()
	if !closed {
		r.client.Close()
	}
}

func (r *redis) Read(ctx context.Context, table string, key string, fields []string) (data map[string][]byte, err error) {
	if ctx, err = started(ctx); err != nil {
		return nil, err
	}
	ctx = r.withEndpointOp(ctx, "READ")
	data = make(map[string][]byte, len(fields))
	switch r.datatype {
	case JSON_DATATYPE:
		cmds := make([]*goredis.Cmd, len(fields))
		// TxPipeline (MULTI/EXEC), not Pipeline: every command below targets
		// the same single key, so this is a valid single-slot transaction
		// even in cluster mode, and it makes the whole-entity read atomic
		// with respect to a concurrent whole-entity write (see Update below).
		pipe := r.client.TxPipeline()
		for pos, fieldName := range fields {
			cmds[pos] = pipe.Do(ctx, JSON_GET, getKeyName(table, key), getFieldJsonPath(fieldName))
		}
		_, err = pipe.Exec(ctx)
		if err != nil {
			return
		}
		var s string = ""
		for pos, fieldName := range fields {
			s, err = cmds[pos].Text()
			if err != nil {
				return
			}
			data[fieldName] = []byte(s)
		}
	case HASH_DATATYPE:
		// Whole-entity read: nil/empty fields, or the full field set, maps to
		// HGETALL rather than an HMGET enumerating every field. This matches
		// how a feature-store style entity-major hash is served in production
		// and lets Redis walk the hash directly instead of parsing a field list.
		if len(fields) == 0 || int64(len(fields)) == r.fieldcount {
			mapReply, errI := r.client.HGetAll(ctx, getKeyName(table, key)).Result()
			if errI != nil {
				err = errI
				return
			}
			// Redis never stores a hash with zero fields (deleting the last
			// field deletes the key), so an empty reply unambiguously means
			// the key doesn't exist - surface that as an error rather than a
			// silent empty-but-successful read, matching the HMGET path below.
			if len(mapReply) == 0 {
				err = fmt.Errorf("redis: no such key %q", getKeyName(table, key))
				return
			}
			for fieldName, value := range mapReply {
				data[fieldName] = []byte(value)
			}
			return
		}
		args := make([]interface{}, 0, len(fields)+2)
		args = append(args, HMGET, getKeyName(table, key))
		for _, fieldName := range fields {
			args = append(args, fieldName)
		}
		sliceReply, errI := r.client.Do(ctx, args...).StringSlice()
		if errI != nil {
			err = errI
			return
		}
		for pos, slicePos := range sliceReply {
			data[fields[pos]] = []byte(slicePos)
		}
	case STRING_DATATYPE:
		fallthrough
	default:
		{
			var res string = ""
			res, err = r.client.Get(ctx, getKeyName(table, key)).Result()
			if err != nil {
				return
			}
			err = json.Unmarshal([]byte(res), &data)
			return
		}
	}
	return

}

func (r *redis) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	return nil, fmt.Errorf("scan is not supported")
}

func (r *redis) Update(ctx context.Context, table string, key string, values map[string][]byte) (err error) {
	if ctx, err = started(ctx); err != nil {
		return err
	}
	ctx = r.withEndpointOp(ctx, "UPDATE")
	// check if it's full update. If yes then we can avoid reading the previous value on string datype
	fullUpdate := false
	if int64(len(values)) == r.fieldcount {
		fullUpdate = true
	}
	err = nil
	switch r.datatype {
	case JSON_DATATYPE:
		cmds := make([]*goredis.Cmd, 0, len(values))
		// TxPipeline: see the matching comment in Read - all commands target
		// the same key, so MULTI/EXEC keeps the whole-row write atomic
		// instead of letting a concurrent reader observe a torn row.
		pipe := r.client.TxPipeline()
		for fieldName, bytes := range values {
			cmd := pipe.Do(ctx, JSON_SET, getKeyName(table, key), getFieldJsonPath(fieldName), jsonEscape(bytes))
			cmds = append(cmds, cmd)
		}
		_, err = pipe.Exec(ctx)
		if err != nil {
			return
		}
		for _, cmd := range cmds {
			err = cmd.Err()
			if err != nil {
				return
			}
		}
	case HASH_DATATYPE:
		args := make([]interface{}, 0, 2*len(values)+2)
		args = append(args, HSET, getKeyName(table, key))
		for fieldName, bytes := range values {
			args = append(args, fieldName, string(bytes))
		}
		err = r.client.Do(ctx, args...).Err()
	case STRING_DATATYPE:
		fallthrough
	default:
		{
			var encodedJson = make([]byte, 0)
			if fullUpdate {
				encodedJson, err = json.Marshal(values)
				if err != nil {
					return err
				}
			} else {
				var initialEncodedJson string = ""
				initialEncodedJson, err = r.client.Get(ctx, getKeyName(table, key)).Result()
				if err != nil {
					return
				}
				err, encodedJson = mergeEncodedJsonWithMap(initialEncodedJson, values)
				if err != nil {
					return
				}
			}
			return r.client.Set(ctx, getKeyName(table, key), string(encodedJson), 0).Err()
		}
	}
	return
}

func mergeEncodedJsonWithMap(stringReply string, values map[string][]byte) (err error, data []byte) {
	curVal := map[string][]byte{}
	err = json.Unmarshal([]byte(stringReply), &curVal)
	if err != nil {
		return
	}
	for k, v := range values {
		curVal[k] = v
	}
	data, err = json.Marshal(curVal)
	return
}

func jsonEscape(bytes []byte) string {
	return fmt.Sprintf("\"%s\"", string(bytes))
}

func getFieldJsonPath(fieldName string) string {
	return fmt.Sprintf("$.%s", fieldName)
}

func getKeyName(table string, key string) string {
	return table + "/" + key
}

// cmdable is what insert needs of a client or a pipeline.
type cmdable interface {
	Do(ctx context.Context, args ...interface{}) *goredis.Cmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *goredis.StatusCmd
}

// insert issues the command that inserts one record on c: it runs it on a
// client and queues it on a pipeline, so Insert and BatchInsert write a
// record with the very same command.
func (r *redis) insert(ctx context.Context, c cmdable, table string, key string, values map[string][]byte) (goredis.Cmder, error) {
	switch r.datatype {
	case JSON_DATATYPE:
		data, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		return c.Do(ctx, JSON_SET, getKeyName(table, key), ".", string(data)), nil
	case HASH_DATATYPE:
		args := make([]interface{}, 0, 2*len(values)+2)
		args = append(args, HSET, getKeyName(table, key))
		for fieldName, bytes := range values {
			args = append(args, fieldName, string(bytes))
		}
		return c.Do(ctx, args...), nil
	case STRING_DATATYPE:
		fallthrough
	default:
		data, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		return c.Set(ctx, getKeyName(table, key), string(data), 0), nil
	}
}

func (r *redis) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	ctx, err := started(ctx)
	if err != nil {
		return err
	}
	ctx = r.withEndpointOp(ctx, "INSERT")
	cmd, err := r.insert(ctx, r.client, table, key, values)
	if err != nil {
		return err
	}
	return cmd.Err()
}

// BatchInsert sends the records' insert commands in one pipeline (not a
// MULTI/EXEC transaction): in cluster mode go-redis splits it by the node
// owning each key's slot and runs the nodes' pipelines concurrently. Each
// record succeeds or fails on its own, as with Insert; a *ycsb.BatchError
// reports which failed.
func (r *redis) BatchInsert(ctx context.Context, table string, keys []string, values []map[string][]byte) error {
	errs := make([]error, len(keys))
	ctx, err := started(ctx)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return ycsb.NewBatchError(errs)
	}
	ctx = r.withEndpointOp(ctx, "BATCH_INSERT")
	cmds := make([]goredis.Cmder, len(keys))
	pipe := r.client.Pipeline()
	for i, key := range keys {
		cmds[i], errs[i] = r.insert(ctx, pipe, table, key, values[i])
	}
	if pipe.Len() == 0 {
		return ycsb.NewBatchError(errs)
	}
	// The per-command errors below are the outcome; Exec's error is the first
	// of them, unless the pipeline failed without setting them.
	_, execErr := pipe.Exec(ctx)
	anyCmdErr := false
	for i, cmd := range cmds {
		if cmd != nil && cmd.Err() != nil {
			errs[i] = cmd.Err()
			anyCmdErr = true
		}
	}
	if execErr != nil && !anyCmdErr {
		for i, cmd := range cmds {
			if cmd != nil {
				errs[i] = execErr
			}
		}
	}
	return ycsb.NewBatchError(errs)
}

func (r *redis) Delete(ctx context.Context, table string, key string) error {
	ctx, err := started(ctx)
	if err != nil {
		return err
	}
	ctx = r.withEndpointOp(ctx, "DELETE")
	return r.client.Del(ctx, getKeyName(table, key)).Err()
}

// pingMaster checks a cluster is up by pinging a master, the master of a
// random slot as go-redis v9.8.0's Ping did, trying another one up to tries
// times. go-redis v9.22.0 sends a keyless command such as PING to any node,
// replicas too, without routing policies (redis.routing_policies=false).
// A slot map that can't be loaded fails at once: each try would load it
// again, synchronously, which against a stalled cluster multiplies the time
// to fail by the tries.
func pingMaster(ctx context.Context, c *goredis.ClusterClient, tries int) error {
	var err error
	for i := 0; i < max(1, tries); i++ {
		var master *goredis.Client
		if master, err = c.MasterForKey(ctx, strconv.Itoa(rand.Int())); err != nil {
			return err
		}
		if err = master.Ping(ctx).Err(); err == nil {
			return nil
		}
	}
	return err
}

// newClusterClient makes the cluster client, loads the slot map and pings a
// master (redis.max_redirects + 1 tries, as go-redis retries a command).
func newClusterClient(ctx context.Context, opts *goredis.ClusterOptions) (*goredis.ClusterClient, error) {
	c := goredis.NewClusterClient(opts) // opts now has go-redis's defaults applied
	// ReloadState reloads cluster state. It calls ClusterSlots func
	// to get cluster slots information.
	c.ReloadState(ctx)
	if err := pingMaster(ctx, c, opts.MaxRedirects+1); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// started returns the context an operation runs in, once it starts. If the
// run has stopped (ctx canceled) the operation isn't sent: its error wraps
// ycsb.ErrNotRun and the stop's, and counts as nothing (the stop came between
// the client's check and this one). Once sent, it runs to
// its own outcome, its timeouts and retries included, on a context the stop
// doesn't cancel until the end of the grace: go-redis ends a retry's
// back-off on a canceled context and then sets every command still in the
// try (in cluster pipelines: every command of the batch, the ones already
// written too) to the context's error, so a stop that canceled them at once
// would turn records written, or retried in, into errors. At the end of the
// grace the operation fails: a try after the close with "redis: client is
// closed", which is also how a read in progress ends at the default retries
// (the closed connection's error is retryable, and the next try finds the
// client closed): it ends with "use of closed network connection" only with
// redis.max_retries / redis.max_redirects at -1, while a pipeline's commands
// on that connection keep that error; what waited on the context (a
// back-off, a pool turn, a dial) fails with context.Canceled.
func started(ctx context.Context) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ycsb.ErrNotRun, err)
	}
	if t, ok := ctx.Value(threadKey{}).(*thread); ok {
		if ctx == t.ctx {
			return t.sent, nil
		}
		return &opContext{ctx, t.run}, nil
	}
	// A context InitThread didn't make: no run, so no grace, and nothing
	// cancels the operation at a stop (its timeouts and retries still end
	// it). go-ycsb's workloads always pass InitThread's context, or one
	// derived from it, so this is for an embedder's direct calls only.
	return context.WithoutCancel(ctx), nil
}

type redisCreator struct{}

func (r redisCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	rds := &redis{endpoints: measurement.EndpointsEnabled()}

	mode := p.GetString(redisMode, redisModeDefault)
	switch mode {
	case "cluster":
		clusterOpts, err := getOptionsCluster(p)
		if err != nil {
			return nil, err
		}
		// on an error newClusterClient has closed its client already
		clusterClient, err := newClusterClient(context.Background(), clusterOpts)
		if err != nil {
			return nil, err
		}
		if p.GetBool(prop.DropData, prop.DropDataDefault) {
			// FlushDB has no key argument, so on a ClusterClient it is
			// routed to a single random master rather than every shard -
			// fan it out to every master explicitly so dropdata=true
			// actually clears the whole cluster, not one node's worth.
			err := clusterClient.ForEachMaster(context.Background(), func(ctx context.Context, master *goredis.Client) error {
				return master.FlushDB(ctx).Err()
			})
			if err != nil {
				clusterClient.Close()
				return nil, err
			}
		}
		rds.client = clusterClient
		if rds.endpoints {
			rds.stopEndpoints = make(chan struct{})
			refreshEndpointInfo(clusterClient, rds.stopEndpoints)
		}
	case "single":
		singleOpts, err := getOptionsSingle(p)
		if err != nil {
			return nil, err
		}
		singleEndpointClient := goredis.NewClient(singleOpts)
		if rds.endpoints {
			singleEndpointClient.AddHook(endpointHook{singleOpts.Addr})
		}
		err = singleEndpointClient.Ping(context.Background()).Err()
		if err != nil {
			singleEndpointClient.Close()
			return nil, err
		}
		rds.client = singleEndpointClient

		if p.GetBool(prop.DropData, prop.DropDataDefault) {
			err := rds.client.FlushDB(context.Background()).Err()
			if err != nil {
				rds.client.Close()
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unknown %s %q: expected \"single\" or \"cluster\"", redisMode, mode)
	}
	rds.mode = mode
	rds.datatype = p.GetString(redisDatatype, redisDatatypeDefault)
	fmt.Println(fmt.Sprintf("Using the redis datatype: %s", rds.datatype))
	rds.fieldcount = p.GetInt64(prop.FieldCount, prop.FieldCountDefault)

	return rds, nil
}

const (
	redisMode                  = "redis.mode"
	redisModeDefault           = "single"
	redisDatatype              = "redis.datatype"
	redisDatatypeDefault       = "hash"
	redisNetwork               = "redis.network"
	redisNetworkDefault        = "tcp"
	redisAddr                  = "redis.addr"
	redisAddrDefault           = "localhost:6379"
	redisUsername              = "redis.username"
	redisPassword              = "redis.password"
	redisDB                    = "redis.db"
	redisMaxRedirects          = "redis.max_redirects"
	redisReadOnly              = "redis.read_only"
	redisRouteByLatency        = "redis.route_by_latency"
	redisRouteRandomly         = "redis.route_randomly"
	redisMaxRetries            = "redis.max_retries"
	redisMinRetryBackoff       = "redis.min_retry_backoff"
	redisMaxRetryBackoff       = "redis.max_retry_backoff"
	redisDialTimeout           = "redis.dial_timeout"
	redisReadTimeout           = "redis.read_timeout"
	redisWriteTimeout          = "redis.write_timeout"
	redisPoolSize              = "redis.pool_size"
	redisPoolSizeDefault       = 0
	redisMinIdleConns          = "redis.min_idle_conns"
	redisMaxIdleConns          = "redis.max_idle_conns"
	redisMaxConnAge            = "redis.max_conn_age"
	redisPoolTimeout           = "redis.pool_timeout"
	redisIdleTimeout           = "redis.idle_timeout"
	redisTLSCA                 = "redis.tls_ca"
	redisTLSCert               = "redis.tls_cert"
	redisTLSKey                = "redis.tls_key"
	redisTLSInsecureSkipVerify = "redis.tls_insecure_skip_verify"
	redisProtocol              = "redis.protocol"
	redisProtocolDefault       = 3
	redisMaintNotifications    = "redis.maint_notifications"
	redisReadBufferSize        = "redis.read_buffer_size"
	redisWriteBufferSize       = "redis.write_buffer_size"
	// go-redis v9.8.0's buffers: bufio's default size
	redisBufferSizeDefault    = 4096
	redisDialerRetries        = "redis.dialer_retries"
	redisDialerRetriesDefault = 1
	redisMaxConcurrentDials   = "redis.max_concurrent_dials"
	redisRoutingPolicies      = "redis.routing_policies"
	// go-redis v9.8.0's defaults
	redisMinRetryBackoffDefault            = 8 * time.Millisecond
	redisMaxRetryBackoffDefault            = 512 * time.Millisecond
	redisReadTimeoutDefault                = 3 * time.Second
	redisClusterStateReloadInterval        = "redis.cluster_state_reload_interval"
	redisClusterStateReloadIntervalDefault = 10 * time.Second
)

// clientBehaviour is how the client talks to Redis where go-redis changed
// its defaults after v9.8.0: by default as v9.8.0 did, so that runs compare
// across go-ycsb builds, and the newer behaviour on request.
type clientBehaviour struct {
	protocol           int
	maintNotifications *maintnotifications.Config
	readBufferSize     int
	writeBufferSize    int
	dialerRetries      int
	maxConcurrentDials int
	routingPolicies    bool
}

func parseClientBehaviour(p *properties.Properties) (clientBehaviour, error) {
	r := &propReader{p: p}
	b := clientBehaviour{
		protocol:           r.int(redisProtocol, redisProtocolDefault),
		readBufferSize:     r.int(redisReadBufferSize, redisBufferSizeDefault),
		writeBufferSize:    r.int(redisWriteBufferSize, redisBufferSizeDefault),
		dialerRetries:      r.int(redisDialerRetries, redisDialerRetriesDefault),
		maxConcurrentDials: r.int(redisMaxConcurrentDials, 0),
		routingPolicies:    r.bool(redisRoutingPolicies, false),
	}
	if r.err != nil {
		return b, r.err
	}
	if b.protocol != 2 && b.protocol != 3 {
		return b, fmt.Errorf("%s must be 2 or 3, got %d", redisProtocol, b.protocol)
	}
	mode := maintnotifications.ModeDisabled
	if v, ok := r.value(redisMaintNotifications); ok {
		mode = maintnotifications.Mode(strings.ToLower(v))
	}
	if !mode.IsValid() {
		return b, fmt.Errorf("%s must be disabled, auto or enabled, got %q", redisMaintNotifications, mode)
	}
	if mode == maintnotifications.ModeEnabled && b.protocol != 3 {
		return b, fmt.Errorf("%s=enabled needs %s=3: go-redis handles maintenance notifications over RESP3 only", redisMaintNotifications, redisProtocol)
	}
	b.maintNotifications = &maintnotifications.Config{Mode: mode}
	if b.readBufferSize <= 0 || b.writeBufferSize <= 0 {
		return b, fmt.Errorf("%s and %s must be > 0, got %d and %d", redisReadBufferSize, redisWriteBufferSize, b.readBufferSize, b.writeBufferSize)
	}
	if b.dialerRetries < 1 {
		return b, fmt.Errorf("%s must be >= 1, got %d", redisDialerRetries, b.dialerRetries)
	}
	if b.maxConcurrentDials < 0 {
		return b, fmt.Errorf("%s must be >= 0, got %d", redisMaxConcurrentDials, b.maxConcurrentDials)
	}
	return b, nil
}

// parseTLS returns (nil, nil) when no TLS property is set at all - the
// caller then dials plaintext, same as before. Any other outcome (a cert
// error, or only one of tls_cert/tls_key set) is returned as an error
// instead of silently falling back to plaintext: dialing unencrypted
// because a cert path was mistyped is a data-in-transit exposure, not a
// condition to swallow.
func parseTLS(p *properties.Properties) (*tls.Config, error) {
	caPath, _ := p.Get(redisTLSCA)
	certPath, _ := p.Get(redisTLSCert)
	keyPath, _ := p.Get(redisTLSKey)
	r := &propReader{p: p}
	insecureSkipVerify := r.bool(redisTLSInsecureSkipVerify, false)
	if r.err != nil {
		return nil, r.err
	}

	if caPath == "" && certPath == "" && keyPath == "" {
		return nil, nil
	}
	if (certPath != "") != (keyPath != "") {
		return nil, fmt.Errorf("%s and %s must be set together", redisTLSCert, redisTLSKey)
	}
	config, err := util.CreateTLSConfig(caPath, certPath, keyPath, insecureSkipVerify)
	if err != nil {
		return nil, fmt.Errorf("invalid redis TLS configuration: %w", err)
	}
	return config, nil
}

// getDuration reads a duration property: a Go duration ("30s", "500ms") or,
// as before, an integer number of nanoseconds. Unset means def; anything else
// is an error, rather than def in silence.
func getDuration(p *properties.Properties, key string, def time.Duration) (time.Duration, error) {
	v, ok := p.Get(key)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return def, nil
	}
	if ns, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(ns), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: want a duration like 30s or 500ms, or an integer number of nanoseconds", key, v)
	}
	return d, nil
}

// propReader reads redis.* properties strictly: a value that doesn't parse
// is an error naming the property (the first one is kept), never the default
// in silence, as magiconair/properties' GetInt/GetBool/GetDuration do.
type propReader struct {
	p   *properties.Properties
	err error
}

func (r *propReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *propReader) value(key string) (string, bool) {
	v, ok := r.p.Get(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (r *propReader) duration(key string, def time.Duration) time.Duration {
	d, err := getDuration(r.p, key, def)
	if err != nil {
		r.fail(err)
	}
	return d
}

func (r *propReader) int(key string, def int) int {
	v, ok := r.value(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.fail(fmt.Errorf("%s=%q: want an integer", key, v))
		return def
	}
	return n
}

// retries reads a retry count: 0 is go-redis's default, -1 none. Below -1
// go-redis would run a command's try loop zero times, so that every command
// "succeeds" without being sent: that is an error.
func (r *propReader) retries(key string) int {
	n := r.int(key, 0)
	if n < -1 {
		r.fail(fmt.Errorf("%s=%d: want -1 (no retries), 0 (go-redis's default) or more", key, n))
		return 0
	}
	return n
}

// bool takes what magiconair/properties takes for true (1, true, yes, on)
// and their opposites for false.
func (r *propReader) bool(key string, def bool) bool {
	v, ok := r.value(key)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	r.fail(fmt.Errorf("%s=%q: want true or false", key, v))
	return def
}

// retryBackoffs reads the retry backoffs. An explicit 0 means go-redis's
// default, which v9.22.0 changed (10 ms / 1 s): it is v9.8.0's 8 ms / 512 ms,
// like the unset default.
func retryBackoffs(r *propReader) (time.Duration, time.Duration) {
	minBackoff := r.duration(redisMinRetryBackoff, redisMinRetryBackoffDefault)
	maxBackoff := r.duration(redisMaxRetryBackoff, redisMaxRetryBackoffDefault)
	if minBackoff == 0 {
		minBackoff = redisMinRetryBackoffDefault
	}
	if maxBackoff == 0 {
		maxBackoff = redisMaxRetryBackoffDefault
	}
	return minBackoff, maxBackoff
}

// newClusterNodeClient makes a cluster node's client as go-redis does, but a
// node read timeout of 0, which a cluster read timeout of -1 gives the nodes
// (the cluster's pipelines then have no deadline), is v9.8.0's 3 s for the
// commands sent on their own, not v9.22.0's 5 s.
func newClusterNodeClient(opt *goredis.Options) *goredis.Client {
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = redisReadTimeoutDefault
	}
	c := goredis.NewClient(opt)
	if measurement.EndpointsEnabled() {
		c.AddHook(endpointHook{opt.Addr})
	}
	return c
}

func getOptionsSingle(p *properties.Properties) (*goredis.Options, error) {
	opts := &goredis.Options{}
	r := &propReader{p: p}

	opts.Addr = p.GetString(redisAddr, redisAddrDefault)
	opts.DB = r.int(redisDB, 0)
	opts.Network = p.GetString(redisNetwork, redisNetworkDefault)
	opts.Username = p.GetString(redisUsername, "")
	opts.Password, _ = p.Get(redisPassword)
	opts.MaxRetries = r.retries(redisMaxRetries)
	opts.MinRetryBackoff, opts.MaxRetryBackoff = retryBackoffs(r)
	opts.DialTimeout = r.duration(redisDialTimeout, time.Second*5)
	opts.ReadTimeout = r.duration(redisReadTimeout, redisReadTimeoutDefault)
	opts.WriteTimeout = r.duration(redisWriteTimeout, opts.ReadTimeout)
	opts.PoolSize = r.int(redisPoolSize, redisPoolSizeDefault)
	if opts.PoolSize < 0 {
		return nil, fmt.Errorf("%s must be >= 0, got %d", redisPoolSize, opts.PoolSize)
	}
	threadCount := p.MustGetInt("threadcount")
	if threadCount <= 0 {
		return nil, fmt.Errorf("threadcount must be > 0, got %d", threadCount)
	}
	if opts.PoolSize == 0 {
		opts.PoolSize = threadCount
		fmt.Println(fmt.Sprintf("Setting %s=%d (from <threadcount>) given you haven't specified a value.", redisPoolSize, opts.PoolSize))
	}
	opts.MinIdleConns = r.int(redisMinIdleConns, opts.PoolSize)
	opts.MaxIdleConns = r.int(redisMaxIdleConns, opts.PoolSize)
	// Since go-redis 9.0.0 the MaxConnAge option was Renamed to ConnMaxLifetime
	// Expired connections may be closed lazily before reuse.
	// If <= 0, connections are not closed due to a connection's age.
	opts.ConnMaxLifetime = r.duration(redisMaxConnAge, -1)
	// Amount of time client waits for connection if all connections
	// are busy before returning an error.
	// Default is ReadTimeout + 1 second.
	opts.PoolTimeout = r.duration(redisPoolTimeout, time.Second+opts.ReadTimeout)
	// an explicit 0 means go-redis's default read timeout: v9.8.0's 3 s, not
	// v9.22.0's 5 s (after the pool timeout, which v9.8.0 took from the 0)
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = redisReadTimeoutDefault
	}
	// Since go-redis 9.0.0 the MaxConnAge option was Renamed to ConnMaxLifetime
	// Expired connections may be closed lazily before reuse.
	// If d <= 0, connections are not closed due to a connection's idle time.
	// -1 disables idle timeout check.
	opts.ConnMaxIdleTime = r.duration(redisIdleTimeout, -1)
	if r.err != nil {
		return nil, r.err
	}
	tlsConfig, err := parseTLS(p)
	if err != nil {
		return nil, err
	}
	opts.TLSConfig = tlsConfig

	b, err := parseClientBehaviour(p)
	if err != nil {
		return nil, err
	}
	if b.routingPolicies {
		fmt.Printf("%s has no effect in single mode (it is for cluster mode)\n", redisRoutingPolicies)
	}
	opts.Protocol = b.protocol
	opts.MaintNotificationsConfig = b.maintNotifications
	opts.ReadBufferSize, opts.WriteBufferSize = b.readBufferSize, b.writeBufferSize
	opts.DialerRetries, opts.MaxConcurrentDials = b.dialerRetries, b.maxConcurrentDials

	return opts, nil
}

func getOptionsCluster(p *properties.Properties) (*goredis.ClusterOptions, error) {
	opts := &goredis.ClusterOptions{}
	r := &propReader{p: p}

	addresses, _ := p.Get(redisAddr)
	opts.Addrs = strings.Split(addresses, ";")
	opts.MaxRedirects = r.retries(redisMaxRedirects)
	opts.ReadOnly = r.bool(redisReadOnly, false)
	opts.RouteByLatency = r.bool(redisRouteByLatency, false)
	opts.RouteRandomly = r.bool(redisRouteRandomly, false)
	opts.Username = p.GetString(redisUsername, "")
	opts.Password, _ = p.Get(redisPassword)
	opts.MaxRetries = r.retries(redisMaxRetries)
	opts.MinRetryBackoff, opts.MaxRetryBackoff = retryBackoffs(r)
	opts.DialTimeout = r.duration(redisDialTimeout, time.Second*5)
	opts.ReadTimeout = r.duration(redisReadTimeout, redisReadTimeoutDefault)
	opts.WriteTimeout = r.duration(redisWriteTimeout, opts.ReadTimeout)
	opts.PoolSize = r.int(redisPoolSize, redisPoolSizeDefault)
	if opts.PoolSize < 0 {
		return nil, fmt.Errorf("%s must be >= 0, got %d", redisPoolSize, opts.PoolSize)
	}
	threadCount := p.MustGetInt("threadcount")
	if threadCount <= 0 {
		return nil, fmt.Errorf("threadcount must be > 0, got %d", threadCount)
	}
	if opts.PoolSize == 0 {
		opts.PoolSize = threadCount
		fmt.Println(fmt.Sprintf("Setting %s=%d (from <threadcount>) given you haven't specified a value.", redisPoolSize, opts.PoolSize))
	}
	opts.MinIdleConns = r.int(redisMinIdleConns, opts.PoolSize)
	opts.MaxIdleConns = r.int(redisMaxIdleConns, opts.PoolSize)
	// Since go-redis 9.0.0 the MaxConnAge option was Renamed to ConnMaxLifetime
	// Expired connections may be closed lazily before reuse.
	// If <= 0, connections are not closed due to a connection's age.
	opts.ConnMaxLifetime = r.duration(redisMaxConnAge, -1)
	// Amount of time client waits for connection if all connections
	// are busy before returning an error.
	// Default is ReadTimeout + 1 second.
	opts.PoolTimeout = r.duration(redisPoolTimeout, time.Second+opts.ReadTimeout)
	// an explicit 0 means go-redis's default read timeout: v9.8.0's 3 s, not
	// v9.22.0's 5 s (after the pool timeout, which v9.8.0 took from the 0)
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = redisReadTimeoutDefault
	}
	// Since go-redis 9.0.0 the MaxConnAge option was Renamed to ConnMaxLifetime
	// Expired connections may be closed lazily before reuse.
	// If d <= 0, connections are not closed due to a connection's idle time.
	// -1 disables idle timeout check.
	opts.ConnMaxIdleTime = r.duration(redisIdleTimeout, -1)
	if r.err != nil {
		return nil, r.err
	}
	tlsConfig, err := parseTLS(p)
	if err != nil {
		return nil, err
	}
	opts.TLSConfig = tlsConfig

	b, err := parseClientBehaviour(p)
	if err != nil {
		return nil, err
	}
	opts.Protocol = b.protocol
	opts.MaintNotificationsConfig = b.maintNotifications
	opts.ReadBufferSize, opts.WriteBufferSize = b.readBufferSize, b.writeBufferSize
	opts.DialerRetries, opts.MaxConcurrentDials = b.dialerRetries, b.maxConcurrentDials
	opts.DisableRoutingPolicies = !b.routingPolicies
	opts.NewClient = newClusterNodeClient
	// go-redis v9.8.0 reloaded the slots every 10 s; v9.22.0's default is 60 s
	if opts.ClusterStateReloadInterval, err = getDuration(p, redisClusterStateReloadInterval, redisClusterStateReloadIntervalDefault); err != nil {
		return nil, err
	}
	if opts.ClusterStateReloadInterval <= 0 {
		return nil, fmt.Errorf("%s must be > 0, got %v", redisClusterStateReloadInterval, opts.ClusterStateReloadInterval)
	}

	return opts, nil
}

func init() {
	ycsb.RegisterDBCreator("redis", redisCreator{})
}

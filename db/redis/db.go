package redis

import (
	"context"
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"time"

	json "github.com/segmentio/encoding/json"

	"github.com/magiconair/properties"
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
}

func (r *redis) Close() error {
	return r.client.Close()
}

func (r *redis) InitThread(ctx context.Context, _ int, _ int) context.Context {
	return ctx
}

func (r *redis) CleanupThread(_ context.Context) {
}

func (r *redis) Read(ctx context.Context, table string, key string, fields []string) (data map[string][]byte, err error) {
	data = make(map[string][]byte, len(fields))
	err = nil
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

func (r *redis) Insert(ctx context.Context, table string, key string, values map[string][]byte) (err error) {
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	switch r.datatype {
	case JSON_DATATYPE:
		err = r.client.Do(ctx, JSON_SET, getKeyName(table, key), ".", string(data)).Err()
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
		err = r.client.Set(ctx, getKeyName(table, key), string(data), 0).Err()
	}
	return
}

func (r *redis) Delete(ctx context.Context, table string, key string) error {
	return r.client.Del(ctx, getKeyName(table, key)).Err()
}

type redisCreator struct{}

func (r redisCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	rds := &redis{}

	mode := p.GetString(redisMode, redisModeDefault)
	switch mode {
	case "cluster":
		clusterOpts, err := getOptionsCluster(p)
		if err != nil {
			return nil, err
		}
		clusterClient := goredis.NewClusterClient(clusterOpts)
		// ReloadState reloads cluster state. It calls ClusterSlots func
		// to get cluster slots information.
		clusterClient.ReloadState(context.Background())
		err = clusterClient.Ping(context.Background()).Err()
		if err != nil {
			clusterClient.Close()
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
	case "single":
		singleOpts, err := getOptionsSingle(p)
		if err != nil {
			return nil, err
		}
		singleEndpointClient := goredis.NewClient(singleOpts)
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
	return goredis.NewClient(opt)
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

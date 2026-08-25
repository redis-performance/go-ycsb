package couchbase

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"sync"
	"time"

	"github.com/couchbase/gocb/v2"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

const (
	couchbaseConnectionString = "couchbase.connection_string"
	couchbaseUsername         = "couchbase.username"
	couchbasePassword         = "couchbase.password"
	couchbaseBucket           = "couchbase.bucket"
	couchbaseScope            = "couchbase.scope"

	couchbaseConnectionStringDefault = "couchbase://127.0.0.1"
	couchbaseUsernameDefault         = "Administrator"
	couchbasePasswordDefault         = "password"
	couchbaseBucketDefault           = "ycsb"
	couchbaseScopeDefault            = "_default"

	couchbaseTLSSkipVerify = "couchbase.tls_skip_verify"
	couchbaseTLSCAFile     = "couchbase.tls_ca_file"

	// couchbaseDurability: "none" (default), "majority",
	// "majorityandpersistactive", or "persisttomajority" - the synchronous
	// replication level required before a write (Insert/Update/Delete) is
	// acknowledged. This is Couchbase's equivalent of MongoDB's write
	// concern. There is no matching "read from majority" knob: unlike a
	// MongoDB replica set, a Couchbase KV Get always reads from the single
	// active node that owns the key, which is by definition the most
	// up-to-date copy - there is no secondary-routing ambiguity to resolve.
	couchbaseDurability        = "couchbase.durability"
	couchbaseDurabilityDefault = "none"

	// couchbaseKVTimeout bounds every point KV op (Get/Insert/Replace/Remove)
	// so a stalled connection (a network partition, a paused node) fails the
	// operation instead of hanging the worker goroutine forever. Defaults to
	// gocb's own default (2.5s) when unset. Scan is bounded separately by
	// couchbaseScanTimeout: it is a fundamentally slower, heavier op (see
	// that constant's doc) and reusing a ~2.5s point-op budget for it would
	// make every Scan fail by default.
	couchbaseKVTimeout = "couchbase.kv_timeout"

	// couchbaseScanTimeout bounds Scan specifically. Defaults to 30s, well
	// above gocb's own internal 10s default: under concurrent range scans
	// against a single-node cluster, this adapter has observed gocb's
	// result-stream draining (res.Next()) block well past its configured
	// timeout instead of actually erroring out - see the Scan method's doc
	// comment. This adapter enforces the bound itself rather than trusting
	// gocb to.
	couchbaseScanTimeout        = "couchbase.scan_timeout"
	couchbaseScanTimeoutDefault = 30 * time.Second

	// couchbaseAutoCreateCollection: when true (default), a table name not
	// yet present as a collection under couchbase.scope is created on first
	// use instead of failing every op with "collection not found". Every
	// bucket always has a ready-to-use "_default" collection, so this only
	// matters for a non-default couchbase.scope or a table name other than
	// "_default". Best-effort: on a target where the configured credentials
	// lack the Manage Collections privilege (common for a least-privilege
	// Capella database credential), creation fails and the adapter reports
	// that clearly instead of silently ignoring it - the collection must
	// then be created out of band before this adapter can be used against it.
	couchbaseAutoCreateCollection        = "couchbase.auto_create_collection"
	couchbaseAutoCreateCollectionDefault = true
)

type couchbaseDB struct {
	cluster *gocb.Cluster
	bucket  *gocb.Bucket
	scope   string

	autoCreateCollection bool
	durability           gocb.DurabilityLevel
	kvTimeout            time.Duration
	scanTimeout          time.Duration

	// collections caches table -> *gocb.Collection. A sync.Map, not a
	// mutex-guarded map: this is looked up on every single op (Read/Scan/
	// Insert/Update/Delete), and every one of go-ycsb's worker goroutines
	// hits it concurrently at full throttle - a shared Mutex here would
	// serialize the whole adapter behind lock contention on the hot path,
	// the same class of bottleneck the measurement path itself was recently
	// reworked to remove (see the "Improve measurement performance by
	// removing contention" and "fix histogram race, improve pooling"
	// commits). The rare first-use-per-table race (several goroutines
	// resolving an uncached table at once) just means ensureCollection runs
	// redundantly a few times; it's idempotent (ErrScopeExists/
	// ErrCollectionExists are treated as success), so that's harmless.
	collections sync.Map
}

func (db *couchbaseDB) Close() error {
	return db.cluster.Close(nil)
}

func (db *couchbaseDB) InitThread(ctx context.Context, threadID int, threadCount int) context.Context {
	return ctx
}

func (db *couchbaseDB) CleanupThread(ctx context.Context) {
}

// getCollection resolves table to a *gocb.Collection, creating the scope and
// collection on first use if they don't exist yet and couchbaseAutoCreateCollection
// is set. Results are cached in db.collections: CollectionsV2().CreateCollection is a
// management-plane call and must not run on every op. ctx is the caller's
// per-op context - only used on a cache miss (ensureCollection's management
// calls and readiness probe), since a cache hit does no I/O at all.
func (db *couchbaseDB) getCollection(ctx context.Context, table string) (*gocb.Collection, error) {
	if v, ok := db.collections.Load(table); ok {
		return v.(*gocb.Collection), nil
	}

	if db.autoCreateCollection {
		if err := db.ensureCollection(ctx, table); err != nil {
			return nil, err
		}
	}

	col := db.bucket.Scope(db.scope).Collection(table)
	db.collections.Store(table, col)
	return col, nil
}

// ensureCollection creates db.scope and the table collection if missing, then
// waits for the collection to become usable: collection creation is a
// management-plane call and the KV routing manifest on each node picks it up
// asynchronously, so an op sent immediately after creation can still fail
// with ErrCollectionNotFound for a short window.
//
// gocb dispatches each management HTTP request to a randomly chosen cluster
// node, independently per call - so on a real multi-node cluster, the
// CreateCollection call below can land on a different node than the
// CreateScope call just above it, one that hasn't yet replicated the new
// scope, and get back ErrScopeNotFound even though the scope now genuinely
// exists. That's retried the same way ErrCollectionNotFound is retried in
// the readiness probe below, rather than treated as fatal.
func (db *couchbaseDB) ensureCollection(ctx context.Context, table string) error {
	mgr := db.bucket.CollectionsV2()
	deadline := time.Now().Add(15 * time.Second)

	if db.scope != couchbaseScopeDefault {
		if err := mgr.CreateScope(db.scope, &gocb.CreateScopeOptions{Context: ctx}); err != nil && !errors.Is(err, gocb.ErrScopeExists) {
			return fmt.Errorf("couchbase: failed to create scope %q (grant Manage Collections, or pre-create it): %w", db.scope, err)
		}
	}

	for {
		err := mgr.CreateCollection(db.scope, table, nil, &gocb.CreateCollectionOptions{Context: ctx})
		if err == nil || errors.Is(err, gocb.ErrCollectionExists) {
			break
		}
		if errors.Is(err, gocb.ErrScopeNotFound) && time.Now().Before(deadline) {
			if waitErr := sleepOrDone(ctx, 200*time.Millisecond); waitErr != nil {
				return waitErr
			}
			continue
		}
		return fmt.Errorf("couchbase: failed to create collection %q.%q (grant Manage Collections, or pre-create it, or set %s=false): %w", db.scope, table, couchbaseAutoCreateCollection, err)
	}

	col := db.bucket.Scope(db.scope).Collection(table)
	probeKey := "__go-ycsb_collection_ready_probe__"
	for {
		_, err := col.Exists(probeKey, &gocb.ExistsOptions{Context: ctx})
		if err == nil {
			return nil
		}
		if !errors.Is(err, gocb.ErrCollectionNotFound) {
			return fmt.Errorf("couchbase: checking readiness of collection %q.%q: %w", db.scope, table, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("couchbase: collection %q.%q was created but did not become ready within 15s", db.scope, table)
		}
		if waitErr := sleepOrDone(ctx, 200*time.Millisecond); waitErr != nil {
			return waitErr
		}
	}
}

// sleepOrDone waits out d, or returns ctx's error early if ctx is cancelled
// first - used by ensureCollection's retry loops so a cancelled caller
// context (e.g. go-ycsb shutting down on SIGINT) doesn't leave a worker
// goroutine sleeping through the loop's full 15s deadline regardless.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Read a document. fields is applied client-side: gocb's server-side
// subdocument projection caps out at 16 paths, well under the feature-store
// workload's 50 fields, so a plain full-document Get is used instead.
func (db *couchbaseDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	col, err := db.getCollection(ctx, table)
	if err != nil {
		return nil, err
	}

	res, err := col.Get(key, &gocb.GetOptions{Timeout: db.kvTimeout, Context: ctx})
	if err != nil {
		return nil, fmt.Errorf("Read error: %s", err.Error())
	}

	var doc map[string][]byte
	if err := res.Content(&doc); err != nil {
		return nil, fmt.Errorf("Read error: %s", err.Error())
	}

	if len(fields) == 0 {
		return doc, nil
	}
	filtered := make(map[string][]byte, len(fields))
	for _, f := range fields {
		if v, ok := doc[f]; ok {
			filtered[f] = v
		}
	}
	return filtered, nil
}

// Scan documents via a KV range scan (Couchbase Server 7.6+/gocb v2.7+, no
// secondary index required). The stream is not guaranteed globally
// key-ordered across the bucket's partitions - only within each partition -
// which matches the best-effort ordering YCSB's own workload generator
// already tolerates from a Scan.
//
// gocb's own docs already say this API is meant "for low concurrency batch
// queries where latency is not critical." Taken literally: running Scan
// concurrently from many workload threads against the same collection is
// outside its intended use, and this adapter has observed exactly that
// abuse produce real trouble (empirically, against Couchbase Server 7.6
// Community Edition) - res.Next() blocking well past its own configured
// timeout, apparently stalled inside gocb/gocbcore's retry handling rather
// than actually erroring out. A high scanproportion workload against
// Couchbase should therefore keep threadcount low; this adapter cannot make
// that safe on the caller's behalf, only bound how long a stuck call can
// block one worker goroutine for (see below).
//
// Both col.Scan() itself AND the result-stream draining run inside a single
// background goroutine, raced against scanCtx's own deadline via a select -
// not just gocb's ScanOptions.Timeout, which the misbehavior above showed
// isn't sufficient by itself. col.Scan() has to be inside that goroutine,
// not just the draining loop after it: gocb's rangeScanOpManager applies the
// caller's context/deadline only to the very first internal request that
// creates the scan stream, then deliberately switches to context.Background()
// for every subsequent request needed to fetch results - including the ones
// producing the first item col.Scan() itself blocks on before it can even
// return a *ScanResult. A stall in one of those later requests is therefore
// invisible to scanCtx and can block col.Scan() itself forever - so if this
// goroutine only started after col.Scan() returned, that call could still
// hang the calling worker goroutine indefinitely, defeating the whole point
// of this method having a timeout at all.
//
// This can only bound how long the CALLING goroutine blocks, not the spawned
// one: if col.Scan() itself is the thing wedged (no *ScanResult exists yet),
// there is no handle for this method to call Close() on to force it to
// unblock, so on that specific timeout path the background goroutine leaks
// permanently, still blocked inside gocb. That's an accepted, documented
// consequence of a real gap in gocb's own cancellation model, not something
// fixable purely from this side of the client - see the note on Scan
// concurrency in the README's Couchbase section.
func (db *couchbaseDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	col, err := db.getCollection(ctx, table)
	if err != nil {
		return nil, err
	}

	scanTimeout := db.scanTimeout
	scanCtx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()

	scanType := gocb.RangeScan{
		From: &gocb.ScanTerm{Term: startKey},
		To:   gocb.ScanTermMaximum(),
	}

	type scanResult struct {
		docs []map[string][]byte
		err  error
	}
	resultCh := make(chan scanResult, 1)
	go func() {
		res, err := col.Scan(scanType, &gocb.ScanOptions{Timeout: scanTimeout, Context: scanCtx})
		if err != nil {
			resultCh <- scanResult{err: err}
			return
		}
		defer res.Close()

		docs := make([]map[string][]byte, 0, count)
		for len(docs) < count {
			item := res.Next()
			if item == nil {
				break
			}
			var doc map[string][]byte
			if err := item.Content(&doc); err != nil {
				resultCh <- scanResult{err: err}
				return
			}
			if len(fields) > 0 {
				filtered := make(map[string][]byte, len(fields))
				for _, f := range fields {
					if v, ok := doc[f]; ok {
						filtered[f] = v
					}
				}
				doc = filtered
			}
			docs = append(docs, doc)
		}
		resultCh <- scanResult{docs: docs, err: res.Err()}
	}()

	select {
	case r := <-resultCh:
		if r.err != nil {
			return nil, fmt.Errorf("Scan error: %s", r.err.Error())
		}
		return r.docs, nil
	case <-scanCtx.Done():
		return nil, fmt.Errorf("Scan error: timed out after %s: %w", scanTimeout, scanCtx.Err())
	}
}

// Insert a document. Uses gocb's dedicated Insert (not Upsert) so a
// duplicate key during the load phase surfaces as ErrDocumentExists instead
// of silently overwriting, mirroring the other adapters' Insert semantics.
func (db *couchbaseDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	col, err := db.getCollection(ctx, table)
	if err != nil {
		return err
	}
	_, err = col.Insert(key, values, &gocb.InsertOptions{
		DurabilityLevel: db.durability,
		Timeout:         db.kvTimeout,
		Context:         ctx,
	})
	if err != nil {
		return fmt.Errorf("Insert error: %s", err.Error())
	}
	return nil
}

// Update a document. Uses Replace, which fails with ErrDocumentNotFound
// against a missing key instead of silently creating one.
func (db *couchbaseDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	col, err := db.getCollection(ctx, table)
	if err != nil {
		return err
	}
	_, err = col.Replace(key, values, &gocb.ReplaceOptions{
		DurabilityLevel: db.durability,
		Timeout:         db.kvTimeout,
		Context:         ctx,
	})
	if err != nil {
		return fmt.Errorf("Update error: %s", err.Error())
	}
	return nil
}

// Delete a document.
func (db *couchbaseDB) Delete(ctx context.Context, table string, key string) error {
	col, err := db.getCollection(ctx, table)
	if err != nil {
		return err
	}
	_, err = col.Remove(key, &gocb.RemoveOptions{
		DurabilityLevel: db.durability,
		Timeout:         db.kvTimeout,
		Context:         ctx,
	})
	if err != nil {
		return fmt.Errorf("Delete error: %s", err.Error())
	}
	return nil
}

type couchbaseCreator struct{}

func parseDurability(p *properties.Properties) (gocb.DurabilityLevel, error) {
	switch strings.ToLower(p.GetString(couchbaseDurability, couchbaseDurabilityDefault)) {
	case "none":
		return gocb.DurabilityLevelNone, nil
	case "majority":
		return gocb.DurabilityLevelMajority, nil
	case "majorityandpersistactive":
		return gocb.DurabilityLevelMajorityAndPersistOnMaster, nil
	case "persisttomajority":
		return gocb.DurabilityLevelPersistToMajority, nil
	default:
		return gocb.DurabilityLevelNone, fmt.Errorf("unknown %s %q: expected none, majority, majorityAndPersistActive, or persistToMajority", couchbaseDurability, p.GetString(couchbaseDurability, ""))
	}
}

func (c couchbaseCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	connStr := p.GetString(couchbaseConnectionString, couchbaseConnectionStringDefault)
	username := p.GetString(couchbaseUsername, couchbaseUsernameDefault)
	password := p.GetString(couchbasePassword, couchbasePasswordDefault)
	bucketName := p.GetString(couchbaseBucket, couchbaseBucketDefault)
	scope := p.GetString(couchbaseScope, couchbaseScopeDefault)
	autoCreate := p.GetBool(couchbaseAutoCreateCollection, couchbaseAutoCreateCollectionDefault)

	durability, err := parseDurability(p)
	if err != nil {
		return nil, err
	}

	var kvTimeout time.Duration
	if kvTimeoutStr, ok := p.Get(couchbaseKVTimeout); ok {
		kvTimeout, err = time.ParseDuration(kvTimeoutStr)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", couchbaseKVTimeout, kvTimeoutStr, err)
		}
	}

	scanTimeout := couchbaseScanTimeoutDefault
	if scanTimeoutStr, ok := p.Get(couchbaseScanTimeout); ok {
		scanTimeout, err = time.ParseDuration(scanTimeoutStr)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", couchbaseScanTimeout, scanTimeoutStr, err)
		}
	}

	opts := gocb.ClusterOptions{
		Authenticator: gocb.PasswordAuthenticator{Username: username, Password: password},
	}

	tlsSkipVerify := p.GetBool(couchbaseTLSSkipVerify, false)
	caFile := p.GetString(couchbaseTLSCAFile, "")
	// Capella (couchbases://) requires TLS and ships publicly-trusted
	// certificates by default, so no CA configuration is needed for it in
	// the common case - couchbase.tls_ca_file/tls_skip_verify exist for a
	// self-managed cluster's private CA or, for skip_verify, local testing
	// against a TLS-terminating proxy the way db/aerospike's test does.
	//
	// gocb only ever consults SecurityConfig.TLSSkipVerify/TLSRootCAs when
	// the connection string's scheme itself requests TLS (couchbases://) -
	// there is no independent option to force it on. Without this check, an
	// operator who sets couchbase.tls_ca_file but leaves
	// couchbase.connection_string at the plain couchbase:// default (or
	// mistypes it) gets a silent, unencrypted connection: the CA file is
	// read, parsed, and then simply never used, with no error or log line.
	if (tlsSkipVerify || caFile != "") && !strings.HasPrefix(connStr, "couchbases://") {
		return nil, fmt.Errorf("%s/%s requires a couchbases:// connection string (TLS) - %s is %q", couchbaseTLSSkipVerify, couchbaseTLSCAFile, couchbaseConnectionString, connStr)
	}
	if tlsSkipVerify {
		opts.SecurityConfig.TLSSkipVerify = true
	}
	if caFile != "" {
		caCert, err := ioutil.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", couchbaseTLSCAFile, err)
		}
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return nil, fmt.Errorf("%s %q: certificate could not be parsed", couchbaseTLSCAFile, caFile)
		}
		opts.SecurityConfig.TLSRootCAs = caCertPool
	}

	cluster, err := gocb.Connect(connStr, opts)
	if err != nil {
		return nil, err
	}

	bucket := cluster.Bucket(bucketName)
	if err := bucket.WaitUntilReady(15*time.Second, nil); err != nil {
		// Connect() already established the connection manager's background
		// goroutines/sockets by this point; without Close() here, a failed
		// WaitUntilReady (wrong bucket name, not yet provisioned, a
		// transient hiccup) leaks them for the life of the process, since
		// `cluster` is a local variable nothing else can ever reach again.
		cluster.Close(nil)
		return nil, fmt.Errorf("couchbase: bucket %q not ready (does it exist?): %w", bucketName, err)
	}
	fmt.Printf("Connected to Couchbase! Using bucket %q, scope %q\n", bucketName, scope)

	db := &couchbaseDB{
		cluster:              cluster,
		bucket:               bucket,
		scope:                scope,
		autoCreateCollection: autoCreate,
		durability:           durability,
		kvTimeout:            kvTimeout,
		scanTimeout:          scanTimeout,
	}
	return db, nil
}

func init() {
	ycsb.RegisterDBCreator("couchbase", couchbaseCreator{})
}

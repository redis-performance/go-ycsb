package cosmosdb

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

const (
	cosmosEndpoint         = "cosmosdb.endpoint"
	cosmosKey              = "cosmosdb.key"
	cosmosConnectionString = "cosmosdb.connection_string"
	cosmosDatabase         = "cosmosdb.database"
	cosmosDatabaseDefault  = "ycsb"

	// cosmosPartitionKeyPath is the JSON path Cosmos DB uses to route a
	// document to a logical partition. Defaulting it to "/id" - the same
	// property every document is keyed by - gives one logical partition per
	// record: the natural mapping for a point-read/point-write KV workload
	// like YCSB's, and the only way to avoid adding artificial
	// cross-partition query overhead this adapter doesn't otherwise need.
	// It also means Scan (see below) is unavoidably cross-partition, and a
	// skewed (zipfian/hotspot) access pattern can genuinely hot-partition
	// under Cosmos DB's consistent-hashing of partition key values onto
	// physical partitions - that's real Cosmos DB behavior under skew, not
	// a benchmark artifact, as long as the partition key stays the record
	// key rather than something synthetically spread out to dodge it.
	cosmosPartitionKeyPath        = "cosmosdb.partition_key_path"
	cosmosPartitionKeyPathDefault = "/id"

	// cosmosConsistencyLevel is a per-operation override - gocb-adjacent
	// adapters in this repo (couchbase.durability) can set a property that
	// unilaterally strengthens a guarantee, but Cosmos DB's SDK explicitly
	// only allows RELAXING consistency below the account's own configured
	// default (see ItemOptions.ConsistencyLevel's doc). There is no way for
	// this adapter, or any client, to request stronger consistency than the
	// account itself was provisioned with. If you want Strong consistency
	// end to end, the Cosmos DB ACCOUNT must be configured with Strong as
	// its default - this property cannot substitute for that.
	cosmosConsistencyLevel = "cosmosdb.consistency_level"

	// cosmosAutoCreateContainer defaults to false - unlike
	// couchbase.auto_create_collection (true by default), because creating
	// a Cosmos DB database/container provisions real, billed throughput
	// (RU/s). Auto-creating that as a side effect of a typo'd
	// cosmosdb.database/table name is a real-money footgun a local/free
	// database like Couchbase CE doesn't have, so this adapter requires an
	// explicit opt-in instead of Couchbase's best-effort-by-default.
	cosmosAutoCreateContainer        = "cosmosdb.auto_create_container"
	cosmosAutoCreateContainerDefault = false

	// cosmosThroughput / cosmosAutoscaleMaxThroughput: only consulted when
	// cosmosAutoCreateContainer=true. If neither is set, autoCreate falls
	// back to manual throughput at cosmosThroughputDefault (400 RU/s is
	// Cosmos DB's own platform minimum for manual provisioning).
	cosmosThroughput             = "cosmosdb.throughput"
	cosmosThroughputDefault      = int32(400)
	cosmosAutoscaleMaxThroughput = "cosmosdb.autoscale_max_throughput"
	cosmosOpTimeout              = "cosmosdb.op_timeout"
	cosmosOpTimeoutDefault       = 10 * time.Second

	// cosmosScanTimeout bounds a whole Scan call, not a single request:
	// Scan pages through a cross-partition query via repeated
	// pager.NextPage calls until count items are collected, so its total
	// duration is a multiple of a single point-operation's, and reusing
	// cosmosOpTimeout for the whole thing (as this adapter's Read/Insert/
	// Update/Delete correctly do for their own single request) starves
	// later pages of an already-succeeding scan under go-ycsb's own
	// defaults (maxscanlength=1000 at roughly 100 items/page, and a
	// freshly auto-created container's 400 RU/s default throughput).
	cosmosScanTimeout        = "cosmosdb.scan_timeout"
	cosmosScanTimeoutDefault = 60 * time.Second

	cosmosInsecureSkipVerify = "cosmosdb.insecure_skip_verify"
)

// cosmosMaxPatchOps is Cosmos DB's hard cap on the number of operations in a
// single PatchItem request (https://learn.microsoft.com/azure/cosmos-db/partial-document-update).
// Update() uses this as a fallback trigger the same way db/couchbase/db.go's
// couchbaseMaxSubdocOps does: go-ycsb's core workload defaults to
// writeallfields=false (a single field per Update), staying under this cap,
// but a workload with writeallfields=true and more than 10 fields hits the
// fallback for real.
const cosmosMaxPatchOps = 10

// cosmosUpdateEtagRetries bounds the fallback Read+merge+Replace path's
// optimistic-concurrency retries (ETag-based, Cosmos DB's equivalent of a
// CAS token) - see Update's doc comment for why retrying instead of failing
// on the first conflict matters for go-ycsb's own hotspot/zipfian key
// distributions.
const cosmosUpdateEtagRetries = 5

// jsonPointerEscape escapes a field name for use as a Cosmos DB
// PatchOperations path, which follows RFC 6901 JSON Pointer syntax: '/'
// separates path segments and must be escaped as "~1", and a literal '~'
// must be escaped as "~0" so it isn't read as the start of an escape
// sequence itself. Unlike Couchbase's N1QL-style subdocument paths (where
// '.' also has to be treated specially), JSON Pointer only reserves these
// two characters, and the escape is simple enough to apply directly rather
// than needing a "give up and fall back" safety net - see
// db/couchbase/db.go's fieldPathSafe for the case where escaping was judged
// too risky to implement instead of avoided.
var jsonPointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// cosmosSystemFields are properties Cosmos DB adds to every document it
// returns (in addition to "id", which this adapter itself injects - see
// Insert) that are not part of the record's own fields and must be
// stripped out before a Read/Scan result is handed back to the caller.
var cosmosSystemFields = map[string]bool{
	"id": true, "_rid": true, "_self": true, "_etag": true, "_attachments": true, "_ts": true,
}

// rejectSystemFieldNames errors out if values contains a field name Cosmos
// DB reserves for its own system properties (see cosmosSystemFields).
// Writing such a field would be silently discarded/overwritten server-side,
// and decodeDoc strips it from every later Read/Scan regardless - so
// without this check, a workload field that happens to collide with one of
// these names loses its data permanently with no error anywhere in the
// pipeline.
func rejectSystemFieldNames(values map[string][]byte) error {
	for field := range values {
		if cosmosSystemFields[field] {
			return fmt.Errorf("field name %q collides with a Cosmos DB system property and cannot be used", field)
		}
	}
	return nil
}

type cosmosDB struct {
	client         *azcosmos.Client
	database       *azcosmos.DatabaseClient
	partitionPath  string
	autoCreate     bool
	throughputOpts *azcosmos.ThroughputProperties
	opTimeout      time.Duration
	scanTimeout    time.Duration
	consistency    *azcosmos.ConsistencyLevel

	// containers caches table -> *azcosmos.ContainerClient. A sync.Map, not
	// a mutex-guarded map, for the same reason as db/couchbase/db.go's
	// collections cache: this is looked up on every single op from every
	// worker goroutine, and a shared Mutex here would serialize the whole
	// adapter behind lock contention on the hot path.
	containers sync.Map
}

func (db *cosmosDB) Close() error {
	db.client.Close()
	return nil
}

func (db *cosmosDB) InitThread(ctx context.Context, threadID int, threadCount int) context.Context {
	return ctx
}

func (db *cosmosDB) CleanupThread(ctx context.Context) {
}

// getContainer resolves table to a *azcosmos.ContainerClient, creating the
// container on first use if cosmosAutoCreateContainer is set. Results are
// cached: container creation provisions billed throughput and must not be
// attempted on every op.
func (db *cosmosDB) getContainer(ctx context.Context, table string) (*azcosmos.ContainerClient, error) {
	if v, ok := db.containers.Load(table); ok {
		return v.(*azcosmos.ContainerClient), nil
	}

	if db.autoCreate {
		opCtx, cancel := context.WithTimeout(ctx, db.opTimeout)
		defer cancel()
		_, err := db.database.CreateContainer(opCtx, azcosmos.ContainerProperties{
			ID: table,
			PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{
				Paths: []string{db.partitionPath},
			},
		}, &azcosmos.CreateContainerOptions{ThroughputProperties: db.throughputOpts})
		if err != nil {
			var respErr *azcore.ResponseError
			if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusConflict {
				return nil, fmt.Errorf("cosmosdb: failed to create container %q (grant Manage privileges, or pre-create it, or set %s=false): %w", table, cosmosAutoCreateContainer, err)
			}
		}
	}

	container, err := db.client.NewContainer(db.database.ID(), table)
	if err != nil {
		return nil, fmt.Errorf("cosmosdb: %w", err)
	}
	db.containers.Store(table, container)
	return container, nil
}

// docPartitionKey derives the partition key value for key. Since
// cosmosPartitionKeyPathDefault ("/id") is a direct alias for the
// document's own id, and this adapter always sets id=key (see Insert), the
// partition key value is simply key itself for the default configuration.
// A non-default cosmosdb.partition_key_path pointing anywhere other than
// "/id" is not supported by this adapter - see the README note - so this
// intentionally does not attempt to resolve an arbitrary path.
func docPartitionKey(key string) azcosmos.PartitionKey {
	return azcosmos.NewPartitionKeyString(key)
}

// decodeDoc parses a raw Cosmos DB document body into the caller's
// map[string][]byte shape, stripping "id" and every Cosmos-injected system
// property (_rid, _self, _etag, _attachments, _ts) - those aren't part of
// the record the caller wrote and must not leak into a Read/Scan result.
func decodeDoc(raw []byte) (map[string][]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	doc := make(map[string][]byte, len(fields))
	for k, v := range fields {
		if cosmosSystemFields[k] {
			continue
		}
		var b []byte
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		doc[k] = b
	}
	return doc, nil
}

func filterFields(doc map[string][]byte, fields []string) map[string][]byte {
	if len(fields) == 0 {
		return doc
	}
	filtered := make(map[string][]byte, len(fields))
	for _, f := range fields {
		if v, ok := doc[f]; ok {
			filtered[f] = v
		}
	}
	return filtered
}

// Read a document. fields is applied client-side: ReadItem has no
// projection support (Cosmos DB only offers field projection via SQL
// queries, not point reads), so filtering happens after decoding the full
// document, the same approach db/couchbase/db.go takes for the same reason.
func (db *cosmosDB) Read(ctx context.Context, table string, key string, fields []string) (map[string][]byte, error) {
	container, err := db.getContainer(ctx, table)
	if err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(ctx, db.opTimeout)
	defer cancel()
	res, err := container.ReadItem(opCtx, docPartitionKey(key), key, db.itemOptions())
	if err != nil {
		return nil, fmt.Errorf("Read error: %s", err.Error())
	}

	doc, err := decodeDoc(res.Value)
	if err != nil {
		return nil, fmt.Errorf("Read error: %s", err.Error())
	}
	return filterFields(doc, fields), nil
}

// Scan documents via a cross-partition SQL query. This is unavoidably
// cross-partition given cosmosPartitionKeyPathDefault (see its doc comment):
// with one logical partition per record, no single-partition query could
// ever span more than one record. Cross-partition queries are real,
// supported Cosmos DB functionality (via azcosmos.NewPartitionKey(), an
// empty partition key that tells the query engine to scope via the query's
// own WHERE clause instead) but are inherently costlier in RU and latency
// than a point read - keep that in mind for a scan-heavy workload the same
// way db/couchbase/db.go's README note does for Couchbase's Scan.
func (db *cosmosDB) Scan(ctx context.Context, table string, startKey string, count int, fields []string) ([]map[string][]byte, error) {
	container, err := db.getContainer(ctx, table)
	if err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(ctx, db.scanTimeout)
	defer cancel()

	opts := &azcosmos.QueryOptions{
		QueryParameters: []azcosmos.QueryParameter{{Name: "@start", Value: startKey}},
	}
	if db.consistency != nil {
		opts.ConsistencyLevel = db.consistency
	}
	pager := container.NewQueryItemsPager("SELECT * FROM c WHERE c.id >= @start ORDER BY c.id", azcosmos.NewPartitionKey(), opts)

	docs := make([]map[string][]byte, 0, count)
	for len(docs) < count && pager.More() {
		page, err := pager.NextPage(opCtx)
		if err != nil {
			return nil, fmt.Errorf("Scan error: %s", err.Error())
		}
		for _, item := range page.Items {
			if len(docs) >= count {
				break
			}
			doc, err := decodeDoc(item)
			if err != nil {
				return nil, fmt.Errorf("Scan error: %s", err.Error())
			}
			docs = append(docs, filterFields(doc, fields))
		}
	}
	return docs, nil
}

// Insert a document. Uses CreateItem, which fails with a 409 Conflict
// against a duplicate id instead of silently overwriting, mirroring the
// other adapters' Insert semantics (Couchbase's dedicated Insert vs.
// Upsert, Mongo's InsertOne).
func (db *cosmosDB) Insert(ctx context.Context, table string, key string, values map[string][]byte) error {
	if err := rejectSystemFieldNames(values); err != nil {
		return fmt.Errorf("Insert error: %s", err.Error())
	}
	container, err := db.getContainer(ctx, table)
	if err != nil {
		return err
	}

	doc := make(map[string]interface{}, len(values)+1)
	doc["id"] = key
	for field, value := range values {
		doc[field] = value
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("Insert error: %s", err.Error())
	}

	opCtx, cancel := context.WithTimeout(ctx, db.opTimeout)
	defer cancel()
	if _, err := container.CreateItem(opCtx, docPartitionKey(key), body, db.itemOptions()); err != nil {
		return fmt.Errorf("Insert error: %s", err.Error())
	}
	return nil
}

// canUsePatchUpdate reports whether values is small enough to write via a
// single atomic PatchItem call. A pure function - not inlined into Update -
// so this branch-selection logic can be unit-tested directly without a live
// Cosmos DB connection, matching db/couchbase/db.go's canUseSubdocUpdate.
func canUsePatchUpdate(values map[string][]byte) bool {
	return len(values) <= cosmosMaxPatchOps
}

// Update a document. Per the ycsb.DB interface contract, Update must merge
// values into the existing document - fields not mentioned must survive
// untouched - not replace the document wholesale (see db/couchbase/db.go's
// Update doc comment for the real data-corruption bug that comes from
// getting this wrong). For a values map within Cosmos DB's per-request
// patch-operation limit (see cosmosMaxPatchOps), this is done atomically
// and in one round trip via PatchItem/AppendSet, one operation per field:
// each sets exactly that top-level field, leaving every other field alone,
// with no read-modify-write race window.
//
// For a wider values map (reachable in practice with writeallfields=true
// against a table with more than cosmosMaxPatchOps fields), it falls back
// to Read+merge+Replace, using the Read's ETag to detect a concurrent write
// landing in between (Cosmos DB's IfMatchEtag is its equivalent of a CAS
// token) rather than silently losing it. A worker retries this loop up to
// cosmosUpdateEtagRetries times on a 412 Precondition Failed before giving
// up: with go-ycsb's own hotspot/zipfian key distributions and enough
// worker threads, two Updates landing on the same key at nearly the same
// time is an expected, not exceptional, occurrence.
func (db *cosmosDB) Update(ctx context.Context, table string, key string, values map[string][]byte) error {
	if err := rejectSystemFieldNames(values); err != nil {
		return fmt.Errorf("Update error: %s", err.Error())
	}
	container, err := db.getContainer(ctx, table)
	if err != nil {
		return err
	}

	if canUsePatchUpdate(values) {
		ops := azcosmos.PatchOperations{}
		for field, value := range values {
			ops.AppendSet("/"+jsonPointerEscaper.Replace(field), value)
		}
		opCtx, cancel := context.WithTimeout(ctx, db.opTimeout)
		defer cancel()
		if _, err := container.PatchItem(opCtx, docPartitionKey(key), key, ops, db.itemOptions()); err != nil {
			return fmt.Errorf("Update error: %s", err.Error())
		}
		return nil
	}

	var lastErr error
	for attempt := 0; attempt <= cosmosUpdateEtagRetries; attempt++ {
		readCtx, readCancel := context.WithTimeout(ctx, db.opTimeout)
		res, err := container.ReadItem(readCtx, docPartitionKey(key), key, db.itemOptions())
		readCancel()
		if err != nil {
			return fmt.Errorf("Update error: %s", err.Error())
		}

		doc, err := decodeDoc(res.Value)
		if err != nil {
			return fmt.Errorf("Update error: %s", err.Error())
		}
		for field, value := range values {
			doc[field] = value
		}
		body := make(map[string]interface{}, len(doc)+1)
		body["id"] = key
		for field, value := range doc {
			body[field] = value
		}
		marshalled, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("Update error: %s", err.Error())
		}

		itemOpts := db.itemOptions()
		itemOpts.IfMatchEtag = &res.ETag
		replaceCtx, replaceCancel := context.WithTimeout(ctx, db.opTimeout)
		_, err = container.ReplaceItem(replaceCtx, docPartitionKey(key), key, marshalled, itemOpts)
		replaceCancel()
		if err == nil {
			return nil
		}
		var respErr *azcore.ResponseError
		if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusPreconditionFailed {
			return fmt.Errorf("Update error: %s", err.Error())
		}
		lastErr = err
	}
	return fmt.Errorf("Update error: %s (after %d ETag retries)", lastErr.Error(), cosmosUpdateEtagRetries)
}

// Delete a document.
func (db *cosmosDB) Delete(ctx context.Context, table string, key string) error {
	container, err := db.getContainer(ctx, table)
	if err != nil {
		return err
	}

	opCtx, cancel := context.WithTimeout(ctx, db.opTimeout)
	defer cancel()
	if _, err := container.DeleteItem(opCtx, docPartitionKey(key), key, db.itemOptions()); err != nil {
		return fmt.Errorf("Delete error: %s", err.Error())
	}
	return nil
}

func (db *cosmosDB) itemOptions() *azcosmos.ItemOptions {
	if db.consistency == nil {
		return &azcosmos.ItemOptions{}
	}
	return &azcosmos.ItemOptions{ConsistencyLevel: db.consistency}
}

type cosmosDBCreator struct{}

func parseConsistencyLevel(p *properties.Properties) (*azcosmos.ConsistencyLevel, error) {
	raw, ok := p.Get(cosmosConsistencyLevel)
	if !ok {
		return nil, nil
	}
	for _, level := range azcosmos.ConsistencyLevelValues() {
		if strings.EqualFold(string(level), raw) {
			return level.ToPtr(), nil
		}
	}
	return nil, fmt.Errorf("unknown %s %q: expected one of %v", cosmosConsistencyLevel, raw, azcosmos.ConsistencyLevelValues())
}

// parseThroughputOptions builds the ThroughputProperties passed to container
// creation when cosmosAutoCreateContainer is set. Both cosmosThroughput and
// cosmosAutoscaleMaxThroughput provision real, billed RU/s, so a malformed
// value here must be a hard config error at startup, not a silent fallback
// to the platform-minimum default - a pure function (rather than inlined
// into Create) so this parsing can be unit-tested without a live connection,
// matching parseConsistencyLevel above.
func parseThroughputOptions(p *properties.Properties) (*azcosmos.ThroughputProperties, error) {
	if autoscaleStr, ok := p.Get(cosmosAutoscaleMaxThroughput); ok {
		autoscaleMax, err := strconv.ParseInt(autoscaleStr, 10, 32)
		if err != nil || autoscaleMax <= 0 {
			return nil, fmt.Errorf("invalid %s %q: must be a positive integer", cosmosAutoscaleMaxThroughput, autoscaleStr)
		}
		t := azcosmos.NewAutoscaleThroughputProperties(int32(autoscaleMax))
		return &t, nil
	}

	manual := int64(cosmosThroughputDefault)
	if throughputStr, ok := p.Get(cosmosThroughput); ok {
		var err error
		manual, err = strconv.ParseInt(throughputStr, 10, 32)
		if err != nil || manual <= 0 {
			return nil, fmt.Errorf("invalid %s %q: must be a positive integer", cosmosThroughput, throughputStr)
		}
	}
	t := azcosmos.NewManualThroughputProperties(int32(manual))
	return &t, nil
}

func (c cosmosDBCreator) Create(p *properties.Properties) (ycsb.DB, error) {
	endpoint := p.GetString(cosmosEndpoint, "")
	key := p.GetString(cosmosKey, "")
	connStr := p.GetString(cosmosConnectionString, "")
	databaseName := p.GetString(cosmosDatabase, cosmosDatabaseDefault)
	partitionPath := p.GetString(cosmosPartitionKeyPath, cosmosPartitionKeyPathDefault)
	if partitionPath != cosmosPartitionKeyPathDefault {
		return nil, fmt.Errorf("%s: only %q is supported by this adapter (docPartitionKey assumes the partition key value is always the record key) - got %q", cosmosPartitionKeyPath, cosmosPartitionKeyPathDefault, partitionPath)
	}
	autoCreate := p.GetBool(cosmosAutoCreateContainer, cosmosAutoCreateContainerDefault)

	consistency, err := parseConsistencyLevel(p)
	if err != nil {
		return nil, err
	}

	opTimeout := cosmosOpTimeoutDefault
	if opTimeoutStr, ok := p.Get(cosmosOpTimeout); ok {
		opTimeout, err = time.ParseDuration(opTimeoutStr)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", cosmosOpTimeout, opTimeoutStr, err)
		}
	}

	scanTimeout := cosmosScanTimeoutDefault
	if scanTimeoutStr, ok := p.Get(cosmosScanTimeout); ok {
		scanTimeout, err = time.ParseDuration(scanTimeoutStr)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", cosmosScanTimeout, scanTimeoutStr, err)
		}
	}

	var throughputOpts *azcosmos.ThroughputProperties
	if autoCreate {
		throughputOpts, err = parseThroughputOptions(p)
		if err != nil {
			return nil, err
		}
	}

	clientOpts := &azcosmos.ClientOptions{}
	if p.GetBool(cosmosInsecureSkipVerify, false) {
		// For local/self-signed testing only (e.g. the Cosmos DB Linux
		// emulator's default self-signed certificate) - mirrors
		// couchbase.tls_skip_verify / mongodb.tls_skip_verify's role in
		// this repo's other adapters.
		clientOpts.ClientOptions.Transport = &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		}
	}

	if connStr != "" && (endpoint != "" || key != "") {
		return nil, fmt.Errorf("cosmosdb: %s is set together with %s/%s - set only one connection method", cosmosConnectionString, cosmosEndpoint, cosmosKey)
	}

	var client *azcosmos.Client
	switch {
	case connStr != "":
		client, err = azcosmos.NewClientFromConnectionString(connStr, clientOpts)
	case endpoint != "" && key != "":
		cred, credErr := azcosmos.NewKeyCredential(key)
		if credErr != nil {
			return nil, fmt.Errorf("cosmosdb: %w", credErr)
		}
		client, err = azcosmos.NewClientWithKey(endpoint, cred, clientOpts)
	default:
		return nil, fmt.Errorf("cosmosdb: either %s, or both %s and %s, must be set", cosmosConnectionString, cosmosEndpoint, cosmosKey)
	}
	if err != nil {
		return nil, fmt.Errorf("cosmosdb: %w", err)
	}

	if autoCreate {
		createCtx, createCancel := context.WithTimeout(context.Background(), opTimeout)
		_, err := client.CreateDatabase(createCtx, azcosmos.DatabaseProperties{ID: databaseName}, nil)
		createCancel()
		if err != nil {
			var respErr *azcore.ResponseError
			if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusConflict {
				return nil, fmt.Errorf("cosmosdb: failed to create database %q: %w", databaseName, err)
			}
		}
	}
	database, err := client.NewDatabase(databaseName)
	if err != nil {
		return nil, fmt.Errorf("cosmosdb: %w", err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), opTimeout)
	_, err = database.Read(readCtx, nil)
	readCancel()
	if err != nil {
		return nil, fmt.Errorf("cosmosdb: database %q not reachable (does it exist? set %s=true to create it): %w", databaseName, cosmosAutoCreateContainer, err)
	}
	fmt.Printf("Connected to Cosmos DB! Using database %q\n", databaseName)

	db := &cosmosDB{
		client:         client,
		database:       database,
		partitionPath:  partitionPath,
		autoCreate:     autoCreate,
		throughputOpts: throughputOpts,
		opTimeout:      opTimeout,
		scanTimeout:    scanTimeout,
		consistency:    consistency,
	}
	return db, nil
}

func init() {
	ycsb.RegisterDBCreator("cosmosdb", cosmosDBCreator{})
}

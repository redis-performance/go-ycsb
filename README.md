# go-ycsb

[![Docker Pulls](https://img.shields.io/docker/pulls/redis/go-ycsb)](https://hub.docker.com/r/redis/go-ycsb)
[![CI](https://github.com/redis-performance/go-ycsb/actions/workflows/go.yml/badge.svg)](https://github.com/redis-performance/go-ycsb/actions/workflows/go.yml)
[![Integration](https://github.com/redis-performance/go-ycsb/actions/workflows/integration.yml/badge.svg)](https://github.com/redis-performance/go-ycsb/actions/workflows/integration.yml)

go-ycsb is a Go port of [YCSB](https://github.com/brianfrankcooper/YCSB). It fully supports all YCSB generators and the Core workload so we can do the basic CRUD benchmarks with Go.

## Why another Go YCSB?

+ We want to build a standard benchmark tool in Go.
+ We are not familiar with Java.

## Getting Started

### Download

https://github.com/pingcap/go-ycsb/releases/latest

**Linux**
```
wget -c https://github.com/pingcap/go-ycsb/releases/latest/download/go-ycsb-linux-amd64.tar.gz -O - | tar -xz

# give it a try
./go-ycsb --help
```

**OSX**
```
wget -c https://github.com/pingcap/go-ycsb/releases/latest/download/go-ycsb-darwin-amd64.tar.gz -O - | tar -xz

# give it a try
./go-ycsb --help
```

### Building from source

```bash
git clone https://github.com/pingcap/go-ycsb.git
cd go-ycsb
make

# give it a try
./bin/go-ycsb  --help
```

Notice:

+ Minimum supported go version is 1.16.
+ To use FoundationDB, you must install [client](https://www.foundationdb.org/download/) library at first, now the supported version is 6.2.11.
+ To use RocksDB, you must follow [INSTALL](https://github.com/facebook/rocksdb/blob/master/INSTALL.md) to install RocksDB at first.

## Docker

Pre-built Docker images are available on Docker Hub:

```bash
docker pull redis/go-ycsb:latest
docker run --rm redis/go-ycsb --help
```

Images are built for `linux/amd64` and `linux/arm64`.

## Usage

Mostly, we can start from the official document [Running-a-Workload](https://github.com/brianfrankcooper/YCSB/wiki/Running-a-Workload).

### Shell

```basic
./bin/go-ycsb shell basic
» help
YCSB shell command

Usage:
  shell [command]

Available Commands:
  delete      Delete a record
  help        Help about any command
  insert      Insert a record
  read        Read a record
  scan        Scan starting at key
  table       Get or [set] the name of the table
  update      Update a record
```

### Load

```bash
./bin/go-ycsb load basic -P workloads/workloada
```

### Run

```bash
./bin/go-ycsb run basic -P workloads/workloada
```

### Feature-store workload

`workloads/workload_feature_store` models the entity-major serving layout from the [redis-benchmarks-specification feature-store playbook](https://github.com/redis/redis-benchmarks-specification/blob/main/redis_benchmarks_specification/test-suites/memtier_benchmark-playbook-feature-store-hash-10M-entities-50-features-template.yml): one hash/document per entity (`feature_1`..`feature_50` + a trailing `event_ts` field), whole-entity reads (Redis `HGETALL`, MongoDB `findOne`), whole-row writes (`HSET`/`$set` of every field), a 95:5 read:write mix, and Zipfian-skewed serving traffic. Field values default to realistic typed scalars (numeric mix + a real timestamp) shaped like what [Feast](https://docs.feast.dev/reference/type-system) and [Featureform](https://docs.featureform.com/abstractions/feature) actually store, rather than opaque bytes. It works against `redis` and `mongodb` out of the box:

```bash
./bin/go-ycsb load redis   -P workloads/workload_feature_store -p redis.addr=127.0.0.1:6379
./bin/go-ycsb run  redis   -P workloads/workload_feature_store -p redis.addr=127.0.0.1:6379

./bin/go-ycsb load mongodb -P workloads/workload_feature_store -p mongodb.url="mongodb://127.0.0.1:27017/ycsb?w=1"
./bin/go-ycsb run  mongodb -P workloads/workload_feature_store -p mongodb.url="mongodb://127.0.0.1:27017/ycsb?w=1"
```

See the comments at the top of `workloads/workload_feature_store` for the full data-shape/command-mix rationale, and the "Field generation" table below for the properties it's built from (`fieldnameprefix`, `fieldvaluetype`, etc.) - those are generic core-workload properties, so they work in any workload file, not just this one.

## Supported Database

- MySQL / TiDB
- TiKV
- FoundationDB
- Aerospike
- Badger
- Cassandra / ScyllaDB
- Couchbase / Couchbase Capella
- Azure Cosmos DB (Core/SQL API)
- Pegasus
- PostgreSQL / CockroachDB / AlloyDB / Yugabyte
- RocksDB
- Spanner
- Sqlite
- MongoDB
- Redis and Redis Cluster
- BoltDB
- etcd
- DynamoDB

## Field generation

These are core-workload properties (see [Running-a-Workload](https://github.com/brianfrankcooper/YCSB/wiki/Running-a-Workload) for the base set like `fieldcount`/`fieldlength`/`readallfields`); the ones below extend field naming and value content and work in any workload file.

|field|default value|description|
|-|-|-|
|fieldlengthminimum|1|Lower bound for `uniform`/`zipfian` fieldlengthdistribution, e.g. to model 8-24 byte values instead of always starting at 1 byte|
|fieldnameprefix|"field"|Prefix for generated field names (`field0`, `field1`, ...), e.g. `feature_` for `feature_0`, `feature_1`, ...|
|fieldnamestartindex|0|Starting index for numbered field names, e.g. `1` for `feature_1`..`feature_N` instead of `feature_0`..`feature_(N-1)`|
|lastfieldname|""|If set, overrides the name of the final generated field - e.g. a trailing `event_ts` metadata column alongside numbered feature fields|
|fieldvaluetype|"random"|Content of generated field values: `random` (opaque bytes, only size matters), `integer`, `float`, `boolean`, `timestamp` (RFC3339), or `numeric` (a realistic int/float/boolean mix) - see [Feast](https://docs.feast.dev/reference/type-system)/[Featureform](https://docs.featureform.com/abstractions/feature)'s typed scalar columns for the shape this models|
|lastfieldvaluetype|""|If set, overrides the value type of the final field (see `lastfieldname`), e.g. `timestamp` for a trailing `event_ts` field while numbered feature fields stay `numeric`. Must be set together with `lastfieldname`|
|fieldvalueintegermin|0|Lower bound for `integer`/`numeric` fieldvaluetype content|
|fieldvalueintegermax|100000|Upper bound for `integer`/`numeric` fieldvaluetype content, e.g. a bounded count column|
|fieldvaluefloatmin|0.0|Lower bound for `float`/`numeric` fieldvaluetype content|
|fieldvaluefloatmax|1.0|Upper bound for `float`/`numeric` fieldvaluetype content, e.g. a rate/score/probability column|
|fieldvaluefloatprecision|4|Decimal places for `float`/`numeric` fieldvaluetype content, e.g. `0.8472`|

## Output configuration

|field|default value|description|
|-|-|-|
|measurementtype|"histogram"|The mechanism for recording measurements, one of `histogram`, `raw` or `csv`|
|measurement.output_file|""|File to write output to, default writes to stdout|

## Database Configuration

You can pass the database configurations through `-p field=value` in the command line directly.

Common configurations:

|field|default value|description|
|-|-|-|
|dropdata|false|Whether to remove all data before test|
|verbose|false|Output the execution query|
|debug.pprof|":6060"|Go debug profile address|

### MySQL & TiDB

|field|default value|description|
|-|-|-|
|mysql.host|"127.0.0.1"|MySQL Host|
|mysql.port|3306|MySQL Port|
|mysql.user|"root"|MySQL User|
|mysql.password||MySQL Password|
|mysql.db|"test"|MySQL Database|
|tidb.cluster_index|true|Whether to use cluster index, for TiDB only|
|tidb.instances|""|Comma-seperated address list of tidb instances (eg: `tidb-0:4000,tidb-1:4000`)|


### TiKV

|field|default value|description|
|-|-|-|
|tikv.pd|"127.0.0.1:2379"|PD endpoints, seperated by comma|
|tikv.type|"raw"|TiKV mode, "raw", "txn", or "coprocessor"|
|tikv.conncount|128|gRPC connection count|
|tikv.batchsize|128|Request batch size|
|tikv.async_commit|true|Enalbe async commit or not|
|tikv.one_pc|true|Enable one phase or not|
|tikv.apiversion|"V1"|[api-version](https://docs.pingcap.com/tidb/stable/tikv-configuration-file#api-version-new-in-v610) of tikv server, "V1" or "V2"|

### FoundationDB

|field|default value|description|
|-|-|-|
|fdb.cluster|""|The cluster file used for FoundationDB, if not set, will use the [default](https://apple.github.io/foundationdb/administration.html#default-cluster-file)|
|fdb.dbname|"DB"|The cluster database name|
|fdb.apiversion|510|API version, now only 5.1 is supported|

### PostgreSQL & CockroachDB & AlloyDB & Yugabyte

|field|default value|description|
|-|-|-|
|pg.host|"127.0.0.1"|PostgreSQL Host|
|pg.port|5432|PostgreSQL Port|
|pg.user|"root"|PostgreSQL User|
|pg.password||PostgreSQL Password|
|pg.db|"test"|PostgreSQL Database|
|pg.sslmode|"disable|PostgreSQL ssl mode|

### Aerospike

|field|default value|description|
|-|-|-|
|aerospike.host|"localhost"|The port of the Aerospike service|
|aerospike.port|3000|The port of the Aerospike service|
|aerospike.ns|"test"|The namespace to use|
|aerospike.tls|false|Enable a TLS connection to the cluster. Required for Aerospike Cloud/Enterprise deployments that enforce TLS|
|aerospike.tls.ca|""|Path to a PEM-encoded CA certificate to verify the server's certificate against. If unset, falls back to the system trust store|
|aerospike.tls.skip.verify|false|Skip TLS certificate verification entirely (insecure; for local/self-signed testing only)|
|aerospike.tls.name|""|The TLS certificate name (Aerospike's "tls-name") the server's certificate is registered under - distinct from `aerospike.host`, since a managed/cloud deployment's connect address doesn't necessarily match what the certificate was issued for|

Note: `aerospike-client-go`'s TLS handling is more predictable than gocql's (see the Cassandra section above) - it uses the configured `*tls.Config` as-is, so `aerospike.tls.ca`/`aerospike.tls.skip.verify` behave exactly as they read, no extra workaround needed.

### Badger

|field|default value|description|
|-|-|-|
|badger.dir|"/tmp/badger"|The directory to save data|
|badger.valuedir|"/tmp/badger"|The directory to save value, if not set, use badger.dir|
|badger.sync_writes|false|Sync all writes to disk|
|badger.num_versions_to_keep|1|How many versions to keep per key|
|badger.max_table_size|64MB|Each table (or file) is at most this size|
|badger.level_size_multiplier|10|Equals SizeOf(Li+1)/SizeOf(Li)|
|badger.max_levels|7|Maximum number of levels of compaction|
|badger.value_threshold|32|If value size >= this threshold, only store value offsets in tree|
|badger.num_memtables|5|Maximum number of tables to keep in memory, before stalling|
|badger.num_level0_tables|5|Maximum number of Level 0 tables before we start compacting|
|badger.num_level0_tables_stall|10|If we hit this number of Level 0 tables, we will stall until L0 is compacted away|
|badger.level_one_size|256MB|Maximum total size for L1|
|badger.value_log_file_size|1GB|Size of single value log file|
|badger.value_log_max_entries|1000000|Max number of entries a value log file can hold (approximately). A value log file would be determined by the smaller of its file size and max entries|
|badger.num_compactors|3|Number of compaction workers to run concurrently|
|badger.do_not_compact|false|Stops LSM tree from compactions|
|badger.table_loading_mode|options.LoadToRAM|How should LSM tree be accessed|
|badger.value_log_loading_mode|options.MemoryMap|How should value log be accessed|

### RocksDB

|field|default value|description|
|-|-|-|
|rocksdb.dir|"/tmp/rocksdb"|The directory to save data|
|rocksdb.allow_concurrent_memtable_writes|true|Sets whether to allow concurrent memtable writes|
|rocksdb.allow_mmap_reads|false|Enable/Disable mmap reads for reading sst tables|
|rocksdb.allow_mmap_writes|false|Enable/Disable mmap writes for writing sst tables|
|rocksdb.arena_block_size|0(write_buffer_size / 8)|Sets the size of one block in arena memory allocation|
|rocksdb.db_write_buffer_size|0(disable)|Sets the amount of data to build up in memtables across all column families before writing to disk|
|rocksdb.hard_pending_compaction_bytes_limit|256GB|Sets the bytes threshold at which all writes are stopped if estimated bytes needed to be compaction exceed this threshold|
|rocksdb.level0_file_num_compaction_trigger|4|Sets the number of files to trigger level-0 compaction|
|rocksdb.level0_slowdown_writes_trigger|20|Sets the soft limit on number of level-0 files|
|rocksdb.level0_stop_writes_trigger|36|Sets the maximum number of level-0 files. We stop writes at this point|
|rocksdb.max_bytes_for_level_base|256MB|Sets the maximum total data size for base level|
|rocksdb.max_bytes_for_level_multiplier|10|Sets the max Bytes for level multiplier|
|rocksdb.max_total_wal_size|0(\[sum of all write_buffer_size * max_write_buffer_number\] * 4)|Sets the maximum total wal size in bytes. Once write-ahead logs exceed this size, we will start forcing the flush of column families whose memtables are backed by the oldest live WAL file (i.e. the ones that are causing all the space amplification)|
|rocksdb.memtable_huge_page_size|0|Sets the page size for huge page for arena used by the memtable|
|rocksdb.num_levels|7|Sets the number of levels for this database|
|rocksdb.use_direct_reads|false|Enable/Disable direct I/O mode (O_DIRECT) for reads|
|rocksdb.use_fsync|false|Enable/Disable fsync|
|rocksdb.write_buffer_size|64MB|Sets the amount of data to build up in memory (backed by an unsorted log on disk) before converting to a sorted on-disk file|
|rocksdb.max_write_buffer_number|2|Sets the maximum number of write buffers that are built up in memory|
|rocksdb.max_background_jobs|2|Sets maximum number of concurrent background jobs (compactions and flushes)|
|rocksdb.block_size|4KB|Sets the approximate size of user data packed per block. Note that the block size specified here corresponds opts uncompressed data. The actual size of the unit read from disk may be smaller if compression is enabled|
|rocksdb.block_size_deviation|10|Sets the block size deviation. This is used opts close a block before it reaches the configured 'block_size'. If the percentage of free space in the current block is less than this specified number and adding a new record opts the block will exceed the configured block size, then this block will be closed and the new record will be written opts the next block|
|rocksdb.cache_index_and_filter_blocks|false|Indicating if we'd put index/filter blocks to the block cache. If not specified, each "table reader" object will pre-load index/filter block during table initialization|
|rocksdb.no_block_cache|false|Specify whether block cache should be used or not|
|rocksdb.pin_l0_filter_and_index_blocks_in_cache|false|Sets cache_index_and_filter_blocks. If is true and the below is true (hash_index_allow_collision), then filter and index blocks are stored in the cache, but a reference is held in the "table reader" object so the blocks are pinned and only evicted from cache when the table reader is freed|
|rocksdb.whole_key_filtering|true|Specify if whole keys in the filter (not just prefixes) should be placed. This must generally be true for gets opts be efficient|
|rocksdb.block_restart_interval|16|Sets the number of keys between restart points for delta encoding of keys. This parameter can be changed dynamically|
|rocksdb.filter_policy|nil|Sets the filter policy opts reduce disk reads. Many applications will benefit from passing the result of NewBloomFilterPolicy() here|
|rocksdb.index_type|kBinarySearch|Sets the index type used for this table. __kBinarySearch__: A space efficient index block that is optimized for binary-search-based index. __kHashSearch__: The hash index, if enabled, will do the hash lookup when `Options.prefix_extractor` is provided. __kTwoLevelIndexSearch__: A two-level index implementation. Both levels are binary search indexes|
|rocksdb.block_align|false|Enable/Disable align data blocks on lesser of page size and block size|

### Spanner

|field|default value|description|
|-|-|-|
|spanner.db|""|Spanner Database|
|spanner.credentials|"~/.spanner/credentials.json"|Google application credentials for Spanner|

### Sqlite

|field|default value|description|
|-|-|-|
|sqlite.db|"/tmp/sqlite.db"|Database path|
|sqlite.mode|"rwc"|Open Mode: ro, rc, rwc, memory|
|sqlite.journalmode|"DELETE"|Journal mode: DELETE, TRUNCSTE, PERSIST, MEMORY, WAL, OFF|
|sqlite.cache|"Shared"|Cache: shared, private|

### Cassandra

|field|default value|description|
|-|-|-|
|cassandra.cluster|"127.0.0.1:9042"|Cassandra cluster|
|cassandra.keyspace|"test"|Keyspace|
|cassandra.connections|2|Number of connections per host|
|cassandra.username|cassandra|Username|
|cassandra.password|cassandra|Password|
|cassandra.tls|false|Enable a TLS connection to the cluster. Required for ScyllaDB Cloud and most managed Cassandra-protocol services, which enforce TLS with no plaintext option|
|cassandra.tls.ca|""|Path to a PEM-encoded CA certificate to verify the server's certificate CHAIN against (not its hostname - see below). If unset, falls back to the system trust store|
|cassandra.tls.skip.verify|false|Skip TLS certificate verification entirely (insecure; for local/self-signed testing only)|
|cassandra.tls.disable_host_lookup|true|Disable gocql's automatic ring discovery when TLS is enabled (see below). Set to false for a self-managed cluster that serves TLS on the same port throughout and doesn't need this|

Notes:
- `cassandra.tls.ca` verifies the certificate **chain** but deliberately not the **hostname**: managed/SNI-proxied clusters (ScyllaDB Cloud confirmed) are dialed via explicit `host:port` pairs whose address doesn't necessarily match the certificate's SAN, so standard hostname verification would reject a perfectly valid connection. This can't be expressed as a simple boolean in the underlying gocql library, so this adapter supplies its own certificate-chain verification instead of relying on gocql's all-or-nothing verify/don't-verify toggle - see the comment above `newCassandraTLSConfig` in `db/cassandra/db.go` for the full mechanism.
- When `cassandra.tls=true`, `cassandra.tls.disable_host_lookup` defaults to `true`. Managed/SNI-proxied clusters (confirmed on ScyllaDB Cloud) expose CQL-over-TLS on a distinct port from the plaintext native port reported back by `system.peers`/`system.local` during gocql's automatic ring discovery — without disabling that discovery, the driver connects fine to the first seed host on the TLS port, then tries every *other* discovered peer on the plaintext port and fails. Pass every node as an explicit `host:port` pair in `cassandra.cluster` when using TLS with the default. If you're instead connecting to a self-managed cluster that serves TLS uniformly, set `cassandra.tls.disable_host_lookup=false` to keep automatic node discovery.

### Couchbase

Works against both a self-managed Couchbase cluster and [Couchbase Capella](https://www.couchbase.com/products/capella/) - for Capella, set `couchbase.connection_string` to the `couchbases://cb.<cluster>.cloud.couchbase.com` connection string shown in its UI and a database credential's username/password; Capella enforces TLS (the `couchbases://` scheme) and ships a publicly-trusted certificate, so no CA configuration is needed for the common case.

|field|default value|description|
|-|-|-|
|couchbase.connection_string|"couchbase://127.0.0.1"|Cluster connection string. Use `couchbases://...` for a TLS connection (required by Capella)|
|couchbase.username|"Administrator"|Username|
|couchbase.password|"password"|Password|
|couchbase.bucket|"ycsb"|Bucket name. Must already exist - unlike scopes/collections (see `couchbase.auto_create_collection` below), this adapter does not create buckets|
|couchbase.scope|"_default"|Scope name. Every bucket always has a ready-to-use "_default" scope, so this only needs setting for a non-default scope|
|couchbase.auto_create_collection|true|Create the scope/collection for a workload's `table` on first use if it doesn't already exist. Every bucket always has a ready-to-use "_default" collection, so this only matters for a non-default `couchbase.scope` or a `table` other than "_default". Best-effort: a least-privilege credential (e.g. a Capella database credential without Manage Collections) will fail to create it, at which point the collection must be created out of band|
|couchbase.durability|"none"|Synchronous replication level required before a write is acknowledged: "none", "majority", "majorityAndPersistActive", or "persistToMajority" - Couchbase's equivalent of a MongoDB write concern. Requires a multi-node cluster; there's no "read from majority" equivalent to pair with it, since a Couchbase KV read always goes to the single active node that owns the key, unlike a MongoDB replica set|
|couchbase.tls_skip_verify|false|Skip TLS certificate verification entirely (insecure; for local/self-signed testing only). Requires `couchbase.connection_string` to use `couchbases://` - the adapter fails fast at startup rather than silently ignoring this if it doesn't|
|couchbase.tls_ca_file|""|Path to a PEM-encoded CA certificate, for a self-managed cluster's private CA. Not needed for Capella. Requires `couchbase.connection_string` to use `couchbases://` - the adapter fails fast at startup rather than silently ignoring this if it doesn't|
|couchbase.kv_timeout|N/A|Timeout for a point KV op (Read/Insert/Update/Delete), e.g. "5s". Defaults to gocb's own default (2.5s)|
|couchbase.scan_timeout|"30s"|Timeout for a Scan op. Kept separate from `couchbase.kv_timeout` and well above gocb's own internal 10s default: see the note on Scan concurrency below|

Notes:
- **TLS is entirely controlled by the connection string's scheme.** gocb only ever consults `couchbase.tls_skip_verify`/`couchbase.tls_ca_file` when `couchbase.connection_string` uses `couchbases://` - there is no way to force TLS on independently. Setting either property while leaving the connection string at the plain `couchbase://` default (or a typo'd one) is therefore rejected at startup with a clear error, rather than silently connecting in plaintext with the CA/skip-verify setting parsed and then discarded.
- **Scan concurrency.** Scan is implemented via a KV range scan, which gocb's own docs describe as meant "for low concurrency batch queries where latency is not critical." Taken literally: running a high-`scanproportion` workload with many threads against Couchbase is outside that intended use, and this adapter has observed exactly that - concurrent range scans against the same collection - cause gocb's result stream to stall well past its own configured timeout. This adapter bounds every Scan call itself (`couchbase.scan_timeout`) so a stuck call fails cleanly instead of hanging a worker goroutine forever, but the underlying slowness/contention isn't something a client-side timeout can fully fix: gocb stops honoring the caller's context/deadline partway through a scan's own internal request sequence, so in the specific case where `col.Scan()` itself is what's wedged (not just the result draining afterward), the calling worker is still freed at `couchbase.scan_timeout`, but the goroutine attempting the scan leaks permanently in the background rather than the process eventually recovering it. Keep `threadcount` low for a scan-heavy workload, same guidance gocb gives - this is a real gap in gocb's own cancellation model, not something fixable purely from the client side.

### Azure Cosmos DB

Uses the Core (SQL) API natively - not Cosmos DB's MongoDB- or Cassandra-API compatibility layers, so this reflects Cosmos DB's actual RU-based, partition-aware behavior rather than a wire-protocol shim. The partition key is always the record key itself (`cosmosdb.partition_key_path` only accepts its default, `"/id"`): one logical partition per record, the natural mapping for a point-read/point-write workload and the only way to avoid adding cross-partition query overhead this adapter doesn't otherwise need. One consequence: Scan is unavoidably a cross-partition SQL query (no single-partition query could span more than one record under this design), and a skewed (zipfian/hotspot) access pattern can genuinely land many "hot" keys on the same underlying physical partition and get RU-throttled - that's real Cosmos DB behavior under skew, not a benchmark artifact, as long as the partition key stays the record key.

|field|default value|description|
|-|-|-|
|cosmosdb.endpoint|""|Account endpoint URL, e.g. `https://myaccount.documents.azure.com:443/`. Required unless `cosmosdb.connection_string` is set|
|cosmosdb.key|""|Account key. Required unless `cosmosdb.connection_string` is set|
|cosmosdb.connection_string|""|Alternative to `cosmosdb.endpoint`/`cosmosdb.key`|
|cosmosdb.database|"ycsb"|Database name|
|cosmosdb.auto_create_container|false|Create the database/container for a workload's `table` on first use if it doesn't already exist. Defaults to **false**, unlike Couchbase's equivalent (`couchbase.auto_create_collection`, true by default): creating a Cosmos DB container provisions real, billed throughput, so auto-creating one as a side effect of a typo'd `cosmosdb.database`/table name is a real-money footgun a local/free database doesn't have|
|cosmosdb.throughput|400|Manual RU/s for an auto-created database/container (400 is Cosmos DB's own platform minimum). Ignored if `cosmosdb.autoscale_max_throughput` is set|
|cosmosdb.autoscale_max_throughput|N/A|Autoscale max RU/s for an auto-created database/container, instead of manual `cosmosdb.throughput`|
|cosmosdb.consistency_level|N/A|Per-operation consistency override, e.g. "Strong", "Session", "Eventual". The Cosmos DB SDK only allows *relaxing* consistency below the account's own configured default - there is no way for this property to request stronger consistency than the account was provisioned with. If you want Strong consistency end to end, the Cosmos DB **account** itself must be configured with Strong as its default; leave this unset to just inherit that|
|cosmosdb.op_timeout|"10s"|Timeout for every individual point operation (Read/Insert/Update/Delete, and each Scan page request), via context cancellation|
|cosmosdb.scan_timeout|"60s"|Timeout for a whole Scan call, separate from `cosmosdb.op_timeout`: Scan pages through a cross-partition query via multiple round trips until `count` items are collected, so its total duration is a multiple of a single operation's, not comparable to one|
|cosmosdb.insecure_skip_verify|false|Skip TLS certificate verification entirely (insecure; for local/self-signed testing only, e.g. against the Cosmos DB Linux emulator's self-signed certificate)|

Notes:
- **Update merges, and mostly does so atomically.** For a values map within Cosmos DB's 10-operation-per-request `PatchItem` limit (the common case - go-ycsb's core workload defaults to `writeallfields=false`, a single field per Update), Update uses `PatchItem`/`AppendSet`, one operation per field: this sets exactly the fields being updated and leaves every other field on the document untouched, in one round trip, with no read-modify-write race window. A wider values map (reachable with `writeallfields=true` against a table with more than 10 fields - `workloads/workload_feature_store`'s actual default) falls back to Read+merge+Replace, using the Read's ETag for optimistic concurrency (Cosmos DB's equivalent of a CAS token) so a concurrent write landing in between is detected as a conflict and retried (up to 5 times) rather than silently lost.
- **Scan performance.** Since the partition key is always the record key, Scan has to run as a cross-partition `SELECT * FROM c WHERE c.id >= @start ORDER BY c.id` query - correct, but meaningfully slower per-op (and costlier in RU) than a point read. Keep this in mind for a scan-heavy workload.

### MongoDB

|field|default value|description|
|-|-|-|
|mongodb.url|"mongodb://127.0.0.1:27017/ycsb?w=1"|MongoDB URI. The database go-ycsb uses is taken from the URI's path segment (e.g. "ycsb" above); it falls back to "ycsb" if the URI has none|
|mongodb.tls_skip_verify|false|Enable/disable server ca certificate verification|
|mongodb.tls_ca_file|""|Path to mongodb server ca certificate file|
|mongodb.authdb|"admin"|Authentication database|
|mongodb.username|N/A|Username for authentication|
|mongodb.password|N/A|Password for authentication|
|mongodb.write_concern|N/A|Write concern: "majority" or a numeric ack count (e.g. "1", "2"). "0" (unacknowledged) is supported: Insert/Update/Delete treat the driver's expected ErrUnacknowledgedWrite as success rather than a failure|
|mongodb.write_concern_journal|false|Also require the write to hit the on-disk journal; combine with mongodb.write_concern=majority for durable/synchronous writes. Incompatible with mongodb.write_concern=0|
|mongodb.write_concern_timeout|N/A|How long the server waits for the configured write concern (e.g. majority) to be satisfied before giving up, e.g. "5s". Without it, majority against a replica set that can't currently reach a majority blocks indefinitely|
|mongodb.read_concern|N/A|Read concern: "local", "available", "majority", or "linearizable" ("snapshot" is not offered - it requires a transaction this adapter never opens)|
|mongodb.read_preference|N/A|Read preference: "primary", "primaryPreferred", "secondary", "secondaryPreferred", or "nearest"|
|mongodb.socket_timeout|N/A|Timeout for socket reads/writes, e.g. "10s". Without it, a stalled connection (e.g. a silently dropped network path) can hang an operation indefinitely|

### Redis
|field|default value|description|
|-|-|-|
|redis.datatype|hash|"hash", "string" or "json" ("json" requires [RedisJSON](https://redis.io/docs/stack/json/) available)|
|redis.mode|single|"single" or "cluster"|
|redis.network|tcp|"tcp" or "unix"|
|redis.addr||Redis server address(es) in "host:port" form, can be semi-colon `;` separated in cluster mode|
|redis.username||Redis server username|
|redis.password||Redis server password|
|redis.db|0|Redis server target db|
|redis.max_redirects|0|The maximum number of retries before giving up (only for cluster mode)|
|redis.read_only|false|Enables read-only commands on slave nodes (only for cluster mode)|
|redis.route_by_latency|false|Allows routing read-only commands to the closest master or slave node (only for cluster mode)|
|redis.route_randomly|false|Allows routing read-only commands to the random master or slave node (only for cluster mode)|
|redis.max_retries||Max retries before giving up connection|
|redis.min_retry_backoff|8ms|Minimum backoff between each retry|
|redis.max_retry_backoff|512ms|Maximum backoff between each retry|
|redis.dial_timeout|5s|Dial timeout for establishing new connection|
|redis.read_timeout|3s|Timeout for socket reads|
|redis.write_timeout|3s|Timeout for socket writes|
|redis.pool_size|10|Maximum number of socket connections|
|redis.min_idle_conns|0|Minimum number of idle connections|
|redis.max_idle_conns|0|Maximum number of idle connections. If <= 0, connections are not closed due to a connection's idle time.|
|redis.max_conn_age|0|Connection age at which client closes the connection|
|redis.pool_timeout|4s|Amount of time client waits for connections are busy before returning an error|
|redis.idle_timeout|5m|Amount of time after which client closes idle connections. Should be less than server timeout|
|redis.idle_check_frequency|1m|Frequency of idle checks made by idle connections reaper. Deprecated in favour of redis.max_idle_conns|
|redis.tls_ca||Path to CA file|
|redis.tls_cert||Path to cert file|
|redis.tls_key||Path to key file|
|redis.tls_insecure_skip_verify|false|Controls whether a client verifies the server's certificate chain and host name|

### BoltDB

|field|default value|description|
|-|-|-|
|bolt.path|"/tmp/boltdb"|The database file path. If the file does not exists then it will be created automatically|
|bolt.timeout|0|The amount of time to wait to obtain a file lock. When set to zero it will wait indefinitely. This option is only available on Darwin and Linux|
|bolt.no_grow_sync|false|Sets DB.NoGrowSync flag before memory mapping the file|
|bolt.read_only|false|Open the database in read-only mode|
|bolt.mmap_flags|0|Set the DB.MmapFlags flag before memory mapping the file|
|bolt.initial_mmap_size|0|The initial mmap size of the database in bytes. If <= 0, the initial map size is 0. If the size is smaller than the previous database, it takes no effect|

### etcd

|field|default value|description|
|-|-|-|
|etcd.endpoints|"localhost:2379"|The etcd endpoint(s), multiple endpoints can be passed separated by comma.|
|etcd.dial_timeout|"2s"|The dial timeout duration passed into the client config.|
|etcd.cert_file|""|When using secure etcd, this should point to the crt file.|
|etcd.key_file|""|When using secure etcd, this should point to the pem file.|
|etcd.cacert_file|""|When using secure etcd, this should point to the ca file.|
|etcd.serializable_reads|false|Whether to use serializable reads.|

### DynamoDB

|field|default value|description|
|-|-|-|
|dynamodb.tablename|"ycsb"|The database tablename|
|dynamodb.primarykey|"_key"|The table primary key fieldname|
|dynamodb.rc.units|10|Read request units throughput|
|dynamodb.wc.units|10|Write request units throughput|
|dynamodb.ensure.clean.table|true|On load mode ensure that the table is clean at the begining. In case of true and if the table previously exists it will be deleted and recreated|
|dynamodb.endpoint|""|Used endpoint for connection. If empty will use the default loaded configs|
|dynamodb.region|""|Used region for connection ( should match endpoint ). If empty will use the default loaded configs|
|dynamodb.consistent.reads|false|Reads on DynamoDB provide an eventually consistent read by default. If your benchmark/use-case requires a strongly consistent read, set this option to true|
|dynamodb.delete.after.run.stage|false|Detele the database table after the run stage|

## Testing

```bash
go test ./...

# Feature-store workload load+run against dockerized Redis + MongoDB
# (starts and tears down its own disposable containers)
make test-integration-feature-store

# Cassandra TLS support against a dockerized, TLS-enabled ScyllaDB node -
# asserts a connection with the correct CA succeeds AND one with an
# unrelated CA is rejected, since the latter is what catches a TLS setup
# that encrypts but never actually verifies the server
make test-integration-cassandra-tls

# Aerospike TLS support against dockerized Aerospike (Community Edition has
# no native TLS of its own, so this goes through a TLS-terminating proxy)
# - same correct-CA/wrong-CA assertions as the cassandra test above
make test-integration-aerospike-tls

# Couchbase adapter (core + feature-store workloads, Scan, and an
# auto_create_collection=false negative check) against dockerized Couchbase
# Community Edition. The adapter's TLS path (couchbases://, for Capella) has
# no CE equivalent to test in CI and was instead verified by hand against a
# live Capella cluster - see db/couchbase/db.go and the README's Couchbase
# section
make test-integration-couchbase

# Cosmos DB adapter (core + feature-store workloads, a cross-partition Scan,
# a field-preservation regression check, and an
# auto_create_container=false negative check) against a dockerized Azure
# Cosmos DB (vNext) Linux emulator
make test-integration-cosmosdb
```

All five integration tests run in CI on every push/PR to `master` (see `.github/workflows/integration.yml`); see [CONTRIBUTING.md](CONTRIBUTING.md) for the full testing/review bar for PRs.

## TODO

- [ ] Support more measurement, like HdrHistogram
- [ ] Add tests for generators
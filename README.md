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

## Batched loads

|field|default value|description|
|-|-|-|
|batch.size|1|Records per batch: above 1, each thread inserts (on `load`) its records `batch.size` at a time, through the DB's `BatchInsert` (`redis`, `mysql`, `sqlite`, `tikv`, `basic`), or one `Insert` per record for a DB without it. `run` batches its reads, updates and inserts the same way|

- **Key range.** A batched load inserts exactly the records of a plain one: each thread's last batch is cut to what is left of its share, so `insertstart`/`insertcount` hold whatever the batch size (as without batches, `insertstart` without `insertcount` loads `recordcount` records from `insertstart` on). `insertorder=hashed` folds each key number's 64-bit hash into 63 bits, so two key numbers can in principle get the same key: for an exact key count use `insertorder=ordered`.
- **Counting.** For a DB that implements the batch operation itself (`BatchInsert`, and for `run` `BatchRead`/`BatchUpdate`), every record of a batch counts as one operation: `INSERT` (and `TOTAL`) for each record that succeeded, `INSERT_ERROR` for each that failed, all with the batch's latency, since a record is done when its batch is. So `Count` and `OPS` are records, as without batches, and the latencies are per batch. Each batch also counts once as `BATCH_INSERT` (`BATCH_READ`, ...), failed records or not (but not a batch the run's stop kept from being sent), which `TOTAL` leaves out: the `*_ERROR` counts are failed records only. With insertion retries `BATCH_INSERT` counts attempts: a retry of a batch's failed records is one more. A DB without the batch operation gets one call per record, each measured on its own with its own latency, and no `BATCH_` sample: `redis` implements `BatchInsert` alone, so its batched `run` reads and updates are measured per record.
- **Batched runs.** A batch is one draw of the operation mix: all reads, all updates or all inserts. A read-modify-write draw does nothing but counts as `batch.size` operations done (they are missing from the measurements, so `TOTAL` falls short of `operationcount`), a scan draw is not supported (it panics), and `run` never batches deletes. With `warmuptime`, a batch the warm-up was still on for when it started is neither measured nor counted, also if the warm-up ends while it runs.
- **Errors.**
  - A DB that reports which records of a batch failed (a `*ycsb.BatchError`; `redis` does) gets just those counted as failed and retried (`core_workload_insertion_retry_limit`, every `core_workload_insertion_retry_interval` seconds). Any other error from a batch fails, and retries, the whole batch.
  - `INSERT_ERROR` counts failed attempts: a record that failed and then went in on a retry is in both `INSERT_ERROR` and `INSERT`, so `INSERT_ERROR` overstates what was lost.
  - `insertcount` minus `INSERT` is an upper bound on the records not loaded: a record that failed may have been written all the same (a timeout, say).
  - Stopping a run (SIGINT, `timeout -s INT`), for every DB: an operation (or batch) the stop came before isn't handed to the DB and counts as nothing. One the DB was given counts as it ends; a `context.Canceled` from the DB (a driver giving up in flight) counts as failed.
  - So after a stop `INSERT` is a lower bound of the records written, and `INSERT` + `INSERT_ERROR` an upper one.
  - With `redis`, an operation already sent at the stop still runs to its outcome for up to 5 s, and is then ended: see "Stopping a run" in the [Redis](#redis) section.
- **Throttling.** `target` holds with batches too, on average: a thread sends a batch, then waits as long as that many operations take at its share of the target. Each thread starts at a random point of its first batch period (`batch.size` operations at its share of the target), so the threads' batches spread over it instead of all being sent at once; each batch is still a burst of `batch.size` records. The spread adds up to one batch period to a thread's run, so a short throttled run falls a little short of `target` (a few percent). With `warmuptime` (batches or not) a thread's schedule starts with the warm-up, whose operations aren't counted, so once it ends the threads run fast until they have caught up, well over `target` for a while.
- **Changes in output** from earlier builds, which matter when comparing runs across them:
  - **Stopped runs, every DB, batches or not.** An operation the run's stop came before isn't run and counts as nothing. An earlier build could still start one more operation after the stop, when the stop came between a worker's check and that operation, and counted it, usually as failed. An operation in flight counts as it ends, as before. `READ_MODIFY_WRITE` counts a failure as `READ_MODIFY_WRITE_ERROR` (earlier builds counted every one as `READ_MODIFY_WRITE`), and counts as nothing when the stop came before its update. A failed `READ_MODIFY_WRITE` thus shows in two rows: its read's or update's `READ_ERROR`/`UPDATE_ERROR`, and `READ_MODIFY_WRITE_ERROR` (at a stop, up to twice `threadcount` error counts from the `READ_MODIFY_WRITE`s in flight): don't sum every `*_ERROR` row to count failed operations. Earlier builds also slept through a stop in the insertion retry back-off and then sent the batch again; now the back-off ends at the stop, with no resend. With `redis`, an operation in flight at the stop used to run on the run's canceled context: a retry's back-off ended at once with `context.Canceled`, counted as an error even where the try before had written the records, and a read in progress waited up to `redis.read_timeout`, which a long one made outlast the force-exit; now it gets the 5 s grace (see "Stopping a run" in the [Redis](#redis) section).
  - **Batch counting.** Earlier builds counted batches differently, for every DB with batch operations (`mysql`, `sqlite`, `tikv`, `basic`) and for the per-record fallback of the others (`mongodb`, `cassandra`, ..., and `redis`, which had no batch operation): a batch was one `BATCH_<OP>` sample (or `BATCH_<OP>_ERROR`), counted in `TOTAL`, and the fallback recorded nothing and stopped at its first failed record. Now `Count`/`OPS` and `TOTAL` are records, `BATCH_<OP>` is in no `TOTAL`, `BATCH_<OP>_ERROR` is gone, and the fallback runs and measures every record. A script keyed on `BATCH_INSERT_ERROR`, or on `TOTAL` with batches, reads something else now.
  - **Batched loads wrote more than asked.** Each thread's last batch took a full `batch.size` keys, so a batched load wrote, and counted, up to `batch.size` − 1 records per thread past `insertstart` + `insertcount`, unless each thread's share was a multiple of `batch.size`. Batched runs likewise overshot `operationcount`; both now cut each thread's last batch to what is left of its share.
  - **Batched runs, other changes.** With `target`, the threads of a batched run or load start at random points of one batch period, so a short throttled one falls a few percent short of `target`. With `warmuptime`, a batch is measured and counted as a whole or not at all, by whether the warm-up was over when it started.
  - **New log lines.** With errors printed (`silence=false`): "operation err: N of M records failed, the first: ..." for a batch with failed records; in a `run`, "operation err: not run: the run stopped: ..." for an operation the stop came before, and, with `redis`, errors for what the end of a stop's grace ended: "use of closed network connection" (a read in progress), "redis: client is closed" (a try after the close) and "context canceled" (a back-off, a pool turn or a dial). A `load` prints none of the stop's errors: its insert, stopped, returns no error.
  - **Batch retries.** With `core_workload_insertion_retry_limit` set, a batch that succeeded was sent again, once per allowed retry, each time counted, and a batch that failed was never retried. Now only a batch's failed records are retried.
  - **A load's warm-up.** With `warmuptime` set, a load started a warm-up anyway: its first records could go unmeasured and, uncounted, could make the threads insert past `insertcount`. A load has no warm-up now.
  - **Client CPU, `redis`.** This build uses go-redis v9.22.0 where earlier builds used v9.8.0. The `redis.*` properties above restore v9.8.0's wire and timing behaviour, not its CPU cost: on reads and updates the client spends some 7.5-10% more CPU per operation than a v9.8.0 build (in a local microbenchmark with `perf stat` and rusage, 5 to 6 alternating runs, no errors, workload A, CPU per operation, a build on go-redis v9.8.0 → this build on v9.22.0: against a fake server 42.8 → 46.0 µs, against a real Redis 8.6 41.4 → 44.5 µs, against a 6-node cluster 59.8 → 65.6 µs), almost all of it inside go-redis (connection pool, push-notification checks, metrics hooks, deadlines). Loads of `redis.datatype=hash` records, such as the feature-store workload's, cost some 20-40% less CPU, because their values are no longer JSON-encoded for nothing. So on a client that is CPU-bound, don't compare throughput across these builds.
- **Caveats.** A batch waits for its slowest target, and its whole part for one server shares one read deadline: see the [Redis](#redis) section for what a stalled master does to a batched load, the settings for it, and a go-redis cluster-pipeline race that can count a written record as failed.

## Output configuration

|field|default value|description|
|-|-|-|
|measurementtype|"histogram"|The mechanism for recording measurements, one of `histogram`, `raw` or `csv`|
|measurement.output_file|""|File to write output to, default writes to stdout|
|measurement.interval|10s|How often the status lines (and interval records) are written: a Go duration such as `1s` or `500ms`, or a number of seconds; at least `100ms`. `--interval <seconds>` sets it too, in whole seconds only, and wins over `-p measurement.interval` when both are given: for a sub-second interval use `-p measurement.interval` alone|
|measurement.interval_output_file|""|With `measurementtype=histogram`: a file that gets one JSON line per operation per interval, with that interval's own latency percentiles (the status lines' percentiles are cumulative since the start)|
|measurement.prometheus_listen|""|Serve `/metrics` at this address for `measurementtype=histogram`; empty disables the exporter. Bind to loopback or a private network: the endpoint has no authentication|
|measurement.prometheus_labels|""|Comma-separated constant labels (`k=v`) on every exported metric, for example `phase=load`; names and values are validated|
|measurement.prometheus_linger|1s|How long to serve final counts after the command finishes, as a non-negative Go duration|

### Prometheus exporter

The opt-in exporter serves live counters, cumulative latency histograms, and the last completed interval's latency measurements. For example,
`-p measurement.prometheus_listen=127.0.0.1:9464 -p measurement.prometheus_labels=phase=run -p measurement.interval=1s -p debug.pprof=127.0.0.1:6060`
uses one-second windows. Set the Prometheus or Alloy scrape interval to one second or less for the best window
coverage, and use its `remote_write` to retain the series in a long-term metrics store. The endpoint holds only the
latest window: slower scrapes skip windows, and scrape timing can skip or repeat one even at equal cadence. Use
`measurement.interval_output_file` when every window must be retained. Set `measurement.prometheus_linger` to at
least the scrape interval so the final counts have a chance to be scraped (the default 1s is for a 1s scrape).
A scrape can still miss the last partial window; the final summary remains the complete record. A busy or invalid
listen address fails at startup.
The exporter uses a private HTTP mux; the existing pprof server is separate and defaults to `:6060` on all interfaces,
so bind it to loopback or another private address too.

| metric | meaning |
|---|---|
| `ycsb_info{workload,command,threadcount,batch_size,target,version}` | process configuration; value 1 |
| `ycsb_phase_running` | 1 from the start of `Client.Run`, 0 after final counts are drained and printed |
| `ycsb_operations_total{op}`, `ycsb_errors_total{op}` | cumulative counts from the same histograms as the final summary; batches and failed records follow the Counting rules above |
| `ycsb_latency_seconds_bucket{op,le}`, `ycsb_latency_seconds_sum{op}`, `ycsb_latency_seconds_count{op}` | cumulative Prometheus histogram for each raw operation name, including `READ_ERROR` and other failures; fixed bucket bounds span 100 µs to 60 s, plus `+Inf` |
| `ycsb_interval_latency_seconds{op,quantile}` | p50, p90, p95, p99 and p99.9 of the last completed interval, in seconds |
| `ycsb_interval_latency_avg_seconds{op}`, `ycsb_interval_latency_max_seconds{op}` | mean and max latency of that interval, in seconds |
| `ycsb_interval_operations{op}` | samples in that interval |
| `ycsb_interval_window_seconds`, `ycsb_interval_end_timestamp_seconds` | length and end time of that interval |

The Prometheus histogram is updated from the same samples as the HDR histogram used for summaries. Its buckets,
count and sum accumulate for the life of the process; `_sum` uses the original nanosecond durations, while HDR
summaries round latencies to microseconds. `rate(ycsb_latency_seconds_bucket[5m])` can feed
`histogram_quantile()` for a rolling latency estimate. For example,
`histogram_quantile(0.99, sum by (le, op) (rate(ycsb_latency_seconds_bucket[5m])))` estimates p99 per operation;
retain the desired run labels in the grouping when scraping multiple runs.

Import [`dashboards/ycsb-latency-heatmap.json`](dashboards/ycsb-latency-heatmap.json) into Grafana for a live
latency heatmap and rolling p50/p99 estimates. Select the Prometheus data source, then one job, instance,
operation and a 30s or 60s window. If your scraper adds labels that distinguish simultaneous runs or phases on
the same target, add selectors for those labels to the panel queries to avoid combining their distributions. The
dashboard needs Prometheus to scrape the exporter; on the same host, a minimal scrape job is:

```yaml
scrape_configs:
  - job_name: go-ycsb
    scrape_interval: 1s
    static_configs:
      - targets: ['127.0.0.1:9464']
```

Adjust the target when Prometheus runs elsewhere. The
heatmap uses `sum by (le) (rate(ycsb_latency_seconds_bucket{...}[$window]))` with the Prometheus query format set to
Heatmap; Grafana converts cumulative buckets into per-range cells. Scrape at 1s for frequent updates. Each column
is a trailing 30s or 60s average of bucket rates, so a 1s scrape does not resolve individual one-second events. The
30s view needs at least two scrapes in its range, and its newest point is delayed by the scrape interval. The
distribution is limited to the exporter's 33 finite bucket boundaries plus `+Inf` (latencies above 60s share that
last bucket); HDR interval quantiles remain
available separately. At 1s scraping, this histogram contributes 36 samples per second per operation (34 buckets,
`_sum`, `_count`), before Prometheus labels and storage overhead. No additional histogram ring or recording work
is needed in go-ycsb for this heatmap. The dashboard refreshes every 5s by default; Grafana's default minimum
refresh interval is 5s, so a 1s dashboard refresh requires changing that Grafana setting.

Future packed HDR windows need more than `PackedHistogram` recording support: the type merged in
[hdrhistogram-go PR #75](https://github.com/HdrHistogram/hdrhistogram-go/pull/75) has no reset, merge or sparse
iteration API for combining 1s slices into a 30s/60s HDR snapshot. Keep the cumulative Prometheus histogram as a
counter; a rolling HDR distribution would need a separate representation and an off-recording-path merge.

`TOTAL` repeats successful per-operation samples, and `BATCH_*` measures batch calls; keep these separate from
record-level operations when aggregating distributions.
Interval series appear after the first interval ends, and an operation's interval series are omitted when it had no
samples in that window. Interval quantiles are gauges for one window: don't apply `rate()` to them. Counters and
histogram series are live and may differ briefly across operations during a scrape. Once `ycsb_phase_running` is
zero, the operation and error counters and histogram `_count` values match the final summary; buckets and sums hold
the final distribution data. External run labels belong in the scraper, not in go-ycsb. Enabling the exporter
keeps per-interval histograms even without an interval file, and the single measurement goroutine also updates the
fixed Prometheus buckets. The worker hot path is unchanged. No existing
output changes when the exporter is off.

Each line of `measurement.interval_output_file` describes one operation over one interval, e.g.:

```json
{"ts":"2026-10-03T21:12:36.55Z","t":1.0004,"window_s":1.0004,"op":"READ","count":32711,"ops":32698.0,"avg_us":487.5,"min_us":94,"max_us":2835,"p50_us":470,"p90_us":644,"p95_us":710,"p99_us":903,"p999_us":1412,"p9999_us":2193,"cum_count":32711}
```

- `ts` is the end of the interval (UTC), `t` the seconds since the measurement started (after warm-up), `window_s` the interval's length; `count` and the latencies (µs) cover that interval only, `ops` is `count / window_s`, and `cum_count` is the running total.
- Every operation seen so far gets a line each interval. An interval in which it had no samples has `count: 0` and no latency fields.
- Failed operations appear under their own names (`READ_ERROR`, ...). The last, partial interval is written when the run ends, also when it's stopped with SIGINT.
- A sample belongs to the interval in which the single measurement goroutine records it, not the one in which the operation ended. That is within microseconds while the goroutine keeps up. If it falls behind (client threads produce samples faster than it records them, which shows as the measure channel filling up), its backlog is recorded late: interval rates and percentiles shift towards later intervals, and at the end of the run the backlog drains into the last interval, whose rate can then exceed anything the database served. Treat a last interval with an implausible `ops` as a saturated client, not as a database result.
- All operations' intervals end at the same instant. `TOTAL` counts the successful operations, so in each interval it matches their sum up to the operation/`TOTAL` pairs a cut falls between (at most one per client thread, each of up to `batch.size` records), and exactly over the run. Three exceptions: `READ_MODIFY_WRITE` records no `TOTAL` sample of its own (its inner `READ` and `UPDATE` do), nor does a `BATCH_<OP>` (its records count as `<OP>`), and with `warmuptime` set a thread can record `TOTAL` for an operation the warm-up dropped.
- The file is created (truncated) at start by both `load` and `run`, so with the property in a shared workload file `run` replaces what `load` wrote; give each command its own path (`-p measurement.interval_output_file=...`) to keep both.
- Without `measurement.interval_output_file` or the Prometheus exporter, no per-interval histograms are kept. Recording a sample now takes a lock (the fix for a data race between recording and reporting), about 11 ns more per sample, uncontended: one goroutine records, the reporter takes it once per interval.

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
|cosmosdb.throughput|400|Manual RU/s for an auto-created container (400 is Cosmos DB's own platform minimum). Only applies to container creation - an auto-created database itself gets no shared throughput of its own, since this adapter is designed around per-container throughput. Ignored if `cosmosdb.autoscale_max_throughput` is set|
|cosmosdb.autoscale_max_throughput|N/A|Autoscale max RU/s for an auto-created container, instead of manual `cosmosdb.throughput`. Same per-container scope as above|
|cosmosdb.consistency_level|N/A|Per-operation consistency override, e.g. "Strong", "Session", "Eventual". The Cosmos DB SDK only allows *relaxing* consistency below the account's own configured default - there is no way for this property to request stronger consistency than the account was provisioned with. If you want Strong consistency end to end, the Cosmos DB **account** itself must be configured with Strong as its default; leave this unset to just inherit that|
|cosmosdb.op_timeout|"10s"|Timeout for every individual point operation (Read/Insert/Update/Delete), via context cancellation. Does not bound Scan - see `cosmosdb.scan_timeout`. Must be a positive duration|
|cosmosdb.scan_timeout|"60s"|Timeout for a whole Scan call, separate from `cosmosdb.op_timeout`: Scan pages through a cross-partition query via multiple round trips until `count` items are collected, so its total duration is a multiple of a single operation's, not comparable to one. Must be a positive duration|
|cosmosdb.insecure_skip_verify|false|Skip TLS certificate verification entirely (insecure; for local/self-signed testing only, e.g. against the Cosmos DB Linux emulator's self-signed certificate)|

Notes:
- **Update merges, and mostly does so atomically.** For a values map within Cosmos DB's 10-operation-per-request `PatchItem` limit (the common case - go-ycsb's core workload defaults to `writeallfields=false`, a single field per Update), Update uses `PatchItem`/`AppendSet`, one operation per field: this sets exactly the fields being updated and leaves every other field on the document untouched, in one round trip, with no read-modify-write race window. A wider values map (reachable with `writeallfields=true` against a table with more than 10 fields - `workloads/workload_feature_store`'s actual default) falls back to Read+merge+Replace, using the Read's ETag for optimistic concurrency (Cosmos DB's equivalent of a CAS token) so a concurrent write landing in between is detected as a conflict and retried (up to 5 additional times, 6 attempts total) rather than silently lost.
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
|redis.addr|localhost:6379|Redis server address(es) in "host:port" form, can be semi-colon `;` separated in cluster mode (where it has no default)|
|redis.username||Redis server username|
|redis.password||Redis server password|
|redis.db|0|Redis server target db|
|redis.max_redirects|0|Cluster mode: how many times a command is retried, on a `MOVED`/`ASK` redirect and on a connection error or timeout; 0 means go-redis's default, 3, and -1 none (below -1 is an error: go-redis would send nothing)|
|redis.read_only|false|Enables read-only commands on slave nodes (only for cluster mode)|
|redis.route_by_latency|false|Allows routing read-only commands to the closest master or slave node (only for cluster mode)|
|redis.route_randomly|false|Allows routing read-only commands to the random master or slave node (only for cluster mode)|
|redis.max_retries|0|Single mode: how many times a command (or a pipeline, whole) is retried on a connection error or timeout; 0 means go-redis's default, 3, and -1 none (below -1 is an error: go-redis would send nothing). In cluster mode see `redis.max_redirects` (0 there means no retries per node)|
|redis.min_retry_backoff|8ms|Minimum backoff between each retry (0 means this default too; -1 none)|
|redis.max_retry_backoff|512ms|Maximum backoff between each retry (0 means this default too; -1 none)|
|redis.dial_timeout|5s|Dial timeout for establishing new connection|
|redis.read_timeout|3s|Timeout for socket reads; 0 means the default, 3 s (go-redis v9.8.0's default; v9.22.0's is 5 s); -2 sets no read deadlines at all; -1 is no timeout in single mode, while in cluster mode it is no deadline for pipelines (batches, and the `MULTI`/`EXEC` of `redis.datatype=json` reads and updates) and 3 s for commands sent on their own, as with v9.8.0 (use -2 there)|
|redis.write_timeout|redis.read_timeout|Timeout for socket writes; 0 means the same as `redis.read_timeout` (so `redis.read_timeout=30s` with `redis.write_timeout=0` is 30 s); -1 and -2 as for `redis.read_timeout`|
|redis.pool_size|threadcount|Maximum number of socket connections (per node in cluster mode)|
|redis.min_idle_conns|redis.pool_size|Minimum number of idle connections: dialed when the client starts, and redialed in the background as connections go (see below for what that does during an outage)|
|redis.max_idle_conns|redis.pool_size|Maximum number of idle connections: 0 means no cap; a negative value closes every connection returned to the pool|
|redis.max_conn_age|-1|Connection age at which client closes the connection; -1 never|
|redis.pool_timeout|redis.read_timeout + 1s|Amount of time client waits for connections are busy before returning an error (from the value given for `redis.read_timeout`: about 1 s for its 0, -1 or -2)|
|redis.idle_timeout|-1|Amount of time after which client closes idle connections. Should be less than server timeout; -1 never|
|redis.tls_ca||Path to CA file|
|redis.tls_cert||Path to cert file|
|redis.tls_key||Path to key file|
|redis.tls_insecure_skip_verify|false|Controls whether a client verifies the server's certificate chain and host name|
|redis.protocol|3|RESP version, 2 or 3|
|redis.maint_notifications|disabled|go-redis maintenance notifications (`CLIENT MAINT_NOTIFICATIONS ON` on every new connection): `disabled`, `auto` (go-redis v9.22.0's default: sent, and dropped if the server rejects it) or `enabled`|
|redis.read_buffer_size|4096|Bytes of each connection's read buffer (go-redis v9.22.0's default is 32768)|
|redis.write_buffer_size|4096|Bytes of each connection's write buffer (go-redis v9.22.0's default is 32768)|
|redis.dialer_retries|1|Dial attempts for a new connection (go-redis's `DialerRetries`: its default is 5, 100 ms apart)|
|redis.max_concurrent_dials|0|Connections dialed at once for callers waiting for one, per node; 0 means `redis.pool_size`, the most go-redis allows. It doesn't limit the dials that keep `redis.min_idle_conns` idle connections|
|redis.routing_policies|false|Cluster mode: go-redis's command routing policies, which look up `COMMAND` information for every command sent on its own. Without them, go-redis v9.22.0 sends a keyless command (`PING`, `DBSIZE`, ...) to any node, replicas included, where v9.8.0 sent it to the master of a "random" slot (its random source had a fixed seed, so nearly always the same master); go-ycsb's own startup check pings a random master. No effect in single mode (a notice says so when it is set to true)|
|redis.cluster_state_reload_interval|10s|Cluster mode: how often the slot map is reloaded (`CLUSTER SLOTS`) without a `MOVED` asking for it (go-redis v9.22.0's default is 60s)|

go-ycsb used go-redis v9.8.0 before v9.22.0; the last eight properties (and the meaning of an explicit 0 for the backoffs and `redis.read_timeout`, and of `redis.read_timeout=-1` in cluster mode) keep the client's wire and resource behaviour by default what it was with v9.8.0 (no `CLIENT MAINT_NOTIFICATIONS`, no `COMMAND` lookups, 4 KiB buffers, one dial attempt, the slot map reloaded every 10 s), so that runs compare across go-ycsb builds, and make go-redis's newer behaviour opt-in. One difference can't be configured back exactly: go-redis tops `redis.min_idle_conns` up in the background after handing out an idle connection and after removing one (both versions), and v9.22.0 also on every miss of a node's pool (a caller finding no idle connection). With go-ycsb's default `redis.min_idle_conns` (`redis.pool_size`, i.e. `threadcount`) a node that is down then sees more background dials. Their effect depends on the load and on the outage, and can go either way: on 17 local masters, one crashed (port closed) for 10 s under an unthrottled load, 800 threads, one record at a time, v9.8.0 counted 16,237 failed records (p99 0.43 s each) and v9.22.0 14,426 (p99 0.55 s); one stalled for 15 s, both failed one record per thread, after 12.1 s. Other setups have shown larger differences (several times the error latency, more or fewer failed records with 800 threads, batched stalls failing 1.3 to 1.7 times the records of `redis.min_idle_conns=0`), which `redis.min_idle_conns=0` brings back to v9.8.0's numbers, at the cost of no connections dialed ahead when the client starts (some 13,600 connections in steady state at 800 threads on 17 masters, about a third without). So error counts and latencies during outages may not compare exactly with go-ycsb builds on go-redis v9.8.0; steady-state behaviour does.

Other differences from go-redis v9.8.0 that no property changes:

- With `redis.read_only=true` and neither `redis.route_by_latency` nor `redis.route_randomly`, a read goes to a replica picked round-robin (one counter for the process) where v9.8.0 picked one at random, and go-redis pings (100 ms timeout) each replica the first time it is picked and after it was marked failing. With one replica per shard the node chosen is the same; only the pings are new.
- TCP keep-alive on new connections: probes after 30 s idle, every 5 s, 3 of them, where v9.8.0 used a 5 min keep-alive period. A dead peer is found sooner on an idle connection. No property sets it.
- go-redis's random source is no longer seeded with 1, so node order, replica choice and retry jitter differ from run to run.
- go-redis retries a few more kinds of errors (`NOREPLICAS`, dial errors wrapped in a deadline, wrapped errors): this changes failure paths only.
- With `redis.read_only`, `redis.route_by_latency` or `redis.route_randomly`, every read-only command and pipeline reads go-redis's `COMMAND` information under one process-wide exclusive lock (v9.8.0 read it without a lock once loaded): possibly more contention at high thread counts with those options; not measured.

With `dropdata=true` the database runs `FLUSHDB` (synchronous unless the server's `lazyfree-lazy-user-flush` is `yes`) under `redis.read_timeout`, each try under the timeout: in cluster mode every master, retried as many times as `redis.max_retries` says (none by default: 0 there means no retries per node); in single mode retried as `redis.max_retries` says (go-redis's 3 by default). Flushing a large database can take longer, and fail the run's start: flush it beforehand, or give that run a longer `redis.read_timeout`.

The durations (`redis.dial_timeout`, `redis.read_timeout`, `redis.write_timeout`, `redis.pool_timeout`, `redis.min_retry_backoff`, `redis.max_retry_backoff`, `redis.max_conn_age`, `redis.idle_timeout`, `redis.cluster_state_reload_interval`) take a Go duration such as `30s` or `500ms`, or an integer number of nanoseconds (`-1` disables `redis.max_conn_age`/`redis.idle_timeout`). Any other value is an error: before, a value such as `30s` was ignored and the default used. The same holds for the integer and boolean `redis.*` properties (booleans take `true`/`false`, `1`/`0`, `yes`/`no`, `on`/`off`): a value such as `redis.read_buffer_size=32k` or `redis.routing_policies=enabled` fails the run, naming the property, where before it was the default (or, for a boolean, false). So some configurations run differently than with earlier builds without a change: `redis.read_timeout=30s`, for one, ran at 3 s and now runs at 30 s.

With `batch.size` above 1, `load` (and a batched `run`'s inserts) writes each batch as one pipeline of the very commands a plain load sends (`HSET`, `SET` or `JSON.SET`, per `redis.datatype`), not a `MULTI`/`EXEC` transaction: every record succeeds or fails on its own and is counted so (see [Batched loads](#batched-loads)). In cluster mode the pipeline is split by the master that owns each key's slot, and the masters' parts are sent concurrently, one connection each; a batch ends when its slowest master has answered. Reads, updates and deletes stay one command per record.

A batch spreads over every master, so it waits for a stalled master (say, during an AOF rewrite or a slow disk). Batching raises the rate between stalls; it does not let the other masters go on during one. That would need records buffered per master, about the master's share of the rate times the stall, and threads that don't wait for a batch's slowest master, which this client doesn't do.

A stall also costs a batched load more errors than a plain one, under the same timeouts. Every thread has a batch in flight, so a master has about `threadcount × batch.size / masters` records queued at once (some 3,000 for 256 threads, batches of 200 and 17 masters; one per thread, at most, without batches). `redis.read_timeout` (3 s by default) bounds each read of a master's part of a batch, all its records' replies; on a timeout go-redis resends that whole part, up to `redis.max_redirects` times (0, the default, means go-redis's 3; -1 means none), with a backoff between tries (`redis.min_retry_backoff`..`redis.max_retry_backoff`). So a stalled master's records fail only after about (`redis.max_redirects` + 1) × `redis.read_timeout` plus the backoffs: some 12 s by default (12.1 s measured), some 2 minutes at `redis.read_timeout=30s`. A crashed master is another matter: its closed port refuses each try at once, so its records fail after the tries' backoffs, well under a second to a second or so (p99 0.55 s one record at a time, 1.2 s for batches of 200, on 17 local masters), whatever the timeout. A master that is slow, or stalls for longer than the tries take, fails its share of every thread's batch, where one record at a time fails one record per thread at first. Either way, a stall that outlasts the tries fails more: connections that timed out are redialed into the stalled master, and once `redis.pool_size` dials to it have failed go-redis fails the next records at once, with the last dial error, until it answers again (9 to 31 failed records per thread in a 15 s stall, one record at a time, have been measured, in go-redis v9.8.0 and v9.22.0 alike). This is cluster mode; in single mode the retries are `redis.max_retries` (0 means go-redis's 3), and a retry resends the whole pipeline. For a batched load against masters that can stall:

- set `redis.read_timeout` above the longest stall you expect, e.g. `30s`: one long wait instead of resends of every batch's part into a master that is stalled anyway;
- retry the failed records: `core_workload_insertion_retry_limit=3`, `core_workload_insertion_retry_interval=1`;
- if a long timeout isn't acceptable (a master that stalls for good then takes (`redis.max_redirects` + 1) × `redis.read_timeout` to fail; a crashed one fails fast either way), use smaller batches;
- don't raise `redis.max_redirects`: besides `MOVED`/`ASK` redirects it retries connection errors and timeouts, and each try resends the master's whole part of every batch into it.

A record that timed out has often been written all the same (the master ran it, the reply came late), so `insertcount − INSERT` is an upper bound on the records missing, not their count: compare with `DBSIZE`.

Stopping a run (SIGINT, `timeout -s INT`):

- An operation already sent (or waiting for a connection: go-redis doesn't tell the two apart) runs to its outcome, retries included, for up to 5 s after the stop: the grace.
- At the end of the grace whatever is still in flight is ended, and counts as failed. The client is closed, which fails a read in progress ("use of closed network connection") and every try after it ("redis: client is closed"). 0.2 s later the operations' context is canceled, which ends what they wait for on it: a retry's back-off, a pool turn or a dial (`context.Canceled`).
- So the summary comes at most some 5.2 s after the stop (at once when nothing is in flight), before go-ycsb's force-exit 10 s after it, whatever `redis.read_timeout`, `redis.dial_timeout` or the retry settings.
- One exception: with `redis.read_only=true` or `redis.routing_policies=true`, go-redis fetches `COMMAND` information on a context of its own. While that information has never been fetched (`COMMAND` denied by an ACL, say), a fetch waiting to connect to an unreachable node outlasts the cancel by up to `redis.dial_timeout`.
- A batch the end of the grace finds in flight, in cluster mode: the stalled master's records fail (with `redis.max_redirects=-1`, only those it hadn't answered yet), and those the other masters answered count as inserted. Unless that batch was still retrying (with a large `redis.max_redirects`) or still waiting to connect to a master (at any setting): go-redis then reports every record of the batch failed, the ones the healthy masters wrote too, and `INSERT` undercounts what was written. For exact per-master outcomes at a stop, use `redis.max_redirects=-1`.
- In single mode every record of the pipeline in flight fails. Either way the batch counts once in `BATCH_INSERT`.
- So after a stop during a stall, `INSERT_ERROR` holds at least the stalled master's share of each thread's batch in flight (in single mode, whole batches): records a stalled server may have run all the same. The upper bound, `INSERT` + `INSERT_ERROR`, can be well above what was written.
- The client belongs to the DB instance, and every run on the instance shares it: the end of one run's grace closes it for all of them, and their operations then fail ("redis: client is closed"), stopped or not. go-ycsb's CLI has one run per process. An embedder running several runs at once needs an instance (a `Create`) per run; runs one after the other on one instance are not affected (a run whose threads are done arms no close).

Caveat, in go-redis's cluster pipelines (seen in v9.8.0 and, by reading, still in v9.22.0): a command can be queued twice for the next try, on two nodes, and run on both, e.g. when one pipeline gets a `MOVED` reply and loses its connection while the client's slot map is stale; plain timeouts were seen to trigger it too. The record is written, but can be reported failed: `INSERT_ERROR` can then count a record that is in, never the reverse.

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

# Redis adapter: a short load and run against a dockerized single Redis and a
# 3-master Redis Cluster with the adapter's defaults (INSERT = DBSIZE, no
# errors, no COMMAND or CLIENT MAINT_NOTIFICATIONS on the nodes)
make test-integration-redis

# Batched loads (batch.size 1, 7, 100) into a dockerized single Redis and a
# 6-node Redis Cluster: DBSIZE, key set and values checked against batch.size=1
make test-integration-redis-batch

# Cosmos DB adapter (core + feature-store workloads, a cross-partition Scan,
# a field-preservation regression check, and an
# auto_create_container=false negative check) against a dockerized Azure
# Cosmos DB (vNext) Linux emulator
make test-integration-cosmosdb

# Cosmos DB TLS support (cosmosdb.insecure_skip_verify) - the emulator above
# only serves plain HTTP, so this goes through a TLS-terminating proxy
# (same pattern as the Aerospike TLS test) to assert rejected-by-default /
# accepted-with-skip-verify=true
make test-integration-cosmosdb-tls
```

All of these integration tests run in CI on every push/PR to `master` (see `.github/workflows/integration.yml`); see [CONTRIBUTING.md](CONTRIBUTING.md) for the full testing/review bar for PRs.

## TODO

- [ ] Support more measurement, like HdrHistogram
- [ ] Add tests for generators

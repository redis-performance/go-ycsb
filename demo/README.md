# Live latency heatmap demo

This disposable stack runs go-ycsb against a local Redis container, scrapes
the exporter every second with Prometheus, and provisions the repository's
Grafana dashboard. Its traffic is illustrative; it is not a performance result.

From the repository root:

You need a running Docker Engine and Docker Compose v2 (`docker compose`).
Allow enough local resources to compile the Go binary and run the local stack.

```sh
YCSB_DEMO_VERSION="$(git describe --always --dirty)" docker compose -f demo/compose.yaml up --build -d
```

Open [the dashboard](http://127.0.0.1:3000/d/ycsb-latency-heatmap/ycsb-live-latency-distribution?var-DS_PROMETHEUS=demo-prometheus&var-job=go-ycsb&var-instance=ycsb%3A9464&var-op=READ&var-window=30s&from=now-5m&to=now).
The heatmap needs two scrapes before it has data; the rolling 30s and 60s HDR
gauges need 30 and 60 completed reporter slices. The exporter is also at
<http://127.0.0.1:9464/metrics>, and the latest full HDR buckets are at
<http://127.0.0.1:9464/hdr-windows>. Prometheus is at <http://127.0.0.1:9090>.

The first `load` container inserts 10,000 disposable records, then the `ycsb`
container runs Workload A (50% reads, 50% updates) at a target of 1,000 ops/s
for up to 1.8 million operations (about 30 minutes). Redis and the YCSB exporter have no
host exposure beyond the loopback exporter port. Grafana and Prometheus also
bind only to loopback. To stop the traffic and remove the sample data:

```sh
docker compose -f demo/compose.yaml down -v
```

The dashboard's minute heatmap uses the exporter's fixed Prometheus buckets.
Its cells estimate counts over trailing 60-second intervals; they are not
aligned to wall-clock minute boundaries, and Grafana may combine intervals when
you zoom out.
The full packed HDR bucket arrays are available live at `/hdr-windows`; historical
full-HDR minute heatmaps need a separate data-source ingestion step.

## Sample capture

![Live Redis latency heatmap in Grafana](sample-heatmap.png)

This screenshot is an illustrative local run, not a Redis or go-ycsb performance
claim. It shows READ operations, the fixed-bucket trailing-minute heatmap, and
the packed HDR quantile panel. The sample was captured with a two-minute
dashboard range.

| Capture detail | Value |
| --- | --- |
| Capture time | 2026-10-07 13:40:47 UTC |
| Local run ID | YCSB container `8c1fc2983b21` (started 2026-10-07 13:38:51 UTC) |
| go-ycsb source/build | Commit `926e385`, as exported by `ycsb_info` |
| Data node | One local Redis 8.10.2 container on an arm64 Colima VM; 10,000 keys |
| Neptune operator build | N/A; no Neptune cluster was used |
| Errors | 0 READ and 0 UPDATE run-phase errors; separate load container exited 0 |
| Profiled | No |

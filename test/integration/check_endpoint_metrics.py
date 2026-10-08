#!/usr/bin/env python3
"""Check go-ycsb's per-endpoint metrics (measurement.prometheus_endpoints)
against the run's final summary and the server's topology.

Used by redis.sh; kept separate so it can be linted and run on its own:

    check_endpoint_metrics.py METRICS RUN_LOG MODE ENDPOINTS [MASTERS]

METRICS is the exporter's /metrics after the run's final counts
(ycsb_phase_running 0), RUN_LOG the run's output, MODE single or cluster,
ENDPOINTS the comma-separated addresses the client dials (every node of the
cluster, or the single address), MASTERS (cluster) the masters among them.

On a healthy, stable cluster each operation is exactly one request to one
endpoint, so per operation the endpoints' request counts add up to the
summary's count: a connection's set-up (HELLO, CLIENT SETINFO, ...) or a
topology read counted as a request, or a request missed or counted twice,
breaks the sum. Every master serves requests, none is a failure, redirect or
client-ended request, and in cluster mode every endpoint the requests went
to has an identity (ycsb_endpoint_info) under the same address: the join
the dashboards rely on.
"""

import re
import sys

SERIES = re.compile(r'^(\w+)\{([^}]*)\} (\S+)$')
LABEL = re.compile(r'(\w+)="((?:[^"\\]|\\.)*)"')
ROW = re.compile(r"^(\w+)\s+- Takes\(s\): [\d.]+, Count: (\d+),", re.MULTILINE)
OUTCOMES = ("_ERROR", "_REDIRECT", "_CANCELED")


def fail(msg):
    print(f"FAIL: endpoint metrics: {msg}")
    sys.exit(1)


def main(metrics_path, log_path, mode, endpoints, masters=""):
    endpoints = set(endpoints.split(","))
    masters = set(filter(None, masters.split(",")))
    counts = {}  # (endpoint, op) -> requests
    info = {}  # endpoint -> labels
    running = None
    with open(metrics_path) as f:
        for line in f:
            m = SERIES.match(line.strip())
            if not m:
                if line.startswith("ycsb_phase_running "):
                    running = float(line.split()[1])
                continue
            name, labels, value = m.group(1), dict(LABEL.findall(m.group(2))), float(m.group(3))
            if name == "ycsb_phase_running":
                running = value
            elif name == "ycsb_endpoint_latency_seconds_count":
                counts[(labels["endpoint"], labels["op"])] = value
            elif name == "ycsb_endpoint_info":
                info[labels["endpoint"]] = labels
    if running != 0:
        fail(f"scraped before the final counts (ycsb_phase_running {running})")
    if not counts:
        fail("no ycsb_endpoint_latency_seconds series")

    with open(log_path) as f:
        log = f.read()
    final = log[log.rindex("Run finished"):]
    summary = {op: int(n) for op, n in ROW.findall(final)
               if op != "TOTAL" and not op.endswith("_ERROR")}
    if not summary:
        fail("no operations in the final summary")

    bad = {k: v for k, v in counts.items() if k[1].endswith(OUTCOMES) and v}
    if bad:
        fail(f"failed, redirected or client-ended requests on a healthy server: {bad}")
    stray = {e for e, _ in counts} - endpoints
    if stray:
        fail(f"requests to endpoints the client doesn't dial: {stray} (want within {endpoints})")
    for op, want in summary.items():
        got = sum(v for (e, o), v in counts.items() if o == op)
        if got != want:
            fail(f"{op}: {got:.0f} requests over the endpoints, want the summary's {want} (one per operation)")
        print(f"OK: endpoint metrics: {op}: {got:.0f} requests over the endpoints = summary")
    unexpected = {o for (_, o), v in counts.items() if v and not o.endswith(OUTCOMES)} - set(summary)
    if unexpected:
        fail(f"requests under operations the run didn't count: {unexpected}")
    served = {e for (e, o), v in counts.items() if v and not o.endswith(OUTCOMES)}
    if mode == "cluster":
        idle = masters - served
        if idle:
            fail(f"masters with no requests: {idle}")
        missing = served - set(info)
        if missing:
            fail(f"endpoints with requests but no ycsb_endpoint_info: {missing} (info has {sorted(info)})")
        unknown = endpoints - set(info)
        if unknown:
            fail(f"cluster nodes with no ycsb_endpoint_info: {unknown}")
        for e in masters:
            if info[e].get("role") != "master" or info[e].get("shard") != info[e].get("node_id"):
                fail(f"{e}: info {info[e]}, want role master and shard = node_id")
        print(f"OK: endpoint metrics: {len(served)} masters served, every endpoint joins its CLUSTER NODES identity")
    elif served != endpoints:
        fail(f"single: requests went to {served}, want {endpoints}")
    else:
        print(f"OK: endpoint metrics: single endpoint {next(iter(endpoints))}")


if __name__ == "__main__":
    if len(sys.argv) not in (5, 6):
        print(__doc__)
        sys.exit(2)
    main(*sys.argv[1:])

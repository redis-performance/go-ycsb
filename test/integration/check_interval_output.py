#!/usr/bin/env python3
"""Check a go-ycsb measurement.interval_output_file against the run's log.

Used by interval_output.sh; kept separate so it can be linted and run on its
own:

    check_interval_output.py INTERVALS_JSONL RUN_LOG THREADCOUNT LAUNCH_WALL INTERVAL_S

LAUNCH_WALL is the wall clock (Unix seconds) just before go-ycsb was started,
INTERVAL_S the configured interval. The run must have had no warm-up.

The checks follow what the run itself recorded, not how long it was meant to
take, so a slow runner (late start, stalls, a long final drain, a wall-clock
step) passes and malformed records fail.
"""

import json
import re
import statistics
import sys
import time
from datetime import datetime, timezone
from itertools import pairwise

ORDER = ["min_us", "p50_us", "p90_us", "p95_us", "p99_us", "p999_us", "p9999_us", "max_us"]
ROW = re.compile(r"^(\w+)\s+- Takes\(s\): ([\d.]+), Count: (\d+),.*?Min\(us\): (\d+), Max\(us\): (\d+),", re.MULTILINE)


def ts_seconds(ts):
    """RFC 3339 with up to nanoseconds (Go trims trailing zeros) to Unix seconds."""
    whole, _, frac = ts.rstrip("Z").partition(".")
    sec = datetime.strptime(whole, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
    return sec + (float("0." + frac) if frac else 0.0)


def is_success(op):
    return op != "TOTAL" and not op.endswith("_ERROR")


def check(recs, text, threads, launch, interval):
    fail = []
    # the final summary: operation -> (Takes(s), Count, Min(us), Max(us))
    summary = text[text.find("Run finished") :]
    final = {
        m.group(1): (float(m.group(2)), int(m.group(3)), int(m.group(4)), int(m.group(5)))
        for m in ROW.finditer(summary)
    }
    if "Run finished" not in text:
        fail.append("no final summary: the run didn't end cleanly on SIGINT")

    by = {}
    for r in recs:
        by.setdefault(r["op"], []).append(r)
    # ops cut short by the SIGINT can show up as *_ERROR
    expected = {"READ", "UPDATE", "TOTAL"}
    if not expected <= set(by) or any(op not in expected and not op.endswith("_ERROR") for op in by):
        fail.append(f"ops {sorted(by)}")

    # One cut per ts: every record of it carries the same t and window_s.
    cuts = {}
    for r in recs:
        c = cuts.setdefault(r["ts"], (r["t"], r["window_s"]))
        if (r["t"], r["window_s"]) != c:
            fail.append(f"records at ts {r['ts']} disagree: t/window_s {c} vs {(r['t'], r['window_s'])} ({r['op']})")
    cut = sorted((t, w, ts) for ts, (t, w) in cuts.items())
    ts_s = [ts_seconds(ts) for _, _, ts in cut]

    # The cuts tile the run from its start: the first ends one window after
    # t=0, and each starts where the previous ended (no gap, no overlap).
    if cut and abs(cut[0][0] - cut[0][1]) > 0.25:
        fail.append(f"first interval ends at t={cut[0][0]} after a {cut[0][1]}s window: t doesn't start at 0")
    for (t0, _, _), (t1, w1, ts1) in pairwise(cut):
        if abs((t1 - w1) - t0) > 1e-3:
            fail.append(f"interval {ts1} starts at t={t1 - w1:.3f}s, the previous ended at t={t0:.3f}s")

    # ts is the wall clock at the end of each interval: increasing, today, and
    # advancing with t (loosely: the wall clock can step, t is monotonic).
    # ts - t is then the wall clock when measuring started, which is after
    # go-ycsb was launched; a ts taken at the interval's start would put it
    # about one interval before the launch.
    if any(b <= a for a, b in pairwise(ts_s)):
        fail.append("ts isn't increasing with t")
    if any(abs(s - time.time()) > 86400 for s in ts_s):
        fail.append(f"ts {cut[0][2]} .. {cut[-1][2]} isn't within a day of now")
    if any(abs((ts_s[i] - ts_s[0]) - (cut[i][0] - cut[0][0])) > 0.5 for i in range(len(cut))):
        fail.append("ts doesn't advance with t")
    for (t, _, ts), s in zip(cut, ts_s):
        if not launch - 0.5 <= s - t <= launch + 60:
            fail.append(
                f"interval {ts}: ts - t is {s - t - launch:+.3f}s from the launch; "
                "ts must be the end of the interval, measuring starts after the launch"
            )
            break

    # Windows of the configured interval, checked loosely enough for a busy CI
    # runner: a stall stretches one window and shortens the next (the ticker
    # keeps its schedule), a pause over one interval drops a tick, so single
    # windows can be far off; none before the last is over 10 s. Their mean
    # (not the median, which a few stall pairs move) stays near the interval
    # when there are enough windows to tell: well inside half or double it, the
    # errors that matter. The last window runs until the final drain, so it has
    # no upper bound.
    ws = [w for _, w, _ in cut]
    if any(w <= 0 for w in ws) or any(w > 10 for w in ws[:-1]):
        fail.append(f"windows {[round(w, 3) for w in ws]}")
    if len(ws) - 1 >= 5 and not 0.75 * interval <= statistics.mean(ws[:-1]) <= 1.5 * interval:
        fail.append(f"mean window {statistics.mean(ws[:-1]):.3f}s, want about {interval}s: {[round(w, 3) for w in ws]}")

    seq = [ts for _, _, ts in cut]
    for op, rs in by.items():
        # From its first sample on, an operation has a record at every cut.
        first = seq.index(rs[0]["ts"]) if rs[0]["ts"] in seq else len(seq)
        if [r["ts"] for r in rs] != seq[first:]:
            fail.append(f"{op}: records skip intervals (it first appears at {rs[0]['ts']})")
        cum = 0
        lo = hi = None
        for r in rs:
            cum += r["count"]
            if r["count"] < 0 or r["cum_count"] != cum:
                fail.append(
                    f"{op}: count {r['count']}, cum_count {r['cum_count']}, want the running sum {cum} at {r['ts']}"
                )
            if r["window_s"] > 0 and abs(r["ops"] - r["count"] / r["window_s"]) > 1e-6 * max(1.0, r["ops"]):
                fail.append(f"{op}: ops {r['ops']} isn't count/window_s at {r['ts']}")
            if r["count"] == 0:
                continue
            missing = [k for k in ORDER + ["avg_us"] if k not in r]
            if missing:
                fail.append(f"{op}: {r['count']} samples at {r['ts']} but no {', '.join(missing)}")
                continue
            vals = [r[k] for k in ORDER]
            if vals != sorted(vals) or not r["min_us"] <= r["avg_us"] <= r["max_us"]:
                fail.append(f"{op}: latencies out of order at {r['ts']}: { {k: r[k] for k in ORDER + ['avg_us']} }")
            lo = r["min_us"] if lo is None else min(lo, r["min_us"])
            hi = r["max_us"] if hi is None else max(hi, r["max_us"])

        # Summed against what the run recorded, not against how long it was
        # meant to run: a slow runner may start late or stop late.
        if op not in final:
            if not op.endswith("_ERROR"):
                fail.append(f"{op}: not in the final summary")
            continue
        takes, count, smin, smax = final[op]
        if cum != count:
            fail.append(f"{op}: intervals sum to {cum}, the final summary says {count}")
        # The windows and the summary record the same samples into histograms
        # of the same configuration, so their extremes match exactly: this
        # catches a unit or scale error in the interval latencies.
        if (lo, hi) != (smin, smax) and count > 0:
            fail.append(f"{op}: interval latencies span {lo}..{hi} us, the final summary says Min {smin}, Max {smax}")
        # ... and cover the operation's measured duration (Takes(s), timed
        # from its first sample) to within one window.
        span = rs[-1]["t"] - (rs[0]["t"] - rs[0]["window_s"])
        if abs(span - takes) > max(1.0, max(ws)):
            fail.append(f"{op}: windows span {span:.3f}s, the final summary says Takes(s) {takes}")

    # TOTAL counts the successful operations. Each client thread records the
    # operation, then TOTAL, through one FIFO channel that a single goroutine
    # drains, so a cut sees a prefix of it: up to the cut, the operations lead
    # TOTAL by at most one sample per thread, and never trail it. (With a
    # warm-up a thread could record TOTAL but not its operation, which is why
    # the run must have none.) Over the run, they match exactly.
    total_by_ts = {r["ts"]: r["count"] for r in by.get("TOTAL", [])}
    ops_cum = total_cum = 0
    for ts in seq:
        ops_cum += sum(r["count"] for r in recs if r["ts"] == ts and is_success(r["op"]))
        total_cum += total_by_ts.get(ts, 0)
        if not 0 <= ops_cum - total_cum <= threads:
            fail.append(
                f"interval {ts}: operations so far {ops_cum}, TOTAL so far {total_cum}: "
                f"want the operations ahead by 0..{threads}"
            )
    if ops_cum != total_cum:
        fail.append(f"over the run TOTAL is {total_cum}, the operations {ops_cum}")
    return fail, cut, by


def main(argv):
    path, log, threads, launch, interval = argv[1], argv[2], int(argv[3]), float(argv[4]), float(argv[5])
    with open(path) as f:
        recs = [json.loads(line) for line in f]
    with open(log, errors="replace") as f:
        text = f.read()
    fail, cut, by = check(recs, text, threads, launch, interval)
    if fail:
        print("FAIL:\n  " + "\n  ".join(fail))
        return 1
    print(
        f"ok: {len(recs)} interval records in {len(cut)} intervals, "
        f"{', '.join(f'{op} {len(rs)}' for op, rs in sorted(by.items()))}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

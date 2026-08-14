#!/usr/bin/env python3
"""Summarize load-harness reports for one or more connection models.

Reads reports/<model>/loadgen-*.json (written by load_test.sh via RUN_TAG)
and prints a side-by-side table. Usage: summarize_models.py pool conns mux
"""
import glob
import json
import os
import re
import sys


def load(model):
    files = sorted(glob.glob(os.path.join("reports", model, "loadgen-*.json")))
    if not files:
        return None
    agg = {
        "total": 0, "ok": 0, "integrity": 0, "avail": 0, "retries": 0,
        "p50": [], "p95": [], "p99": [], "gens": 0,
    }
    for f in files:
        with open(f) as fh:
            d = json.load(fh)
        agg["total"] += d["total"]
        agg["ok"] += d["ok"]
        agg["integrity"] += d["integrity_failures"]
        agg["avail"] += d["availability_fails"]
        agg["retries"] += d["routing_retries"]
        agg["p50"].append(d["latency_p50_ms"])
        agg["p95"].append(d["latency_p95_ms"])
        agg["p99"].append(d["latency_p99_ms"])
        agg["gens"] += 1
    return agg


def duration_seconds(model):
    """Recover the load-phase length from the run log, for req/s."""
    log = os.path.join("reports", "run-%s.log" % model)
    try:
        with open(log) as fh:
            text = fh.read()
    except OSError:
        return None
    m = re.search(r"Load running for (\d+)m", text)
    if m:
        return int(m.group(1)) * 60
    m = re.search(r"Load running for (\d+)s", text)
    return int(m.group(1)) if m else None


def chaos_events(model):
    path = os.path.join("reports", model, "chaos.log")
    try:
        with open(path) as fh:
            return sum(1 for _ in fh)
    except OSError:
        return 0


def main(models):
    hdr = ("%-8s %9s %8s %9s %7s %8s %7s %7s %7s %7s" %
           ("model", "requests", "req/s", "success%", "integ", "availf",
            "retries", "p50", "p95", "p99"))
    print(hdr)
    print("-" * len(hdr))
    rows = {}
    for m in models:
        agg = load(m)
        if not agg:
            print("%-8s %s" % (m, "no reports found"))
            continue
        secs = duration_seconds(m)
        rps = ("%.0f" % (agg["total"] / secs)) if secs else "?"
        succ = 100.0 * agg["ok"] / agg["total"] if agg["total"] else 0.0
        # Report the worst generator for tail latency, not an average of
        # percentiles (averaging percentiles is meaningless).
        print("%-8s %9d %8s %9.3f %7d %8d %7d %7d %7d %7d" % (
            m, agg["total"], rps, succ, agg["integrity"], agg["avail"],
            agg["retries"], max(agg["p50"]), max(agg["p95"]), max(agg["p99"])))
        rows[m] = (agg, chaos_events(m))

    print()
    for m, (agg, chaos) in rows.items():
        print("%-8s chaos events: %d" % (m, chaos))
    print()
    print("p50/p95/p99 are the worst across load generators, in ms.")
    print("integ must be 0 in every model; any non-zero value fails the run.")


if __name__ == "__main__":
    args = sys.argv[1:] or ["pool", "conns", "mux"]
    main(args)

# gRPC tunnel load & chaos harness

Stands up the full gRPC tunnel path at small-fleet scale, runs sustained
randomized traffic through the real broker ingress, kills and restarts
components mid-run, and validates byte-exact integrity plus availability.

```
loadgen (xT) ──▶ grpc-tunnel-server (xX) ──▶ agent-a/b/c (xN per token) ──▶ echo-server
    │                    │
    │                    │ BROKER_SERVER notifications
    │                    ▼
    └──"who has tok?"─▶ dispatcher-mock  (plays Cortex's routing role)
```

- **echo-server.go** — deterministic upstream: caller-chosen delay,
  status, and response size; responses are a seeded PRNG stream so the
  load generator re-derives and byte-compares them. Echoes the request
  body hash, request id, and the accept-file-injected header.
- **dispatcher-mock.go** — receives the tunnel servers' real
  `client-connected` / `client-disconnected` / `server-starting` /
  `server-stopping` notifications (the same API Cortex implements) and
  answers `GET /servers/{hashedToken}` for routing. Also `/state` (full
  dump + event log) and `/probe` (fans out to server healthz).
- **loadgen.go** — worker pool; each request resolves a server via the
  mock, POSTs a seeded random body through
  `/broker/{token}/echo/{id}?...`, and validates everything. Infra
  errors (a response without echo-server headers) are retried with
  re-resolution — the way Cortex would — and counted as
  `routing_retries`; only exhausted retries hurt availability;
  integrity failures are always fatal.

## Run

```bash
make -C agent load-test
# or, with knobs:
SERVERS=5 AGENTS_PER_TOKEN=3 LOADGENS=3 WORKERS=32 DURATION=10m \
  CHAOS_INTERVAL=15 ./load_test.sh
```

Requires Docker + the repo checked out (images build from source).
Not part of per-PR CI — it's a multi-minute soak; wire it into a nightly
workflow via `make -C agent load-test` if desired.

| Knob | Default | Meaning |
|------|---------|---------|
| `SERVERS` | 3 | tunnel-server replicas |
| `AGENTS_PER_TOKEN` | 2 | agents sharing each of the 3 tokens (N = 3×this) |
| `LOADGENS` | 2 | load generator replicas |
| `WORKERS` | 16 | concurrent workers per load generator |
| `DURATION` | 3m | load phase length |
| `CHAOS` | 1 | 0 disables the kill/restart loop |
| `CHAOS_INTERVAL` | 20 | seconds between chaos events |
| `MIN_SUCCESS_PCT` | 99 | availability floor per load generator |
| `MAX_BODY_BYTES` / `MAX_RESP_BYTES` | 2MiB / 4MiB | payload ceilings (exercise chunking) |
| `CONN_MODE` | pool | agent connection model: `pool`, `conns`, `mux` or `direct` |
| `CONNS` | 8 | connection count for `conns` / `mux` / `direct` |
| `STREAMS_PER_CONN` | 8 | streams multiplexed per connection (`mux` only) |
| `IDLE_STREAMS` | 4 | idle streams held ready (`direct` only) |
| `MAX_STREAMS` | 64 | per-agent stream ceiling (`direct` only) |
| `RUN_TAG` | `$CONN_MODE` | subdirectory under `reports/` for this run's artifacts |

## Comparing connection models

`compare_conn_models.sh` runs the harness once per connection model at
identical topology, load and chaos rate, then prints a side-by-side table:

```bash
./compare_conn_models.sh                      # pool, conns, mux, direct
MODELS="mux direct" DURATION=10m ./compare_conn_models.sh
```

`pool`, `conns` and `mux` are sized to the **same concurrent stream count**
(`STREAMS`, default 16), so comparing them isolates connection count rather
than concurrency:

| Model | Connections | Streams/conn | Shape |
|-------|-------------|--------------|-------|
| `pool` | grows 2→16 | 1 | adaptive watermark (default) |
| `conns` | 16 | 1 | fixed fan-out, one connection per stream |
| `mux` | 2 | 8 | few connections, HTTP/2 multiplexing (snyk-broker's shape) |
| `direct` | 2, round-robin | 1 | on-demand streams, 4 idle held ready, 32 max |

`direct` is deliberately **not** held to `STREAMS`. Its claim is that a fixed
concurrency number is the thing to delete — streams follow demand, and the
ceiling exists only so an overloaded agent pushes back instead of accepting
work it cannot do. Pinning it to a fixed stream count would test that claim
by assuming its opposite. It runs at the same connection count as `mux`, so
the `direct`-vs-`mux` pair isolates exactly one change: on-demand streams and
round-robin spread instead of a fixed multiplex over pinned connections.

Note also that `direct` ignores `MAX_STREAMS_PER_SERVER`. Spread comes from
the round-robin balancer placing streams across every instance, so a
per-server cap could only reject placements the balancer had already made
well.

Note `MAX_STREAMS_PER_SERVER` counts *connections*, not streams — in `mux`
mode the streams sharing a connection all land on the same backend, so only
the first takes a server slot. The comparison raises it to 4 so it never
binds before the model's own connection count does.

Per-model artifacts land in `reports/<model>/`; re-summarize any time with
`python3 summarize_models.py pool conns mux`.

## Reading the results

- `reports/loadgen-*.json` — per-generator: totals, success %, latency
  p50/p95/p99, `routing_retries` (expect these clustered around chaos
  events; see `chaos.log` timestamps), and details for any failures.
  `integrity_failures` must be 0 — any non-zero value means bytes were
  corrupted or misrouted end to end and the run fails regardless of
  rates.
- `samples.log` — every 10s: each server's `/healthz` (streams,
  inflight) via the mock's `/probe`, plus one agent's
  `__axon/broker/systemcheck` (`streams`/`busy`/`target`) — watch the
  watermark pool grow toward max under load and shrink back to min
  after the run (slot idle timeout is 60s here).
- `chaos.log` — timestamped kill/restart events.
- The script's final phase validates steady-state recovery: after chaos
  stops and a 90s quiesce, every token must be routable again.

## What it exercises that unit/e2e tests don't

- Multi-instance topology: slot spread across servers
  (`MAX_STREAMS_PER_SERVER`), multiple agents per token, routing via
  the notification protocol under churn.
- Sustained concurrency with multi-MiB bodies both directions
  (CallData chunking, streaming, backpressure) for minutes at a time.
- SIGKILL recovery paths on both sides while traffic is in flight.

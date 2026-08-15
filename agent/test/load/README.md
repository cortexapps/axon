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
| `CONNS` | 4 | outbound tunnel connections per agent |
| `RUN_TAG` | `default` | subdirectory under `reports/` for this run's artifacts |

## Why the agent is shaped this way

The agent holds a fixed set of connections and opens streams on demand, with
no watermark, growth step or concurrency setting. That shape was chosen by
measuring it against the three alternatives it replaced, at identical
topology, load and chaos rate (5 servers, 2 agents/token, 2 load generators
x 30 workers, 6m each, chaos every 30s, 11 events):

| model | req/s | success% | integ | avail fails | p50 | p95 | p99 |
|-------|-------|----------|-------|-------------|-----|-----|-----|
| adaptive watermark pool, 16 conns | 121 | 99.986 | 0 | 6 | 47 | 1282 | 9144 |
| fixed 16 streams / 16 conns | 122 | 99.995 | 0 | 2 | 47 | 1306 | 8669 |
| 2 conns x 8 multiplexed streams | 232 | 99.968 | 0 | 27 | 48 | 956 | 1979 |
| **on-demand streams, 2 conns** | **281** | 99.994 | 0 | 6 | **40** | **882** | **1024** |

2.3x the throughput of the fixed-stream models at a 9x better p99, on 2
connections rather than 16, with availability and integrity intact.

The p99 gap is the fixed stream count, and `acquire_wait_ms` is the direct
evidence rather than an inference: the fixed models accumulated mean waits of
1-4.8 **seconds** purely queueing for a free stream, while the on-demand
model served 2.3x the requests with fewer waits per server. Callers were
paying seconds for a stream slot, not for the upstream.

The multiplexed model also had by far the most availability failures despite
using few connections, which is the blast-radius argument: 8 streams pinned
to one connection all die together.

## Reading the results

- `reports/loadgen-*.json` — per-generator: totals, success %, latency
  p50/p95/p99, `routing_retries` (expect these clustered around chaos
  events; see `chaos.log` timestamps), and details for any failures.
  `integrity_failures` must be 0 — any non-zero value means bytes were
  corrupted or misrouted end to end and the run fails regardless of
  rates.
- `samples.log` — every 10s: each server's `/healthz` (streams,
  inflight, and the `acquire_waits`/`acquire_wait_ms` backpressure
  counters) via the mock's `/probe`, plus one agent's
  `__axon/broker/systemcheck` (`streams`/`busy`/`target`). Watch streams
  follow demand under load and settle back to the idle reserve after it,
  and watch `acquire_wait_ms` for whether callers ever waited on a
  stream — that, not throughput, is the signal the tunnel is the
  bottleneck rather than the upstream.
- `chaos.log` — timestamped kill/restart events.
- The script's final phase validates steady-state recovery: after chaos
  stops and a 90s quiesce, every token must be routable again.

## What it exercises that unit/e2e tests don't

- Multi-instance topology: round-robin stream spread across servers,
  multiple agents per token, routing via the notification protocol
  under churn.
- Sustained concurrency with multi-MiB bodies both directions
  (CallData chunking, streaming, backpressure) for minutes at a time.
- SIGKILL recovery paths on both sides while traffic is in flight.

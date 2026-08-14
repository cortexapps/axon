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

# gRPC Tunnel E2E Test

## Status: PASSING (both proxy and no-proxy modes)

## Fixes Applied to Get Tests Passing

### 1. `python:3.8-alpine` → `python:3.13-alpine`
Python 3.8 is EOL and the Docker image was removed from Docker Hub.
- **File**: `agent/test/relay/docker-compose.grpc.yml`

### 2. Missing `CORTEX_TENANT_ID` env var
The server requires `tenant_id` in ClientHello but the docker-compose didn't set `CORTEX_TENANT_ID` for the axon-relay container.
- **File**: `agent/test/relay/docker-compose.grpc.yml` — added `CORTEX_TENANT_ID: test-tenant`

### 3. Separated gRPC TLS from HTTP TLS config
`DISABLE_TLS` controlled both gRPC transport credentials and HTTP client TLS verification. When running with proxy (`CA_CERT_PATH` set), `http_client.go` panicked: "Cannot use custom CA cert with TLS verification disabled". Added a new `GRPC_INSECURE` config field specifically for gRPC tunnel connections.
- **Files**:
  - `agent/config/config.go` — added `GrpcInsecure bool` field, read from `GRPC_INSECURE` env var
  - `agent/server/grpctunnel/tunnel_client.go` — uses `GrpcInsecure` instead of `HttpDisableTLS`
  - `agent/test/relay/docker-compose.grpc.yml` — uses `GRPC_INSECURE: "true"` instead of `DISABLE_TLS: "true"`

### 4. Removed snyk-broker-specific header check
The test checked for `x-axon-relay-instance` header which is injected by the snyk-broker reflector, not the gRPC tunnel path.
- **File**: `agent/test/relay/relay_test.grpc.sh`

### 5. macOS compatibility fix
`stat -c%s` doesn't work on macOS (BSD stat). Changed to `wc -c <` which is portable.
- **File**: `agent/test/relay/relay_test.grpc.sh`

## Running the Tests

```bash
# No-proxy mode
cd agent/test/relay && PROXY=0 ./relay_test.grpc.sh

# With proxy mode
cd agent/test/relay && PROXY=1 ./relay_test.grpc.sh

# Both (via Makefile)
cd agent && make grpc-relay-test
```

## Test Architecture

```
                    Host
                     |
                     v
        grpc-tunnel-server (HTTP :8080, gRPC :50052)
                     |
          gRPC bidirectional stream
                     |
                     v
                axon-relay (RELAY_MODE=grpc-tunnel)
                     |
              HTTP request execution
                     |
                     v
              python-server (:80, serves /tmp)
              or GitHub (HTTPS)
              or cortex-fake (:8081, echo endpoint)
```

## Test Cases
1. Text file relay (write to /tmp, fetch via tunnel)
2. Binary file relay (1MB, SHA-256 checksum verification)
3. HTTPS relay (GitHub README fetch)
4. Proxy header injection (PROXY=1 only) — verifies `x-proxy-mitmproxy`
5. Accept file header injection (PROXY=1 only) — verifies `added-fake-server`
6. Plugin header injection (PROXY=1 only) — verifies `HOME=/root`
7. gRPC tunnel stream establishment (PROXY=1 only) — log check

## Remaining Phase 2 Tasks
- None — Phase 2 is complete (code + e2e tests passing)

## Phase 3 & 4 (Not Started)
- Phase 3: JWT auth, graceful shutdown hardening, health endpoints
- Phase 4: Migration, cleanup
- Plan doc: `.claude/wip/grpc-plan.md`


gRPC Tunnel to Replace snyk-broker
Context

Axon currently uses a forked snyk-broker (Node.js) to tunnel HTTP traffic between the Cortex backend and customer-network agents via WebSocket (Primus). This has been fragile — the Node.js WebSocket stack has many failure modes, adds build complexity (Node.js in Docker image), and includes much unused snyk-broker code. We're replacing it with a native Go gRPC bidirectional streaming tunnel that is more reliable, simpler, and eliminates the Node.js dependency.

Key architectural change: the RegistrationReflector is eliminated. All accept file rule matching, header injection, auth handling, variable resolution, plugin execution, and _POOL load balancing are handled by a standalone RequestExecutor component that takes an AcceptFileRule and applies it to a given HTTP request. This is a single-concern component that works identically regardless of relay mode.
Phase 1: Proto Definitions + Server Skeleton with BROKER_SERVER Compat
Proto definitions

Create /src/axon/proto/tunnel/tunnel.proto:

service TunnelService {
  rpc Tunnel(stream TunnelClientMessage) returns (stream TunnelServerMessage);
}

// Client → Server envelope
message TunnelClientMessage {
  oneof message {
    ClientHello hello = 1;
    Heartbeat heartbeat = 2;
    HttpResponse http_response = 3;
  }
}

// Server → Client envelope
message TunnelServerMessage {
  oneof message {
    ServerHello hello = 1;
    Heartbeat heartbeat = 2;
    HttpRequest http_request = 3;
  }
}

message ClientHello {
  string broker_token = 1;    // Cortex-API-issued token, used for BROKER_SERVER dispatch routing
  string client_version = 2;
  string tenant_id = 3;       // from Cortex API registration
  string integration = 4;     // e.g. "github", "jira"
  string alias = 5;           // integration alias
  string instance_id = 6;     // unique agent instance ID (from config.InstanceId)
  string cortex_api_token = 7; // for server-side validation (optional, JWT)
  map<string,string> metadata = 8;
}
message ServerHello {
  string server_id = 1;       // server hostname (or UUID fallback) — client uses for metrics tagging and dedup
  int32 heartbeat_interval_ms = 2;
  string stream_id = 3;       // server-generated UUID for this specific stream, used in BROKER_SERVER notifications
}
message Heartbeat { int64 timestamp_ms = 1; }
message HttpRequest { string request_id = 1; string method = 2; string path = 3; map<string,string> headers = 4; bytes body = 5; int32 chunk_index = 6; bool is_final = 7; }
message HttpResponse { string request_id = 1; int32 status_code = 2; map<string,string> headers = 3; bytes body = 4; int32 chunk_index = 5; bool is_final = 6; }

Server module (/src/axon/server/)

New Go module with its own go.mod. Dependencies: grpc, protobuf, zap, fx, uber/tally + tally/prometheus, golang-jwt/jwt/v5.

Files to create:

    server/go.mod, server/go.sum
    server/Makefile — proto generation, build, test targets
    server/cmd/main.go — fx app bootstrap, gRPC server + HTTP server startup
    server/config/config.go — config struct: GrpcPort, HttpPort, BrokerServerURL, JWTPublicKeyPath, HeartbeatIntervalMs, env-var driven
    server/tunnel/service.go — implements TunnelServiceServer. On stream open: read ClientHello, optionally validate cortex_api_token (JWT), check broker_token for collision, store {tenant_id, integration, alias, instance_id} mapping, call POST /broker-server/client-connected with token + SHA-256 hash, send ServerHello. On stream close: call client-deleted. Send heartbeats on interval. Close stream if client misses 2 heartbeats.
    server/tunnel/client_registry.go — thread-safe sync.RWMutex map of hashedToken → clientEntry{tenantId, integration, alias, instanceId, streams[]}. Supports multiple tunnels per token. Provides GetIdentity(hashedToken) for metrics tagging. Tracks instance_id per stream to distinguish multiple agent instances for the same integration.
    server/broker/broker_server_client.go — HTTP client wrapping BROKER_SERVER API: ServerConnected(), ServerDeleted(), ClientConnected(token, hashedToken, metadata), ClientDeleted(token, hashedToken)
    server/metrics/metrics.go — uber/tally scope + prometheus reporter. All metrics tagged with {server_id, tenant_id, integration, alias} where applicable. Metrics:
        tunnel.connections.active (gauge) — currently open tunnel streams
        tunnel.connections.total (counter) — total tunnel connections over lifetime
        tunnel.heartbeat.sent / tunnel.heartbeat.received (counters)
        tunnel.heartbeat.missed (counter) — heartbeats expected but not received
        tunnel.dispatch.count (counter, by method, status_code) — HTTP requests dispatched through tunnel
        tunnel.dispatch.duration_ms (histogram) — end-to-end dispatch latency
        tunnel.dispatch.inflight (gauge) — currently pending requests
        tunnel.dispatch.errors (counter, by error_type) — dispatch failures (timeout, no_tunnel, stream_error)
        tunnel.dispatch.bytes_sent / tunnel.dispatch.bytes_received (counter) — traffic volume
        tunnel.auth.failures (counter) — JWT validation failures
        tunnel.stream.duration_seconds (histogram) — how long tunnel streams stay alive

Token and identity flow

The Cortex API issues the broker token (as it does today). The client passes the token plus identity metadata to the gRPC server in ClientHello. The server stores the mapping for metrics tagging and dispatch routing.

Flow:

    Client calls Cortex API POST /api/v1/relay/register → receives server_uri, token, plus identity info (tenant_id, integration, alias)
    Client opens gRPC stream, sends ClientHello with broker_token + tenant_id + integration + alias + instance_id
    Server validates (optional JWT check on cortex_api_token), checks for token collision, stores:

    broker_token → { tenant_id, integration, alias, instance_id, stream_handles[] }

    Server calls POST /broker-server/client-connected with the token + SHA-256 hash
    Server returns ServerHello with server_id and heartbeat_interval_ms

This means:

    All server metrics tagged with {tenant_id, integration, alias} — zero external lookups
    The Cortex API controls token issuance (existing behavior preserved)
    Server just registers and maps the client-provided token to its identity
    client_registry.go maps hashedToken → clientEntry{tenantId, integration, alias, instanceId, streams[]}
    On token collision (different client claiming same token), server rejects with error

Files to modify:

    /src/axon/Makefile — add server targets

Verification

    Unit tests for client_registry.go (add/remove/lookup)
    Unit tests for broker_server_client.go using httptest.Server
    Integration test: start server, connect test gRPC client with ClientHello, verify mock BROKER_SERVER receives client-connected, disconnect, verify client-deleted

Phase 2: RequestExecutor + Server HTTP Dispatch + Client Tunnel
RequestExecutor — standalone accept file rule engine

A new standalone component at agent/server/requestexecutor/ that is the single point of responsibility for applying accept file rules to HTTP requests. This replaces all the logic currently split across the reflector, snyk-broker, and accept file rendering. It has no knowledge of gRPC, WebSocket, or tunnel mechanics.

Responsibilities:

    Rule matching: given an HTTP method + path, find the first matching accept file rule (method match or "any", path glob/wildcard match)
    URL rewriting: rewrite the request URL using the matched rule's origin
    Auth injection: apply the rule's auth scheme (bearer, basic, custom header)
    Header injection: resolve and inject custom headers from the rule, including:
        Environment variable expansion: ${VAR}, ${VAR:default}
        Plugin execution: ${plugin:name} → runs executable, captures output
    _POOL load balancing: when an origin or variable uses _POOL suffix (e.g., GITHUB_API_POOL=https://api1.github.com,https://api2.github.com), parse comma-separated values and round-robin across them per-request
    TLS handling: support HTTPS origins with configurable CA cert and HttpDisableTLS
    Request execution: execute the rewritten request via http.Client and return the response

Interface:

// RequestExecutor applies accept file rules to execute HTTP requests
type RequestExecutor interface {
    // Execute matches the request against accept file rules, rewrites it,
    // and executes it against the target origin. Returns the response.
    Execute(ctx context.Context, method, path string, headers map[string]string, body []byte) (*ExecutorResponse, error)
}

type ExecutorResponse struct {
    StatusCode int
    Headers    map[string]string
    Body       []byte  // or io.ReadCloser for streaming
}

Files to create:

    agent/server/requestexecutor/executor.go — RequestExecutor interface and implementation
    agent/server/requestexecutor/rule_matcher.go — accept file rule matching (method + path glob)
    agent/server/requestexecutor/pool.go — _POOL variable parsing and round-robin selection
    agent/server/requestexecutor/executor_test.go — comprehensive tests

Reuses existing code:

    agent/server/snykbroker/acceptfile/accept_file.go — accept file parsing, rendering, rule structures
    agent/server/snykbroker/acceptfile/resolver.go — variable resolution (env vars, defaults). The varIsSet() function already checks _POOL suffix (line 277) but doesn't implement rotation — we add that.
    agent/server/snykbroker/acceptfile/plugin.go — plugin execution for ${plugin:name} headers

_POOL implementation details:

    Currently resolver.go:277 only checks os.Getenv(varName+"_POOL") != "" for variable presence
    New pool.go adds: parse comma-separated pool values, maintain atomic round-robin counter per pool variable, return next value on each resolution
    Pool resolution is transparent to the rest of the system — the resolver returns a single value each time

Server-side HTTP dispatch

Files to create:

    server/dispatch/handler.go — HTTP handler on /broker/:token/*path. Extracts token, SHA-256 hashes, looks up client stream(s), generates UUID request_id, sends HttpRequest down stream, waits for HttpResponse (with timeout), writes HTTP response back. Round-robins across multiple tunnels for same token.
    server/dispatch/pending_requests.go — map of request_id → chan HttpResponse with timeout cleanup

Files to modify:

    server/tunnel/service.go — on receiving HttpResponse from client stream, deliver to pending request channel
    server/cmd/main.go — mount dispatch HTTP handler

Client-side gRPC tunnel

Extract RelayInstanceManager interface to a shared location:

Files to create:

    agent/server/relay/interfaces.go — extracted RelayInstanceManager interface
    agent/server/grpctunnel/tunnel_client.go — implements RelayInstanceManager:
        Start(): call registration.Register() for server URI + token + identity (tenant_id, integration, alias), render accept file, create RequestExecutor from rendered rules, open gRPC connection, open N tunnel streams (default 2) each sending ClientHello with broker_token + identity, receive ServerHello, start heartbeat + request handler goroutines
        Request handler: receive HttpRequest, delegate to RequestExecutor.Execute(), send HttpResponse back (chunked if large)
        Reconnection: on stream error, exponential backoff (1s → 30s max)
        Restart(): close all streams, re-register, re-establish
        Close(): cancel contexts, wait for goroutines
    agent/server/grpctunnel/module.go — fx module mirroring snykbroker/module.go

Files to modify:

    agent/server/snykbroker/relay_instance_manager.go — import interface from server/relay/interfaces.go
    agent/config/config.go — add fields: RelayMode (enum: snyk-broker | grpc-tunnel, default snyk-broker), TunnelCount (int, default 2), env vars RELAY_MODE, TUNNEL_COUNT. BROKER_SERVER_URL is reused as the gRPC server address when RELAY_MODE=grpc-tunnel (same env var, different meaning per mode).
    agent/cmd/stack.go or agent/cmd/serve.go — conditional module selection: when RelayMode == "grpc-tunnel" use grpctunnel.Module, else snykbroker.Module
    agent/server/snykbroker/module.go — update for interface extraction

Chunked streaming for large bodies

Bodies are split into chunks (max 1MB each) sent as a sequence of messages sharing the same request_id. The first chunk carries status_code + headers (for responses) or method + path + headers (for requests). Subsequent chunks carry only body, chunk_index, and is_final. The receiving side reassembles by buffering chunks in order until is_final=true. For small payloads (≤1MB), a single message with chunk_index=0, is_final=true is sent — no overhead.

Implementation:

    server/dispatch/pending_requests.go — accumulates chunks per request_id, resolves the pending request channel only when is_final=true
    RequestExecutor returns response body; tunnel_client.go chunks it into 1MB pieces for sending
    server/dispatch/handler.go — reassembles chunks and writes the full HTTP response back to the BROKER_SERVER caller

Key design notes

    No reflector needed. The RequestExecutor handles all accept file logic natively: rule matching, header injection, auth, variable resolution, plugins, and _POOL rotation. The reflector is eliminated from the gRPC tunnel path entirely.
    The RequestExecutor is a standalone component with a single concern — it knows nothing about tunnels, gRPC, or WebSocket. It takes a request and produces a response using accept file rules.
    Multi-tunnel with server dedup: Client opens N tunnel streams (default 2). After receiving ServerHello, the client checks server_id — if it's already connected to that server, it closes the duplicate stream and retries (with backoff + jitter to land on a different LB target). This ensures tunnels are spread across distinct server instances for real fault isolation. Client tags all metrics with server_id so we can track which servers each agent is connected to.
    Server server_id: Set from HOSTNAME env var (standard in k8s pods). Falls back to a generated UUID if HOSTNAME is unset or "localhost". Returned in every ServerHello.

Verification

    Unit test RequestExecutor: given accept file rules + HTTP request, verify:
        Correct rule matching (method + path)
        Origin URL rewriting
        Bearer/basic auth injection
        Custom header resolution (env vars, plugins)
        _POOL round-robin rotation
        TLS configuration
        Rejection when no rule matches
    Unit test pending_requests.go: request/response correlation, chunking, and timeout
    Integration test: server + mock BROKER_SERVER + client with test accept file → end-to-end HTTP dispatch through tunnel
    Flag switching test: RELAY_MODE=snyk-broker starts old path, RELAY_MODE=grpc-tunnel starts new path

Phase 3: Auth, Hardening, Dockerfiles
JWT Authentication

    server/auth/jwt.go — gRPC stream interceptor validating JWT bearer token from ClientHello. Loads public key from config file path. Disabled when no key configured.

Graceful shutdown

    Server: SIGTERM → POST /broker-server/server-deleted, drain active tunnels (30s), stop gRPC
    Client: Close() → cancel stream contexts, wait for goroutines, clean up temp files

Health endpoints

    Server: /healthz returning 200 when running
    Server: /broker/:token/systemcheck — tunnel health check to client and return result

Dockerfiles

    server/docker/Dockerfile — multi-stage Go build, no Node.js
    docker/Dockerfile.grpc — lighter agent image without Node.js/snyk-broker

Client-side observability

    Client metrics (prometheus/client_golang): grpc_tunnel_connections_active (gauge), grpc_tunnel_requests_total (counter), grpc_tunnel_reconnects_total (counter), grpc_tunnel_request_duration_ms (histogram)
    All tagged with server_id (from ServerHello) + tenant_id + integration + alias
    Enables per-server-instance visibility from the client side

Verification

    JWT unit tests: valid/invalid/expired tokens
    Graceful shutdown test: SIGTERM → verify callbacks fire, no goroutine leaks
    Adapt existing test/relay/relay_test.sh for RELAY_MODE=grpc-tunnel

Phase 4: Migration and Cleanup

    Change RelayMode default from snyk-broker to grpc-tunnel
    Add deprecation warning for snyk-broker mode
    Remove Node.js + snyk-broker from main docker/Dockerfile
    Remove RegistrationReflector and related config (HttpRelayReflectorMode, ReflectorWebSocketUpgrade)
    Update README.relay.md
    Keep snykbroker package for rollback, mark deprecated

Verification

    Full regression suite with new default
    Docker image size reduction (~200MB+ savings)
    Backward compat: RELAY_MODE=snyk-broker still works

Failure Modes and Mitigations
1. Spoofing / unauthorized connections

Risk is low. BROKER_SERVER only dispatches to tokens issued by the Cortex API. A rogue client registering with a random token creates a dead-end entry — Cortex will never dispatch to it. Guessing a valid UUID (122 bits entropy) is impractical. The worst case is DoS via mass registration of junk entries.

Mitigations:

    Rate-limit new tunnel connections per source IP (prevents registry flooding)
    JWT validation (when enabled) confirms client identity — useful for metrics accuracy, not critical for security
    TLS on gRPC listener prevents token interception in transit (configurable, default on in production)

2. Multiple connections and reconnection races

Problem: Client reconnects before server detects old stream is dead → two registry entries for same token. Or client-deleted for old stream arrives at BROKER_SERVER after client-connected for new stream → BROKER_SERVER drops the client.

Mitigations:

    Each stream gets a server-generated stream_id (UUID). The client-connected and client-deleted calls to BROKER_SERVER include this stream_id so they can be correlated — a client-deleted for stream A doesn't affect stream B.
    On ClientHello with a token that already exists in the registry from the same (tenant_id, instance_id): this is a reconnect. Add the new stream handle to the existing entry. The old dead stream will be cleaned up by heartbeat timeout.
    On ClientHello with a token claimed by a different (tenant_id, instance_id): reject with ALREADY_EXISTS error.
    Registry operations for the same token are serialized (per-token mutex or channel) to prevent connect/disconnect races.

3. BROKER_SERVER notification durability

Problem: client-connected POST fails → BROKER_SERVER can't route to client. client-deleted POST fails → BROKER_SERVER routes to dead tunnel. Server crashes → no cleanup notifications sent.

Mitigations:

    Dispatch is ready immediately. On ClientHello, the server registers the stream in client_registry and sends ServerHello right away — the tunnel is live and can dispatch requests. The client-connected POST to BROKER_SERVER happens asynchronously. The registry entry tracks a brokerServerRegistered flag (starts false, set true on success). This means traffic can flow even if BROKER_SERVER is temporarily down.
    client-connected retries indefinitely with backoff in a background goroutine. Backoff starts at 1s, caps at 30s. Continues until success or stream close. The entry stays in registry and dispatches traffic regardless of BROKER_SERVER notification status.
    client-deleted retries with backoff (3 attempts). If it still fails, log an error. The TTL mechanism (below) handles cleanup.
    Periodic re-registration: Server sends POST /broker-server/client-connected for all active connections every N minutes (e.g., 5 min). This acts as a TTL refresh — if BROKER_SERVER has a stale entry, it gets corrected. If the server crashed and restarted, it re-registers its clients.
    Server lifecycle: server-connected on startup, server-deleted on graceful shutdown. On crash, BROKER_SERVER should have a TTL for server entries (server-side concern, not ours to implement, but we should document the expectation).
    Idempotency: All BROKER_SERVER calls should be idempotent. Duplicate client-connected calls with same token are no-ops. This is critical for the periodic re-registration pattern.

4. Heartbeat and stale connection detection

Problem: Heartbeats prove stream liveness, not client health. A deadlocked client process keeps the TCP connection alive but never processes requests. Large chunked responses block the ordered gRPC stream, preventing heartbeats from flowing.

Mitigations:

    Two-layer keepalive:
        gRPC keepalive (transport level): keepalive.ServerParameters{Time: 30s, Timeout: 10s} + keepalive.EnforcementPolicy{MinTime: 15s}. Detects dead TCP connections (half-open sockets, network partitions).
        Application heartbeat (tunnel level): Server sends Heartbeat every heartbeat_interval_ms (default 30s). Client must respond within 2 * heartbeat_interval_ms. Detects hung client processes.
    Heartbeat vs. chunked response ordering: Since gRPC streams are ordered, a large response in progress blocks heartbeats. Two options:
        Option A (recommended): Interleave heartbeat responses between chunks. The chunking sender checks if a heartbeat is pending and sends the heartbeat response before the next chunk. This adds minimal complexity to the chunk sender.
        Option B: Use the gRPC keepalive as the liveness signal during long transfers. If the TCP connection is alive, the transfer is making progress. App heartbeats are skipped while an inflight request is active on that stream.
    Dispatch health check: The server can periodically send a lightweight HttpRequest with a special path (e.g., /__tunnel_health) that the client responds to immediately. This validates the full request/response path, not just stream liveness. Run every 5 minutes.
    Heartbeat latency tracking: Server tracks round-trip heartbeat latency per stream as a metric. Sudden latency spikes indicate degraded connections.

5. Server rolling deploys and thundering herd

Problem: Server instance goes down → all its tunnels die → all clients reconnect simultaneously to other instances → overload.

Mitigations:

    Graceful drain: On SIGTERM, server sends a GoAway-style message to clients (could be a special ServerHello with a reconnect hint) and stops accepting new connections. Existing requests complete (30s drain). Clients begin reconnecting with jitter before the server fully shuts down.
    Client reconnect jitter: On disconnect, client waits random(0, 5s) before first reconnect attempt. Combined with exponential backoff, this spreads reconnections over time.
    Connection rate limiting on server: Server limits new tunnel connections to N per second (configurable) to prevent overload during mass reconnection.

6. Partial chunk delivery

Problem: Client sends chunks 0 and 1 of a response, then stream dies before chunk 2 (is_final=true). Server holds incomplete response data, the dispatch caller blocks forever.

Mitigations:

    Dispatch timeout: Every pending request has a deadline (configurable, default 60s). If is_final is not received by deadline, the partial response is discarded and the caller gets a 504 Gateway Timeout.
    Chunk cleanup on stream close: When a stream closes, all pending requests that were dispatched on that stream are immediately failed with 502 Bad Gateway. The pending_requests.go map tracks which stream each request was dispatched on.
    Retry on other stream: If the token has multiple active streams and one fails mid-response, the server can retry the request on another stream (if the request is idempotent — determined by method: GET/HEAD are safe to retry, POST/PUT/DELETE are not).

7. Token collision vs. legitimate reconnect

Problem: Same token arrives in a new ClientHello. Is it a reconnect (same agent, new stream) or a hijack (different agent, stolen token)?

Decision logic:

    Same (tenant_id, instance_id) as existing entry → reconnect. Add stream, keep existing identity.
    Same tenant_id, different instance_id → new instance for same integration. This is valid (e.g., scaling up agents). Add stream to same token entry, track both instance_ids.
    Different tenant_id → collision/hijack. Reject with error. Log security event.

8. BROKER_SERVER unavailability

Problem: BROKER_SERVER is down or unreachable.

Mitigations:

    Traffic flows regardless. Tunnel streams accept and dispatch requests immediately, independent of BROKER_SERVER notification status. The client-connected and server-connected calls retry indefinitely in background goroutines.
    Health endpoint reports status. /healthz returns 200 but includes broker_server_connected: true/false in the response body for visibility.
    Periodic re-registration (every 5 min) ensures BROKER_SERVER eventually learns about all active connections once it recovers.

Critical Files Reference
File 	Role
relay_instance_manager.go 	Current RelayInstanceManager interface + snyk-broker impl; interface to extract
module.go 	DI module pattern to replicate for grpctunnel
stack.go 	fx stack wiring; needs conditional module selection
config.go 	Agent config; needs RelayMode, TunnelServerAddress, TunnelCount
accept_file.go 	Accept file parsing/rendering — reuse for RequestExecutor
resolver.go 	Variable resolution + _POOL detection (line 277) — extend for pool rotation
plugin.go 	Plugin execution for ${plugin:name} — reuse in RequestExecutor
reflector.go 	HTTP reflector — eliminated in gRPC tunnel mode, logic moves to RequestExecutor
registration.go 	Registration interface — reused by grpctunnel
cortex-axon-agent.proto 	Existing proto — reference for style/conventions
Dockerfile 	Current Docker build — to be updated in Phase 4
Makefile 	Proto generation pattern to replicate for server

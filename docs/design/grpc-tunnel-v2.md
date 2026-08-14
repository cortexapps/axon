# gRPC Tunnel v2 — Design

Status: draft
Owner: shawn@shawnburke.com
Supersedes: PR #85 (`grpc-tunnel`) wire protocol only; keeps its structural choices
Scope: relay-agent transport used by the reflector-style HTTP-forwarding flows
Non-scope: handler dispatch (`AxonAgent.Dispatch`), Cortex-API proxy, webhook handlers

---

## 1. Goals

1. Replace the snyk-broker (Node/Primus WebSocket) subprocess with a native Go
   transport, so the agent has no runtime dependency on the Snyk-owned broker
   or its Node EOL schedule.
2. Keep customer-visible surface identical: same integrations, same accept
   files, same `/register` handshake, same connect/disconnect notifications to
   the cloud dispatcher.
3. Make the wire protocol between agent and cloud generic enough that a
   future gRPC (or other HTTP/2-shaped) upstream can be tunneled through it
   without a protocol change.
4. Ship as a **side-by-side mode**, not a refactor. Snyk-broker mode is the
   default; gRPC mode is opt-in via config. Enabling one does not touch the
   other's code paths.

## 2. Non-goals

- HTTP/3, WebSocket end-to-end passthrough, or raw TCP tunneling. If a
  customer needs those against a SaaS API we'll revisit — this design's escape
  hatches (§4.5) leave room but v1 does not implement them.
- PUSH_PROMISE, custom h2 SETTINGS, or any HTTP/2 wire-level fidelity beyond
  the semantic layer (headers / data / trailers / reset).
- Multi-tenant sharing on a single agent process. One agent still serves one
  `integration + alias` tuple.
- Deprecating snyk-broker mode. That is a separate decision made once gRPC
  mode has bake time in production.

## 3. Coexistence model

The two transports live behind a single interface — the existing
`snykbroker.RelayInstanceManager` — and are selected at fx-DI wire time by a
new config flag.

```
config.AgentConfig.RelayTransport ∈ { "snyk-broker" (default), "grpc-tunnel" }
                                     env: AXON_RELAY_TRANSPORT
```

Wiring rule (implemented in `agent/server/snykbroker/module.go` or a peer
module):

- `snyk-broker`: provide `snykbroker.NewRelayInstanceManager` (today's
  implementation) as `RelayInstanceManager`. **Reflector, ws_proxy, broker
  supervisor, restart consumer, accept-file rewriter — all unchanged.** No
  code in `agent/server/snykbroker/` outside `module.go` moves. No
  behavioural change for anyone not opting in.
- `grpc-tunnel`: provide `grpctunnel.NewTunnelClient` as
  `RelayInstanceManager`. Snyk-broker code is not linked out of the binary
  but is not invoked either — no subprocess, no reflector, no ws_proxy.

Everything else (registration, fx lifecycle, HTTP server, metrics registry,
integration-info resolution, accept-file parsing) is shared. The mode switch
happens strictly at the `RelayInstanceManager` binding.

**Explicit invariants for the coexistence period:**

1. No file under `agent/server/snykbroker/{reflector,ws_proxy,relay_instance_manager,supervisor,event_tracker,registration}.go`
   changes signature or behaviour as part of this work. Bug fixes are fine;
   restructuring is not.
2. New gRPC code lives under `agent/server/grpctunnel/…` and
   `server/…` (the cloud tunnel service). Nothing in
   `agent/server/snykbroker/` imports from those paths.
3. `RelayReflector` is not invoked in gRPC mode. The equivalent logic —
   accept-file rule matching, header injection, origin resolution, pool
   rotation — lives in the agent-side executor (§6).
4. Registration (`snykbroker.Registration`) is shared. The token returned by
   `/register` is used identically; only what the agent does with the returned
   `ServerUri` differs (WS dial vs gRPC dial).
5. Cortex-cloud dispatcher's `POST /internal/brokerservers/…` notification
   contract is preserved. Same URL, same body, same client library
   (`server/broker/broker_server_client.go`).

### 3.1 Rollout gates

- Phase 0: gRPC mode present, off by default, no cloud tunnel-server
  deployed. Unit tests + local docker-compose pass.
- Phase 1: cloud tunnel-server deployed to staging alongside snyk-broker.
  Internal test tenants set `AXON_RELAY_TRANSPORT=grpc-tunnel`, everyone else
  is unaware.
- Phase 2: opt-in for design-partner customers. Config still per-agent.
- Phase 3: default-flip evaluated once a Cortex-side telemetry bar is hit
  (parity on success rate, p95 latency, connection recovery time).
- Phase 4: snyk-broker removal — a separate design doc, out of scope here.

## 4. Wire protocol

The wire is a single bidirectional gRPC stream that carries call-oriented
frames. Semantics are HTTP/2's semantic layer (HEADERS / DATA / TRAILERS /
RESET) without HTTP/2 as the transport. This is what makes a future gRPC
upstream a same-wire change.

### 4.1 Proto

```proto
syntax = "proto3";
package cortex.axon.tunnel.v2;
option go_package = "github.com/cortexapps/axon-server/tunnelpb/v2";

service TunnelService {
  // One long-lived bidirectional stream per gRPC connection.
  // The agent may open multiple concurrent streams — see §5 (concurrency).
  rpc Tunnel(stream ClientFrame) returns (stream ServerFrame);
}

message ClientFrame {
  oneof msg {
    ClientHello hello     = 1;
    Heartbeat   heartbeat = 2;
    CallFrame   call      = 3;
  }
}

message ServerFrame {
  oneof msg {
    ServerHello hello     = 1;
    Heartbeat   heartbeat = 2;
    CallFrame   call      = 3;
  }
}

message ClientHello {
  string broker_token     = 1; // from /register, same as today
  string client_version   = 2;
  string tenant_id        = 3;
  string integration      = 4;
  string alias            = 5;
  string instance_id      = 6;
  string cortex_api_token = 7; // optional, for server-side JWT validation
  map<string,string> metadata = 8;
}

message ServerHello {
  string server_id            = 1; // for dedup / metrics tagging
  string stream_id            = 2; // per-stream UUID
  int32  heartbeat_interval_ms = 3;
  int32  max_frame_bytes       = 4; // agent caps CallData payload accordingly
  int32  max_streams           = 5; // per-token stream cap (agent clamps its pool)
}

message Heartbeat { int64 timestamp_ms = 1; }

// A CallFrame carries one event for one logical call.
// The direction of the enclosing envelope tells you who sent it.
message CallFrame {
  string call_id = 1;
  oneof body {
    CallStart  start  = 10;
    CallData   data   = 11;
    CallEnd    end    = 12;
    CallCancel cancel = 13;
  }
}

message CallStart {
  // HTTP/2-shaped pseudo-headers. Required: :method, :path.
  // Optional: :authority, :scheme.
  map<string,string> pseudo_headers = 1;
  // Regular headers, lowercased keys.
  map<string,string> headers        = 2;
  int32  timeout_ms = 3;

  // Opaque routing hint the server-side adapter fills in and the agent-side
  // dispatcher may consume. For the HTTP adapter this is empty (agent matches
  // by :path against the accept file). For a future gRPC adapter it may be an
  // accept-file rule id or a target selector. Never interpreted by the
  // tunnel service.
  bytes routing_hint = 4;

  // Optional hint about the response shape. Lets the agent pick between an
  // HTTP backend and (future) a gRPC backend without content-type sniffing.
  // Absence means UNARY.
  Kind kind = 5;
  enum Kind {
    UNARY         = 0;
    SERVER_STREAM = 1;
    CLIENT_STREAM = 2;
    BIDI          = 3;
  }
}

message CallData {
  // Opaque payload bytes. Ordered per call_id on a given stream.
  bytes payload = 1;
}

message CallEnd {
  // HTTP trailers or gRPC status trailers (grpc-status, grpc-message,
  // grpc-status-details-bin). Empty for a plain HTTP/1 response with no
  // trailers.
  map<string,string> trailers = 1;
}

message CallCancel {
  string reason = 1;
  int32  code   = 2; // adapter-defined; 0 if not applicable
}
```

### 4.2 Framing rules

- `Start` is the first frame of a call, sent by whichever side initiates. In
  v1 only the server initiates; the wire allows both directions so a future
  agent-initiated call type (health check, config push) does not need a
  proto change.
- Zero or more `Data` frames follow, in order.
- Exactly one terminal frame per call per direction: `End` (normal) or
  `Cancel` (aborted).
- Sender MUST NOT reuse `call_id` on the same stream after it has sent a
  terminal frame; receiver treats reuse as protocol error, cancels the
  stream, agent reconnects.
- Server sends `Start`/`Data`/`End` for the request; agent sends
  `Start`/`Data`/`End` for the response. Both use the same `call_id`.
- Either side may send `Cancel` at any time. On receive, the other side
  stops producing frames for that `call_id` and MAY send its own `Cancel`
  back for symmetry (not required).
- `CallData.payload` size ≤ `ServerHello.max_frame_bytes` (server-chosen,
  default 1 MiB). This exists so a gRPC message-size cap doesn't need
  re-tuning per deployment.

### 4.3 What's NOT on the wire

Deliberate omissions, each of which we've considered and rejected for v1:

- `chunk_index`, `is_final` — the stream is ordered; `End` signals end.
  Callers do not need to reassemble.
- Per-request `body` size caps — replaced by streaming (§6.2). The server
  may still impose per-tenant byte-rate quotas but that's policy, not
  protocol.
- `is_failed_dispatch` — replaced by `CallCancel` with a `code` the HTTP
  adapter maps to 502.
- Any HTTP-message-specific fields (`method`, `path` at top level) — those
  live in `pseudo_headers`, where a gRPC front-end can populate them the
  same way (`:method=POST`, `:path=/pkg.Svc/Method`).

### 4.4 Versioning

Package is `cortex.axon.tunnel.v2`. The `v1` proto from PR #85 is retired at
merge time — nothing outside the PR ever depended on it. Future v3 (if
ever) coexists by exposing a new gRPC service under the same
`TunnelService` name on a different package.

### 4.5 Reserved for future use, not implemented

Documenting these here so we don't paint ourselves into a corner:

- Server pushing an agent-initiated call would use `ClientFrame.call.start`.
- A raw-TCP escape hatch would add a `TcpTunnel(stream TcpFrame) returns
  (stream TcpFrame)` service alongside `TunnelService`, sharing the same
  `ClientHello`. Not proposed for v1; kept as an option.
- Per-call flow control (send credits) would add a `CallCredit` message.
  Only needed if we abandon slot-pooling (§5); v1 does not.

## 5. Concurrency: adaptive slot pool (client-side watermark)

The agent holds a pool of concurrent `Tunnel` RPCs, each its own
`grpc.ClientConn` (one TCP+TLS connection per slot), each carrying **at
most one in-flight call at a time**. The pool is **adaptive**: it idles at
`AXON_GRPC_TUNNEL_MIN_SLOTS` (default 4) and grows toward
`AXON_GRPC_TUNNEL_MAX_SLOTS` (default 32) under load, so a fleet of
mostly-idle agents costs the server a handful of sockets per agent rather
than 32.

**Why one connection per slot** (not h2 multiplexing over shared conns):

- gRPC's HTTP/2 window is per-RPC. One RPC per logical call means we get
  the underlying flow control for free — no app-layer credits.
- Each slot has its own TCP congestion window: a slow 100 MiB response on
  one slot can't wedge another slot's snappy unary call at the TCP layer.
- Failure-domain isolation: one dead TCP path kills one slot.
- LB spread: each connection can land on a different server instance.

Connection-sharing (slots as h2 streams over K shared conns) remains the
documented future lever if peak socket counts ever bite; the adaptive pool
makes idle cost a non-issue without it.

**Why the client controls scaling** (no server negotiation): a slot only
becomes busy because the server dispatched onto it, so the client observes
saturation at the same instant the server does — and acts one RTT sooner
than any advice frame could. With multiple tunnel servers behind an LB,
server-side advice couldn't steer placement of new connections anyway.
One brain, no controller fights.

Mechanics:

- **Grow:** on every call admission (and once per second while saturated,
  via a background re-check), the client keeps a free-capacity watermark —
  at least 25% of connected streams (minimum one) idle. When breached, the
  worker target rises by ~half the current pool, clamped to the effective
  max, at most one step per second.
- **Shrink:** a per-slot watchdog retires a slot that has carried no call
  for `AXON_GRPC_TUNNEL_SLOT_IDLE_TIMEOUT` (default 10m) while the pool is
  above min. Jittered so a burst's worth of slots doesn't retire at once.
- **Server cap (defensive only):** the server enforces
  `MAX_STREAMS_PER_TOKEN` (default 64) per broker token, announces it in
  `ServerHello.max_streams`, and rejects excess streams with
  `ResourceExhausted`; the client clamps its effective max accordingly.
  This is protection against a buggy agent, not a control channel.

The wire supports multiplex-per-stream if we later need it (the `call_id`
is present) — but then per-call flow control becomes our problem. Not
crossing that bridge until we see the ceiling in practice.

Slot lifecycle mirrors PR #85's `manageStream` loop: connect → handshake
→ `ServerHello` → run calls as they arrive → retire on shrink or
reconnect on error with jittered backoff. Auth failure triggers
re-register through the shared `snykbroker.Registration`.

## 6. Server side

### 6.1 Layers

```
Cortex backend ─ HTTP/1.1 ─▶ HttpAdapter ─┐
Cortex backend ─ gRPC ────▶ GrpcAdapter ─┤   (v2+, not v1)
                                          ├─▶ Dispatcher ─▶ TunnelService ─▶ ClientRegistry
Cortex backend ─ any ─────▶ …Adapter ────┘
```

- `TunnelService` (`server/tunnel/service.go`) — owns bidi streams, hello
  handshake, heartbeats, registry lifecycle. Adapter-agnostic. Reuses
  ~80% of PR #85's code; changes are the proto rename and dropping the
  chunk-assembler.
- `ClientRegistry` (`server/tunnel/client_registry.go`) — unchanged from
  PR #85 in shape. `PickStream` (LastSuccessAt-preferring, round-robin
  fallback) still applies; a new `PickIdleStream` variant returns only
  streams with no in-flight call, needed by the slot-pool model.
- `Dispatcher` (`server/dispatch/…`) — new interface, per §6.2. Adapters
  call it. It does not know or care what protocol the caller spoke.
- `BrokerServerClient` (`server/broker/…`) — unchanged. Same
  connect/disconnect notifications to the same Cortex-cloud dispatcher URL.

### 6.1.1 Identity & trust

Possession of the broker token is the sole credential on the tunnel. The
token's meaning — which (tenant, integration, alias) it belongs to — is
fixed **inside Cortex** when the authenticated registration flow mints it,
and the Cortex dispatcher addresses tunnel servers by token only. The
identity fields in `ClientHello` (tenant_id, integration, alias,
instance_id) are client-supplied and informational: they feed logs,
metrics tags, and the BROKER_SERVER notify payload, and MUST NOT feed
authorization, routing, or stream-acceptance decisions. Concretely: the
handshake requires only `broker_token`; the registry keys entries and
enforces the per-token stream cap by hashed token; a tenant mismatch
across streams of one token is logged as likely misconfiguration but
never rejects a stream.

### 6.2 Dispatcher interface

```go
// Dispatcher is transport-agnostic. Adapters call Dispatch and stream a
// request through; they receive back a response they can stream to their
// caller.
type Dispatcher interface {
    Dispatch(ctx context.Context, token broker.Token, req *Request) (*Response, error)
}

type Request struct {
    PseudoHeaders map[string]string
    Headers       map[string]string
    Body          io.Reader          // may be a live pipe; adapter closes on caller-side cancel
    Kind          tunnelpb.CallStart_Kind
    TimeoutMs     int32
    RoutingHint   []byte
}

type Response struct {
    PseudoHeaders map[string]string  // e.g. ":status": "200"
    Headers       map[string]string
    Body          io.ReadCloser      // live pipe; closed on End or Cancel
    TrailersC     <-chan map[string]string // fires once, then closed
    ErrC          <-chan error       // fires once on abort, then closed
}
```

- `Body` on `Request` is read by the dispatcher and emitted as `CallData`
  frames as bytes arrive; the dispatcher blocks on the reader.
- `Body` on `Response` is a pipe the dispatcher writes to as `CallData`
  frames come back. Adapter reads from it and streams to its caller. On
  `End`, dispatcher closes the pipe cleanly and pushes trailers to
  `TrailersC`. On `Cancel`, dispatcher pushes the error to `ErrC` and
  closes both channels.

This shape is what buys streaming (§Q2 of the design discussion): no
`io.ReadAll` anywhere on the hot path.

### 6.3 HttpAdapter (v1)

```go
// Sits at /broker/<token>/<path> to preserve BROKER_SERVER URL shape.
func (h *HttpAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    token, path := parseTokenAndPath(r.URL.Path)
    req := &Request{
        PseudoHeaders: map[string]string{
            ":method": r.Method,
            ":path":   path,
        },
        Headers:   flattenHeaders(r.Header),
        Body:      r.Body,
        Kind:      tunnelpb.CallStart_UNARY, // HTTP/1.1 requests are unary from our POV
        TimeoutMs: deadlineToMs(r.Context()),
    }
    resp, err := h.dispatcher.Dispatch(r.Context(), broker.NewToken(token), req)
    if err != nil { writeGatewayError(w, err); return }
    defer resp.Body.Close()

    for k, v := range resp.Headers { w.Header().Set(k, v) }
    status, _ := strconv.Atoi(resp.PseudoHeaders[":status"])
    w.WriteHeader(status)

    // Stream body; flush after each chunk so SSE / long responses work.
    flusher, _ := w.(http.Flusher)
    io.Copy(&flushWriter{w, flusher}, resp.Body)

    // Apply trailers if the client negotiated them (rare from a Cortex-cloud
    // caller against SaaS APIs; free to add later).
    select {
    case tr := <-resp.TrailersC:
        for k, v := range tr { w.Header().Set(http.TrailerPrefix+k, v) }
    case err := <-resp.ErrC:
        log.Warn("late failure after headers", err) // response already committed
    }
}
```

- Same URL shape as the snyk-broker dispatcher (`/broker/<token>/<path>`).
  BROKER_SERVER-compat unchanged.
- `connection-status` endpoint from PR #85 stays as-is; it's a registry
  lookup that doesn't touch the dispatcher.
- No 100 MiB `io.ReadAll` cap. Per-tenant byte-rate limit belongs in a
  middleware, not the dispatcher.

### 6.4 GrpcAdapter (v2+, not built in this doc's scope)

Sketch only, to prove the interface is right:

```go
// Registered on the same grpc.Server as a wildcard/unknown-service handler,
// or as a specific reflection-based passthrough service.
func (g *GrpcAdapter) HandleStream(srv any, stream grpc.ServerStream) error {
    md, _ := metadata.FromIncomingContext(stream.Context())
    fullMethod, _ := grpc.MethodFromServerStream(stream) // "/pkg.Svc/Method"

    req := &Request{
        PseudoHeaders: map[string]string{":method": "POST", ":path": fullMethod},
        Headers:       flattenMD(md),
        Body:          newGrpcFrameReader(stream), // reads length-prefixed messages
        Kind:          kindFromDescriptor(fullMethod),
        RoutingHint:   routingHintFromMD(md),
    }
    resp, err := g.dispatcher.Dispatch(stream.Context(), token, req)
    if err != nil { return statusFromError(err) }
    defer resp.Body.Close()

    // Stream length-prefixed messages back out.
    if err := copyGrpcFrames(stream, resp.Body); err != nil { return err }

    // Trailers become grpc.SetTrailer.
    tr := <-resp.TrailersC
    stream.SetTrailer(metadataFromMap(tr))
    return statusFromTrailers(tr)
}
```

Point: the dispatcher, tunnel service, registry, and wire don't change.
Adding gRPC callers is one file.

## 7. Agent side

### 7.1 Layers

```
Tunnel bidi stream
  ├─ Slot manager (N slots, §5)
  └─ per slot:
       CallFrame demuxer (trivial — one call at a time in v1)
        └─ Router  (accept-file rule match, origin resolution, header/auth inject)
             └─ Backend
                  ├─ HttpBackend  (net/http, v1)
                  └─ GrpcBackend  (grpc.ClientConn, v2+, not v1)
```

- Slot manager: exists in PR #85 as `manageStream` — keep the shape,
  simplify to "one call at a time per slot" plus the idle/busy signalling
  the server needs for `PickIdleStream`.
- Demuxer: degenerate in v1 (max one call per slot). Still worth having
  the type — if we ever move to per-stream multiplex the change is
  localized.
- Router: matches an incoming `CallStart` against
  `AcceptFile.PrivateRules()` and applies the rule's origin/header/auth
  resolution. It is a thin consumer of the shared `acceptfile` package
  and holds no accept-file semantics of its own — see §9 for the
  one-definition rule and the conformance suite that enforces it.
  (PR #85's `requestexecutor` is refactored to meet this bar.)
- Backend: new interface.

### 7.2 Backend interface

```go
type Backend interface {
    // Do executes a matched request against the resolved origin. It returns
    // a Response whose Body is an io.ReadCloser the caller streams over the
    // tunnel; trailers arrive via a channel to accommodate gRPC.
    Do(ctx context.Context, req *BackendRequest) (*BackendResponse, error)
}

type BackendRequest struct {
    Method    string
    URL       *url.URL         // full resolved URL
    Headers   http.Header
    Body      io.Reader        // may be a live pipe
    Kind      tunnelpb.CallStart_Kind
    Timeout   time.Duration
}

type BackendResponse struct {
    StatusCode int
    Headers    http.Header
    Body       io.ReadCloser
    TrailersC  <-chan http.Header
    ErrC       <-chan error
}
```

### 7.3 HttpBackend (v1)

Wraps the shared `*http.Client` (which already handles proxy, CA certs,
TLS config — nothing to change there).

- `httpClient.Do(req)` with `req.Body` as the piped reader.
- On response: return `resp.Body` as `Body`, drain `resp.Trailer` into
  `TrailersC` once the body reader returns EOF.
- Cancellation: `ctx` cancel propagates to `req.Cancel` naturally; on the
  tunnel side that's a `CallCancel` from the server.

This is a small delta from PR #85's `requestexecutor.Execute` — the
diff is "don't `io.ReadAll` the response body; return a reader
instead." Everything else (rule match, auth, header injection, pool
resolution) stays.

### 7.4 GrpcBackend (v2+, not built here)

Same shape as HttpBackend. `grpc.NewClient` to the resolved authority,
`stream.SendMsg`/`RecvMsg` for the message flow, `grpc.Header` /
`grpc.Trailer` metadata mapping.

## 8. What snyk-broker keeps

For clarity, here's the full list of things that DO NOT change when
gRPC mode is off. If any of these gets touched in the gRPC-tunnel PR,
that's a smell:

- `agent/server/snykbroker/reflector.go` — the URL-rewriting HTTP proxy
  that lets snyk-broker call back into the agent for allowlist/header
  injection.
- `agent/server/snykbroker/ws_proxy.go` — WebSocket-through-proxy
  splicing.
- `agent/server/snykbroker/relay_instance_manager.go` — supervisor
  spawn, restart consumer, generation dedup, TIME_WAIT wait,
  auto-register loop.
- `agent/server/snykbroker/supervisor.go` — subprocess management.
- `agent/server/snykbroker/event_tracker.go` — Primus WS tunnel death
  heuristic.
- `agent/server/snykbroker/registration.go` — `/register` call. **Both
  transports use this.**
- Accept file parser (`acceptfile/…`) — accept-file rendering, rule
  wrappers, resolver map. Used by both transports; changes must be
  additive.
- All the `accept.*.json` fixtures.
- The `snyk-broker` binary in the docker image. Still shipped, still
  runnable; the docker layer only stops invoking it when the mode flag
  is `grpc-tunnel`.

## 9. Accept-file semantics: one definition, no divergence

Hard requirement: **accept files mean exactly the same thing in both
modes, forever.** A feature added to accept files must light up in both
transports from a single change, and must be provably equivalent.

First, an honest statement of where matching happens today, because
"don't fork the rule matcher" needs precision:

- In snyk-broker mode, **rule matching is done by the Node broker
  itself**. The agent's Go code never matches a request against a rule;
  the reflector only resolves origins and injects headers on requests
  the broker has already accepted.
- In gRPC mode there is no Node process, so the agent must match rules
  in Go.

So there are inherently two *implementations* of matching (Node's and
Go's) for as long as both transports exist — that cannot be unified at
the code level. What we control is that there is exactly one
*definition*, and that the implementations are pinned to it:

1. **Single source of truth: the `acceptfile` package.** All accept-file
   parsing, rendering, rule wrapping, header/auth resolution, env-var
   and pool expansion live in `agent/server/snykbroker/acceptfile/`,
   which both transports already consume. The gRPC-mode Router (§7.1)
   is a thin consumer of `AcceptFile.PrivateRules()` — the same wrappers
   the snyk-broker path renders from. It contains **no accept-file
   semantics of its own**: no separate parse, no separate defaulting, no
   separate expansion rules. PR #85's `requestexecutor/rule_matcher.go`
   is refactored to meet this bar before merge: anything in it that
   interprets accept-file content (rather than comparing an incoming
   request to an already-interpreted rule) moves into `acceptfile`.
2. **New accept-file features land in `acceptfile` first.** The rule:
   if a change can be expressed in the shared package (a new field, a
   new resolver, a new rendering step), it must be. A feature PR that
   adds accept-file behaviour to only one transport's code is rejected
   by construction — CI runs the conformance suite (below) on both.
3. **Conformance suite pins the two implementations together.** A
   shared fixture set — accept file + request (method, path, headers)
   → expected outcome (matched rule / rejected, resolved origin,
   injected headers) — lives next to the `accept_files` fixtures. It
   runs two ways in CI:
   - directly against the Go matcher (fast, unit-level);
   - through the snyk-broker e2e harness (docker-compose, request in
     one end, observe what reaches the mock upstream).
   Any divergence — including pre-existing quirks of the Node broker we
   discover along the way — fails the build and forces a decision:
   match the broker's behaviour, or document the exception in the
   fixture with a reason. The suite is the contract; the Node broker's
   current behaviour is its initial content.

This also bounds the blast radius the coexistence rules (§3) care
about: the gRPC work may **add** to `acceptfile` (new accessors,
resolver types), but additions must be exercised by the snyk-broker
path's existing tests where they affect rendering, and must never
change the rendered output for existing accept files (golden-file
tests on `Render()` guard this).

## 10. Config surface

New environment variables (all optional; defaults preserve today's
behaviour):

| Variable                          | Default        | Meaning                                                 |
|-----------------------------------|----------------|---------------------------------------------------------|
| `AXON_RELAY_TRANSPORT`            | `snyk-broker`  | `snyk-broker` (today) or `grpc-tunnel`                  |
| `AXON_GRPC_TUNNEL_SLOTS`          | `32`           | Concurrent bidi streams per agent                       |
| `AXON_GRPC_TUNNEL_MAX_FRAME_BYTES`| `1048576`      | Max `CallData.payload` size (also grpc max msg size)    |
| `AXON_GRPC_TUNNEL_HEARTBEAT_MS`   | `20000`        | Heartbeat interval; timeout = 2×                        |
| `AXON_GRPC_TUNNEL_INSECURE`       | `false`        | Skip TLS on tunnel dial (dev/e2e only)                  |
| `AXON_GRPC_TUNNEL_MAX_REQUEST_MS` | `300000`       | Hard cap on any one call; belt-and-braces vs server bug |

`BROKER_SERVER_URL` and `BROKER_TOKEN` are consumed identically by both
modes when set; if unset, both fall through to `/register`. The
`ServerUri` returned by `/register` is a `grpc(s)://…` URL for gRPC
tenants (server-side config on Cortex) and a `wss://…` URL for
snyk-broker tenants. Tenants are pinned to one transport server-side;
agents choose which server they can talk to via config.

## 11. Compatibility guarantees

- Accept file syntax: unchanged.
- Registration handshake: unchanged.
- BROKER_SERVER dispatcher notifications: unchanged.
- BROKER_SERVER dispatch URL shape (`/broker/<token>/<path>`): unchanged.
  Callers can't tell which transport carried their request.
- Metrics: new gRPC metrics use the `grpc_tunnel_*` prefix from PR #85.
  Existing `broker_operations` counter is emitted in snyk-broker mode
  only.
- Logs: gRPC mode uses a `grpc-tunnel` logger name; snyk-broker's
  logger names are untouched.

## 12. Testing

The project's testing surface is strong; this design leans on it rather
than inventing new frameworks.

### 12.1 Unit
- `requestexecutor/*_test.go`: extended for streaming bodies (pipe in,
  pipe out) and trailers. Existing tests keep working since the shape
  is a superset.
- `grpctunnel/tunnel_client_test.go` (PR #85): rebase onto the new
  proto. Frame assembler goes away, request/response shape tests
  change to Start/Data/End sequences.
- New: `server/dispatch/dispatcher_test.go` — hits `Dispatcher.Dispatch`
  with a mock stream, asserts frame ordering and cancel semantics.
- New: `server/tunnel/adapter_seam_test.go` — asserts that
  `HttpAdapter` never bypasses `Dispatcher` (regression guard for the
  layering).

### 12.2 Integration
- `agent/server/snykbroker/*_test.go`: **must all still pass, unmodified.**
  This is the coexistence guard.
- New `agent/server/grpctunnel/integration_test.go`: agent + tunnel
  server + mock upstream in-process. Covers unary, chunked download
  (larger than one frame), timeout, cancel, agent reconnect, auth
  failure → re-register.
- New: a "kind hint" test proving `SERVER_STREAM` calls are streamed
  end-to-end without buffering (assert first-byte time on a slow
  producer).
- New: accept-file conformance suite (§9) — shared fixtures of
  (accept file, request) → (match/reject, resolved origin, injected
  headers), run against the Go matcher directly and through the
  snyk-broker e2e harness. This is the divergence guard for accept-file
  semantics across transports.
- New: golden-file tests on `acceptfile.Render()` — any `acceptfile`
  addition made for gRPC mode must not change rendered output for
  existing accept files.

### 12.3 E2E (docker-compose)
- Existing `agent/test/relay/relay_test.sh`: unchanged. Runs against
  snyk-broker mode as today.
- New `agent/test/relay/relay_test.grpc.sh`: mirrors PR #85's script
  against the new proto. Same integrations, same accept files.
- New: mixed run — bring up two agent containers, one in each mode,
  against the same integration alias, prove both work independently.

### 12.4 Cutover safety
- Feature-flag the CI matrix: every PR runs both modes. If any snyk-
  broker test needs a shim to keep passing while gRPC changes land,
  that's a design bug — fix it, don't shim it.

## 13. Migration for existing customers

1. Customer upgrades to an agent image that supports both modes.
   Default is snyk-broker; no behaviour change.
2. Cortex ops enables gRPC dispatch on the customer's tenant server-side.
3. Customer sets `AXON_RELAY_TRANSPORT=grpc-tunnel` and restarts. The
   agent's next `/register` gets a `grpc(s)://…` `ServerUri`.
4. Rollback: unset the env var, restart. No state carried between
   modes.

No customer accept-file changes. No new tokens. No re-registration
required at the Cortex UI level.

## 14. Risks & open questions

1. **Slot-pool ceiling.** ~~32 concurrent calls per agent is fine for
   today's workloads but not proven for future ones.~~ Resolved: the
   pool is adaptive (§5) — idles at min, grows toward max on a
   client-side free-capacity watermark, shrinks on idle. The remaining
   ceiling is `AXON_GRPC_TUNNEL_MAX_SLOTS` × per-token server cap,
   both config.
2. **Late trailers vs already-committed response.** If a backend
   returns a status: 200 followed by a `CallCancel`, the HttpAdapter
   has already flushed `WriteHeader(200)`. It logs and drops. This
   matches how Go's `httputil.ReverseProxy` behaves. Documented, not
   fixed.
3. **`routing_hint` semantics.** Empty for HTTP adapter in v1. Whether
   the gRPC adapter uses it to select a target service, or the agent
   infers from `:path` alone, is a v2 decision — not blocking v1.
4. **max_frame_bytes negotiation.** Server-chosen and one-way. If we
   ever need agent-driven caps (memory-constrained agents), add
   `ClientHello.max_frame_bytes` and take the min. Not needed v1.
5. **Trailers-only HTTP responses.** The wire supports them (`End`
   with populated `trailers` and no preceding `Data`). The HttpAdapter
   in v1 will render them as regular response headers if the client
   didn't send `TE: trailers`. This is standard net/http behaviour;
   noted for completeness.
6. **BROKER_SERVER API drift.** We are pinned to Snyk's dispatcher
   URL shape. If they change it, both modes break in the same way.
   Not a regression from today.

## 15. Work breakdown (rough)

1. `proto/tunnel/v2/tunnel.proto` + generated Go — 0.5d.
2. `server/tunnel/service.go` refactor to v2 proto, drop chunk
   assembler, add idle-slot tracking — 1d.
3. `server/dispatch/dispatcher.go` new interface + streaming impl — 2d.
4. `server/adapters/http.go` — refactor of PR #85's dispatch handler
   to sit above the Dispatcher — 1d.
5. `agent/server/grpctunnel/tunnel_client.go` — rebase to v2 proto,
   simplify assembler-away, expose slot-idle signal — 2d.
6. `agent/server/grpctunnel/router.go` + `backend/http.go` — split
   from PR #85's executor — 1d.
7. Tests (§12.1-§12.3) — 3d.
7a. Accept-file conformance suite + `Render()` golden files (§9) — 2d.
    Front-load this: it defines the semantics the Router must match and
    catches Node-broker quirks before they become bug reports.
8. Docker-compose + e2e scripts — 1d.
9. Documentation refresh (`README.relay.md` gets a "transports"
   section) — 0.5d.

Total: ~14 dev-days. Assumes PR #85's cloud infra (docker-compose,
tunnel-server binary, broker-client HTTP shim) is reused wholesale.

## Appendix A — Why not just fix PR #85 incrementally

The wire protocol is the thing worth getting right up front. Everything
else in PR #85 is fine and mostly stays. The proto change is easier now
(zero external consumers) than after even one design-partner customer
depends on it — at that point renaming `HttpRequest.is_final` to a
frame boundary becomes a two-sided coordinated deploy.

## Appendix B — Why not extend the existing `AxonAgent.Dispatch` RPC

`AxonAgent.Dispatch` (in `agent/proto/cortex-axon-agent.proto`) is
the SDK-side handler-invocation stream, not a relay transport. Its
frames are handler-shaped (`DispatchHandlerInvoke`) and its lifecycle
is per-handler registration. Overloading it with relay traffic would
tie two independent evolution paths together. Different service,
different port (agent listens on the SDK port; tunnel client dials out
to Cortex), different lifecycle. Keep them separate.

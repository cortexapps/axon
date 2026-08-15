# Serving both relay transports from one ingress

Findings from `multiplex_test.sh`, which runs two agents on different tokens —
one snyk-broker, one gRPC tunnel — behind a single nginx, and checks that
traffic for either token reaches the right agent and comes back.

**Result: it works.** Both agents connect through one port, and dispatch for
either token routes correctly. What follows is what the spike had to get right
to make that true, including two things that were not obvious up front.

## 1. Match the gRPC path, default everything else to snyk-broker

The demux rule is deliberately one-sided:

```nginx
location /cortex.axon.tunnel.v2. {
    grpc_pass grpc://grpc_tunnel;      # ours
}
location / {
    proxy_pass http://snyk_dispatch;   # everything else
}
```

The starting hypothesis was the reverse — that all snyk-broker traffic lives
under `/primus`, so we could match that and default to gRPC. **That is not
true.** The paths snyk-broker actually used, captured from the nginx access
log during a passing run:

| Path | Purpose |
|---|---|
| `GET/POST /primus/{token}/` | primus websocket + long-polling transport |
| `POST /response-data/{token}/{uuid}` | **response body uploads** |
| `GET /healthcheck` | health |
| `GET /rest/openapi` | served by the broker |

`/response-data/*` is the return path for relayed responses and is not under
`/primus`. A `/primus`-only rule would have handed those to the gRPC backend:
the handshake would look healthy and responses would fail, which is the worst
shape of bug to debug.

The asymmetry is the point. `/cortex.axon.tunnel.v2.TunnelService/Tunnel` is a
generated constant from our own proto package, so its stability is ours to
control. snyk-broker's routes belong to a vendored Node app that can add
paths in any release. Match what you own; default to what you don't.

## 2. One port serves both protocols

```nginx
listen 8080;
http2 on;
```

nginx detects the HTTP/2 connection preface, so h2c (gRPC without TLS) and
HTTP/1.1 (primus, including its `Upgrade: websocket`) coexist on one port.
This is what makes a single pod ingress possible at all — without it you would
need a port per transport and the agents would need to know which to use.

Requires nginx **≥ 1.25.1** for the `http2` directive. The older
`listen ... http2` form does not behave the same way.

## 3. Dispatch needs no demux at all

Both servers serve `/broker/{token}/...`, so at first this looks like a second
routing problem. It is not: **a token belongs to exactly one transport.** An
agent is either snyk-broker or gRPC, never both, so a token can only ever be
held by one server. Nothing has to map tokens to backends.

The right shape is therefore not to route dispatch through a shared front
door at all. Each server keeps its own headless Service and relay-dispatcher
addresses instances directly, using the identity the `client-connected`
notification already carried — which is exactly how snyk-broker works today.

The compose spike does route dispatch through nginx, using a static token map:

```nginx
map $uri $dispatch_backend {
    default              "http://snyk_dispatch";
    ~^/broker/tok-grpc/  "http://grpc_dispatch";
}
```

That exists only so the spike can drive both transports through one address
and prove the demux with one test. **It knows both tokens up front and does
not generalize** — do not carry it into production. The k8s manifests do not.

## 4. Two nginx details that cost a run each

**Hostnames inside a `proxy_pass` variable need a resolver.** These fail with
an opaque `502` and `upstream=-` in the log:

```nginx
map $uri $backend { default "http://grpc-tunnel-server:8080"; }
proxy_pass $backend;      # per-request DNS, needs `resolver`
```

Named upstreams resolve once at config load and avoid the problem entirely:

```nginx
upstream grpc_dispatch { server grpc-tunnel-server:8080; }
map $uri $backend { default "http://grpc_dispatch"; }
```

**Long-lived gRPC streams do not appear in the access log.** nginx logs on
request completion, and a tunnel stream stays open for the life of the
connection. Do not conclude gRPC is not routing because the log is empty —
check the tunnel server's `/healthz` stream count instead. The spike waits on
that count rather than on log output.

## 5. What the test proves, and how

Success alone is not evidence of correct routing: a misrouted request still
returns 200 from the wrong agent. So each agent is given a distinct identity
and the assertions check it:

- Distinct `-a` alias per agent, read back through `/broker/{token}/__axon/info`
- Distinct `CORTEX_INSTANCE_ID`, returned as `x-axon-relay-instance` on every
  relayed response by both transports
- A cross-token check that each backend **refuses** the other's token

The cross-token check is the one that would catch routing that appears to work
because both backends can reach both agents.

## 6. Translating to Kubernetes

Three pieces, on the **existing snyk-broker hostname**. Nothing outside them
changes: agents keep the address they use today and Cortex dispatches as it
does today, so which transport an agent speaks is invisible to the rest of the
system. Manifests in `k8s-relay.yaml`.

| Piece | What it is |
|---|---|
| `cortex-snyk-broker` | existing StatefulSet, untouched |
| `cortex-relay-grpc-tunnel` | new StatefulSet |
| `cortex-relay-nginx` | TLS front for the gRPC path only |

**Separate StatefulSets, not one pod with both servers.** Each server
identifies itself to relay-dispatcher by `$HOSTNAME` and derives its callback
address from that same string (`health_check_link: http://{serverID}/healthcheck`,
`broker_server_client.go` lines 164 and 208). Two containers in one pod share
a hostname, so they would register as the same server and the second would
overwrite the first — including its token connections. Separate pods have
distinct hostnames, so the collision cannot happen and no server code changes.

Two latent problems worth knowing, both avoided by this shape rather than
fixed: `SERVER_ID` is read by nothing (`getServerID()` only consults
`HOSTNAME`), so the `SERVER_ID` values in this repo's compose files are
silently ignored; and the advertised link carries no port, so it could not
distinguish two listeners on one host even with distinct IDs.

**nginx is required, not decoration.** GCLB needs HTTP/2 to a backend to carry
gRPC, and an HTTP/2 backend must accept TLS on the load-balancer leg. The
tunnel server speaks plaintext h2c and has no TLS support at all. nginx holds
a certificate so GCLB is satisfied and speaks h2c onward. GCLB does not verify
backend certificates, so self-signed is fine. Adding TLS to the tunnel server
would delete this component — a real simplification, needing cert loading,
config and rotation.

**The ingress splits the path; nginx only fronts gRPC.** That leaves
snyk-broker's path exactly as it runs in production today, rather than putting
a new hop and a new failure mode in front of a working transport. Routing
everything through nginx also works — that is what the compose spike does —
if you would rather have a single ingress backend.

**The path must be a whole segment.** Ingress prefix matching is segment-wise,
not character-wise, so `/cortex.axon.tunnel.v2.` does not match
`/cortex.axon.tunnel.v2.TunnelService/Tunnel`. Match
`/cortex.axon.tunnel.v2.TunnelService` instead. nginx `location` prefixes are
character-based, which is why the shorter form works there and not here.

**Timeouts and affinity.** `broker-ws-config` already proves GCLB carries
long-lived connections: `timeoutSec: 86400`. The gRPC BackendConfig mirrors
it. It deliberately omits `CLIENT_IP` affinity — primus needs that for
reconnection, gRPC does not, and an agent opens several independent streams it
expects spread across instances.

**Dispatch needs no routing at all.** A token belongs to exactly one
transport, so nothing has to map tokens to backends. Each StatefulSet keeps
its own headless Service and relay-dispatcher addresses instances directly,
exactly as it does for snyk-broker now.

## Running it

```bash
./agent/test/multiplex/multiplex_test.sh
```

Needs `cortex-axon-agent:local` built (`make docker-build`). It builds the two
server images itself, prints the nginx access log on exit, and tears the stack
down. It is a spike: not wired into CI, and it borrows the relay test's accept
files and registration fake rather than defining its own.

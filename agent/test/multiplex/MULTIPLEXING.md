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

## 3. Dispatch is the direction that does not self-describe

Agent-to-server demuxes cleanly, because the protocols differ. Server-to-agent
does not: **both servers serve `/broker/{token}/...`**, and nothing in the
request says which backend owns that token.

The spike resolves it with a static map, which works only because the test
knows both tokens:

```nginx
map $uri $dispatch_backend {
    default              "http://snyk_dispatch";
    ~^/broker/tok-grpc/  "http://grpc_dispatch";
}
```

**This does not generalize** and should not be carried into production. Real
options, roughly in order of preference:

1. **Separate Services for dispatch.** Keep one ingress for agents (where the
   protocol demux earns its keep) and give each server its own Service for
   inbound dispatch. Cortex already learns which server holds a token from the
   `client-connected` notifications, so it can address the right one directly.
   No token-aware routing anywhere.
2. **Try one backend, fall back to the other.** A server that does not hold
   the token returns an error, so `proxy_next_upstream` could retry the other.
   Cheap, but it turns every miss into two round trips and makes a genuine
   404 indistinguishable from a routing miss.
3. **Token-aware routing at the ingress.** Correct but requires the ingress to
   track token-to-transport, which is state the ingress should not own.

Option 1 matches how the system already works and is what the k8s spec below
assumes.

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

The pod holds three containers: nginx, the snyk-broker server, and the tunnel
server. nginx is the only one with a Service exposed to agents.

- **Agent ingress**: one Service on the nginx port. nginx demuxes by path, as
  above. This is the piece the spike validates.
- **Dispatch**: a Service per server (option 1), addressed by Cortex directly.
  Do not route dispatch through the shared nginx.
- **Ingress/TLS**: with TLS terminating at the ingress, ALPN negotiates `h2`
  for gRPC and `http/1.1` for primus, so the same demux applies — but the
  ingress must be configured to allow gRPC backends (`grpc_pass` equivalent).
  On nginx-ingress that is the `backend-protocol: GRPC` annotation, which is
  per-Ingress, so gRPC paths need their own Ingress object.
- **Timeouts**: both transports hold connections open for a long time. The
  spike sets an hour on the gRPC and websocket locations; the ingress needs
  matching `proxy-read-timeout` or streams will be cut on the default 60s.
- **Health**: the tunnel server's `/healthz` reports connected stream count,
  and snyk-broker has `/healthcheck`. Neither should be routed through nginx
  for probes — probe the containers directly so an nginx fault does not read
  as a backend fault.

## Running it

```bash
./agent/test/multiplex/multiplex_test.sh
```

Needs `cortex-axon-agent:local` built (`make docker-build`). It builds the two
server images itself, prints the nginx access log on exit, and tears the stack
down. It is a spike: not wired into CI, and it borrows the relay test's accept
files and registration fake rather than defining its own.

# Accept-file conformance fixtures

These fixtures pin the semantics of accept-file matching and rewriting so
they cannot diverge between the two relay transports (design doc
`docs/design/grpc-tunnel-v2.md` §9):

- **snyk-broker mode**: rules are enforced by the Node broker itself.
- **grpc-tunnel mode**: rules are enforced by the Go matcher
  (`acceptfile.MatchRule`) via the gRPC tunnel Router.

Each fixture is one JSON file:

```json
{
  "name": "what this fixture covers",
  "env": { "VAR": "value", ... },
  "acceptFile": { "private": [ ... ] },
  "cases": [
    {
      "name": "case description",
      "request": { "method": "GET", "path": "/x", "headers": { ... } },
      "expect": {
        "matched": true,
        "url": "https://origin.example/x",
        "headers": { "authorization": "Bearer t" },
        "absentHeaders": ["x-cortex-target-host"]
      }
    }
  ]
}
```

- `expect.matched: false` asserts the request is rejected.
- `expect.code` is the status a rejected request carries. It defaults to
  `404` (no rule matched); `403` says a rule matched but did not
  authorize the destination the request named; `400` says the request
  was malformed before matching (bad encoding, directory traversal).
- `expect.url` asserts the exact URL (path + query included) that would be
  sent to the upstream.
- `expect.headers` asserts a **subset** of the outgoing request headers
  (case-insensitive names).
- `expect.absentHeaders` asserts headers that must NOT reach the
  upstream — internal routing metadata, chiefly.

Runners:

1. `agent/server/grpctunnel/conformance_test.go` runs every fixture
   through the Go path (accept-file render → Router.Route) on every test
   run.
2. A second runner playing the same cases through a live broker
   (`agent/test/relay/`) is **not built yet**. Until it is, divergence
   from the Node broker is caught by review and by the fixtures below
   rather than by the build, so a fixture that encodes broker parity
   should say so in its case names.

Deliberate divergences from snyk-broker, so they are not mistaken for
bugs:

- `valid` header values are compared case-insensitively, and an empty
  `values` array means "the header must be present". snyk-broker
  compares case-sensitively and an empty array rejects everything. Both
  differences are more permissive, so a request the broker routed still
  routes.
- **Nothing in an accept file stops the agent.** Enabling the tunnel
  switches deployments that run on snyk-broker today, and a file the
  broker accepts has to still start. Constructs the Router cannot carry —
  body and query `valid` filters, `requiredCapabilities`, unrecognized
  auth schemes, inbound `public` rules — are warned about and ignored.
  A warning that changes what a rule matches says so.
  The one exception is a malformed wildcard origin, which the
  snyk-broker path refuses too (at render, or by panicking when the
  reflector is off), so no working deployment carries one.
- `${VAR}` in a `path` is a **segment placeholder, not a filter**: it
  matches whatever the caller sent there and the configured value is
  substituted into the outgoing URL. That is snyk-broker's behaviour,
  quirks included.

When adding an accept-file feature, add fixture cases FIRST — they define
the semantics both transports must implement.

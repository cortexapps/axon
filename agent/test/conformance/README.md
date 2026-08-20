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
        "headers": { "authorization": "Bearer t" }
      }
    }
  ]
}
```

- `expect.matched: false` asserts the request is rejected (no rule).
- `expect.url` asserts the exact URL (path + query included) that would be
  sent to the upstream.
- `expect.headers` asserts a **subset** of the outgoing request headers
  (case-insensitive names).

Runners:

1. `agent/server/grpctunnel/conformance_test.go` runs every fixture
   through the Go path (accept-file render → Router.Route) on every test
   run.
2. The snyk-broker docker E2E harness (`agent/test/relay/`) is the second
   runner: it plays the same cases through a live broker and asserts what
   reaches a mock upstream. Divergence — including pre-existing quirks of
   the Node broker — fails the build and forces a decision: match the
   broker's behaviour, or document the exception here with a reason.

When adding an accept-file feature, add fixture cases FIRST — they define
the semantics both transports must implement.

# AGENTS.md

Guidance for AI coding agents working in this repository.

# Comments and PR content

- Never include customer names or identifiers, always anonymize references

# Architecture notes

- The snyk-broker component is always used in websocket mode. Data transport never uses long polling.

# Protocol compatibility

- Changes under `proto/` MUST be wire compatible in both directions. Tunnel servers and agents are deployed independently and run mixed versions, so existing functionality must never depend on which side is upgraded first.
- Change by adding: new fields, new enum values, new `oneof` members. Never renumber, reuse, or change the type or meaning of an existing field. A field that must be retired stays unused and its number and name are `reserved`.
- A new feature may need both sides to know about it before it lights up. A peer that does not must degrade gracefully: existing behavior continues, and nothing fails, hangs, or tears down the stream.
  - Treat a new field's zero value as "the peer didn't say" (e.g. `STAGE_UNSPECIFIED`), never as a meaningful setting.
  - Ignore `oneof` members you don't recognize rather than treating them as a protocol error.
  - Bound anything that waits on a newer peer (e.g. a grace period), so an older peer that never answers costs latency, not a failure.

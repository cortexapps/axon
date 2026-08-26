# Axon accept.json — language specification and conformance audit

Status: audit + proposed normative spec. No code changes.
Audited: `cortexapps/axon@9c5a802` (main), `cortexapps/snyk-broker@43d61aa` (branch `axon`).

## 1. Why this document exists

Axon is on a path to drop the `snyk-broker` Node process entirely and serve all
relay traffic over the gRPC tunnel. The tunnel brought its own accept-file
engine (`agent/server/snykbroker/acceptfile`, Go), which now runs *beside* the
Node broker's engine (`lib/common/filter`, TypeScript) rather than after it.

Two engines, one file format. `agent/test/conformance/README.md` already states
the rule that follows from that:

> When adding an accept-file feature, add fixture cases FIRST — they define the
> semantics both transports must implement.

This document does three things:

1. **§3** catalogs what snyk-broker's accept.json language actually is, from its
   parser and its unit tests.
2. **§4–§5** defines the subset Axon must support (normative), and explicitly
   catalogs the snyk-broker features Axon does *not* need.
3. **§6–§7** audits current Go test coverage against that spec and lists the
   gaps, each with a reproduced observation.

Every behavioural claim below about the Go engine was reproduced by running it;
§8 has the method.

---

## 2. Where the code lives

### snyk-broker (reference implementation)

| Concern | File |
| --- | --- |
| Rule compilation + matching | `lib/common/filter/filtersAsync.ts` |
| Header validators | `lib/common/filter/utils.ts` (`validateHeaders`) |
| `${VAR}` and pool expansion | `lib/common/utils/replace-vars.ts` (`replace`) |
| Auth header construction | `lib/common/utils/auth-header.ts` |
| Rule type | `lib/common/types/filter.ts` |
| Ruleset loading / templating | `lib/common/filter/filter-rules-loading.ts` |
| **Unit tests** | `test/unit/filters.test.ts` (947 lines, ~60 cases) |
| Loading-failure tests | `test/unit/filter-loading.test.ts` |
| Fixtures the tests match against | `test/fixtures/client/filters.json`, `test/fixtures/accept/*.json` |

`test/unit/filters.test.ts` is the file the task asks about. It is organised as
`describe('on URL' | 'on body' | 'on querystring' | 'on query and body' | 'on
headers')` plus `describe('with auth')`, and asserts exactly the thing named in
the task: *a given path picks the right rule, and that rule's origin and auth
are applied*. Each case calls the compiled filter with a request payload and
asserts the returned `{url, auth}` — e.g. `should allow valid /repos path to
manifest`, `allows correct basic auth requests`, `allows requests with a correct
token`, and the negative cases `should block when path includes directory
traversal` and `should block when manifest appears after fragment identifier`.

### Axon (Go)

| Concern | File |
| --- | --- |
| Parse, `${...}` preprocessing, render, typed wrappers | `agent/server/snykbroker/acceptfile/accept_file.go` |
| Rule matching (`method`, `path`, `valid`) | `agent/server/snykbroker/acceptfile/matcher.go` |
| Transport-agnostic routing (origin → URL, header/auth inject) | `agent/server/snykbroker/acceptfile/router.go` |
| `${VAR}`/`${env:}`/`${plugin:}`/`${VAR:default}` resolution | `agent/server/snykbroker/acceptfile/resolver.go` |
| `_POOL` round-robin | `agent/server/snykbroker/acceptfile/pool.go` |
| Plugin discovery + execution | `agent/server/snykbroker/acceptfile/plugin.go` |
| gRPC tunnel adapter | `agent/server/grpctunnel/router.go` |
| **Wildcard origin (reflector only)** | `agent/server/snykbroker/reflector.go` |
| Reflector wiring + guards | `agent/server/snykbroker/relay_instance_manager.go` |
| Shipped accept files | `agent/server/snykbroker/accept_files/accept.*.json` |
| Cross-transport fixtures | `agent/test/conformance/*.json` |

---

## 3. The snyk-broker accept.json language

A file is `{ "private": Rule[], "public": Rule[] }` (or, under universal broker,
a map of `type → {private, public}`). `private` = outbound, agent → customer
system. `public` = inbound, Snyk → agent (webhooks). First matching rule wins;
`loadFilters` returns on the first test that yields a result.

### 3.1 Rule fields

| Field | Meaning | Notes |
| --- | --- | --- |
| `method` | HTTP method | **Defaults to `get`** when absent. `any` matches all. Compared lowercased. |
| `path` | `path-to-regexp@1.9.0` pattern | `:name` params, `*` → `(.*)`. `${VAR}` in a path is rewritten to `:VAR` and then substituted back from config. |
| `origin` | Upstream base URL | `${VAR}` expanded via `replace()`, incl. pool rotation. |
| `auth` | `{scheme, token?, username?, password?}` | Schemes: `token`, `bearer`, `basic`, `raw`. |
| `valid` | Array of validators | Four kinds, see §3.2. |
| `requiredCapabilities` | `string[]` | Throws if the client doesn't advertise them. |
| `stream` | `boolean` | Marks a streaming response. |
| `url` | declared in the `Rule` type | Not read by `filtersAsync.ts`. |
| `"//"` | Comment convention | Used at rule and validator level; ignored. |

### 3.2 `valid` validator kinds

| Shape | Validates |
| --- | --- |
| `{path, value}` | JSON body, `undefsafe` path with `*` globs |
| `{path, regex}` | JSON body, path + regex |
| `{queryParam, values}` | Query string, `minimatch` glob per value |
| `{header, values}` | Request headers |

Combination semantics, from `filtersAsync.ts`:

- Body, body-regex and query validators form **one OR group**: if any of those
  three kinds is present, **at least one** must be satisfied or the rule is
  rejected. (Within the query kind it is `.every(queryParam)`, each satisfied by
  `.some(value)`.)
- Header validators are checked **separately and are all required**
  (`validateHeaders` returns false on the first miss).
- `validateHeaders` uses `values.includes(headerValue)` — **case-sensitive
  value comparison**, and an **empty `values` array rejects everything**.

### 3.3 Path handling before matching

1. `path.normalize(req.url) !== req.url` → **reject** (directory traversal).
2. Everything after the first `#` is **discarded** (fragment).
3. Split on the first `?` into url + querystring.
4. Match; on success, substitute `${VAR}` path params back from config.
5. Result: `origin + url + querystring`.

### 3.4 Auth (`auth-header.ts`)

| `scheme` | Emitted `Authorization` |
| --- | --- |
| `token` | `Token <token>` |
| `bearer` | `Bearer <token>` |
| `basic` + `username`/`password` | `Basic base64(user:pass)` |
| `basic` + `token` | `Basic base64(token)` — token is the *pre-joined* pair |
| `raw` | `<token>` verbatim, no prefix |
| anything else | `undefined` — no header |

### 3.5 `${VAR}` expansion (`replace-vars.ts`)

`${KEY}` resolves from config, with pool support: if `KEY_POOL` (or `KEYPool`)
is a comma-separated list, successive expansions **round-robin** through it.
Otherwise `source[KEY] || ''` — an unknown var silently becomes the empty
string.

---

## 4. Axon's extensions to the language

Three extensions exist beyond snyk-broker. All three are Axon-side only; the
Node broker never sees them in their raw form.

### 4.1 `headers` — arbitrary outbound request headers

```json
{ "method": "any", "path": "/*", "origin": "${HARNESS_API}",
  "headers": { "x-api-key": "${HARNESS_TOKEN}" } }
```

A map of header name → value, injected on the outgoing request, **overriding**
any same-named header from the caller. Used by `accept.harness.json`,
`accept.github.app.json`, `accept.google.json`.

Implemented twice:
- Reflector: `relay_instance_manager.go:477-493` rewrites each rule's origin to
  a reflector proxy URI carrying the header resolver; the reflector's `Director`
  injects (`reflector.go:473-477`).
- Tunnel: `acceptfile/router.go:88-96`, `header.Set` per rule header.

Because the Node broker cannot inject headers, reflector mode **panics at render
time** if a rule has `headers` and `ENABLE_RELAY_REFLECTOR` is not `all`/`traffic`
(`relay_instance_manager.go:462-466`).

### 4.2 `${plugin:name}` — dynamic header values

A header value may embed `${plugin:name}`, which executes `name` from
`config.PluginDirs` (default `./plugins`, overridable via `PLUGIN_DIRS`) and
substitutes its stdout, trimmed of newlines.

```json
"headers": { "Authorization": "Bearer ${plugin:github-app-token}" }
```

Pipeline: `preProcessContent` rewrites `${plugin:x}` → `{{plugin:x}}` so
`os.ExpandEnv` cannot eat it (`resolver.go:18-21`); `CreateResolver` expands env
vars once at parse and returns a closure that re-executes plugins **on every
`Resolve()`** — i.e. per request, which is what a rotating credential needs.
Plugins are currently accepted **only in `headers`** (`plugin.go:13-24`).

Shipped plugins: `github-app-scaffolder`, `github-app-token`, `google-adc`.

### 4.3 Wildcard origins

Added in `112a38c` (per-request target-host retargeting). An origin may name a
*family* rather than a host:

```json
{ "origin": "${GOOGLE_API:https://*.googleapis.com}" }
```

Exactly one `*`, as the **leftmost DNS label only** — matching what a wildcard
TLS certificate covers. The concrete host arrives per request in the
`X-Cortex-Target-Host` header and is checked against the family before dialing.

Reflector semantics, all in `reflector.go` / `reflector_dynamic_target_test.go`:

| Rule | Where |
| --- | --- |
| One `*`, leftmost label, ≥2 further labels | `parseOrigin` |
| `*` matches exactly one label — `a.b.api.example.net` does not match `*.api.example.net` | `wildcardOrigin.matches` |
| Wildcard origin **requires** a target host; absent → 403 | `resolveTargetHost` |
| Duplicate `X-Cortex-Target-Host` → 403 | `resolveTargetHost` |
| Host outside the family → 403 | `resolveTargetHost` |
| Port comes from the origin, never the header | `Director` |
| WebSocket upgrade on a wildcard origin → 403 | `ServeHTTP` |
| Header is stripped before anything can forward or log it | `reflector.go:354-355` |
| Wildcard + `HttpDisableTLS` → render error | `relay_instance_manager.go:485-487` |
| Wildcard without reflector → render panic | `relay_instance_manager.go:467-469` |

### 4.4 Other Axon-only conveniences

| Feature | Where |
| --- | --- |
| `${env:VAR}` — explicit env namespace | `resolver.go:23-25` |
| `${VAR:default}` — default when unset; `${GITHUB:github.com}` in `accept.github.json` | `resolver.go:27-42` |
| `$vars` / `vars` — top-level array declaring required vars; validated at load | `accept_file.go:50`, `resolver.go:271-286` |
| Scheme-less origins default to `https://` | `accept_file.go:225-240` |
| `_POOL` round-robin (Go reimplementation) | `pool.go` |
| Injected `/__axon/*` self-route on render | `accept_file.go:114-130` |
| Unknown keys round-trip losslessly (`map[string]any`, not a struct) | `accept_file.go:136-141` |

---

## 5. Normative spec: what Axon accept.json must support

Derived from every `accept.*.json` that ships (`accept_files/`) plus the three
extensions. **MUST** = required for snyk-broker removal.

### 5.1 Required

| # | Feature | Used by |
| --- | --- | --- |
| R1 | `private` rule array, first-match-wins | all |
| R2 | `method`: `any`, `GET`, `POST`; case-insensitive | all |
| R3 | `path`: exact (`/graphql`), prefix wildcard (`/*`, `/api/*`), **infix wildcard (`/*/info/refs`)** | `accept.gitlab.json`, `accept.github*.json` |
| R4 | `origin` with `${VAR}` | all |
| R5 | `origin` with `${VAR:default}`, including scheme-less defaults | `accept.github*.json` |
| R6 | `origin` as wildcard family + `X-Cortex-Target-Host` | `accept.google.json` |
| R7 | `auth.scheme = bearer` | github, gitlab, jira.bearer, bitbucket, sonarqube |
| R8 | `auth.scheme = basic` with username/password | github, gitlab, jira, prometheus, bitbucket.basic |
| R9 | `headers` map, overriding caller headers | harness, github.app, google |
| R10 | `${plugin:name}` in header values, re-resolved per request | github.app, google |
| R11 | `valid` with `{header, values}` as a rule selector | github, github.app |
| R12 | `$vars` declaration + fail-fast on missing env | github.app |
| R13 | Percent-encoding preserved to the wire (`%2F` in GitLab project IDs) | gitlab |
| R14 | Query string forwarded verbatim | all |
| R15 | `${VAR}` + `VAR_POOL` round-robin | none shipped, but `varIsSet` accepts `_POOL` as satisfying R12, so it is load-bearing |
| R16 | Unknown keys (`"//"`, future fields) round-trip without loss | convention |

### 5.2 Explicitly out of scope

Catalogued so a future reader does not re-derive them. Each of these exists in
snyk-broker and is **not** required by any Axon accept file.

| snyk-broker feature | Why Axon does not need it | Current Go behaviour |
| --- | --- | --- |
| `valid` `{path, value}` body filters | Snyk inspects webhook/manifest bodies; Axon rules gate on route + header only | **Silently ignored — fails open** |
| `valid` `{path, regex}` body filters | same | **Silently ignored — fails open** |
| `valid` `{queryParam, values}` | same | **Silently ignored — fails open** |
| `public` rules (inbound webhooks) | Cortex does not push inbound traffic through the relay | Parsed and preserved; Router never consults them |
| `requiredCapabilities` | Snyk client-capability negotiation | Ignored |
| `stream` | The tunnel streams every body unconditionally | Ignored |
| `url` rule field | Dead in snyk-broker too | Ignored |
| `:param` path segments | Axon rules use `*`, not named params | Not implemented |
| `${VAR}` inside `path` | Not used by any Axon file | Not implemented |
| `auth.scheme = token` / `raw` | Not used by any Axon file | **Implemented incorrectly — see G5** |
| `basic` + pre-encoded `token` | Not used by any Axon file | **Implemented incorrectly — see G5** |
| Universal-broker multi-type filter maps | Axon runs one integration per relay instance | Not implemented |
| Ruleset download over HTTP | Axon ships files in the image / mounts them | Not implemented |

The three "silently ignored — fails open" rows are the ones that matter. Not
supporting them is a choice; **accepting a file that uses them and then widening
the allowlist is not.** See G6.

---

## 6. Test-coverage audit

Legend: ✅ covered · ⚠️ partial · ❌ none

| # | Requirement | Go unit coverage | Conformance fixture | Verdict |
| --- | --- | --- | --- | --- |
| R1 | first-match-wins | `matcher_test.go:TestMatchRule_WithValidHeaders` | `valid_headers.json` | ✅ |
| R2 | methods, `any`, case | `matcher_test.go:TestMatchRule_MethodAndPath` | `basic_matching.json` | ✅ |
| R3a | exact + trailing `/*` | `matcher_test.go:TestMatchRule_WildcardSubpath` | `basic_matching.json`, `paths_and_encoding.json` | ✅ |
| R3b | **infix `/*/info/refs`** | none | none | ❌ **G3** |
| R4 | `${VAR}` origin | `accept_file_test.go:TestAcceptFileValidate` | every fixture | ✅ |
| R5 | `${VAR:default}`, scheme-less | `accept_file_test.go`, `TestRenderEnvVars` | none | ⚠️ no routed-URL assertion |
| R6 | **wildcard origin on the tunnel** | reflector only (`reflector_dynamic_target_test.go`, 12 cases) | none | ❌ **G1** |
| R7 | bearer auth | `grpctunnel/router_test.go:TestRouterBackend_BearerAuth` | `auth_and_headers.json` | ✅ |
| R8 | basic auth | `grpctunnel/router_test.go:TestRouterBackend_BasicAuth` | `auth_and_headers.json` | ✅ |
| R9 | `headers` injection + override | `router_test.go:TestRouterBackend_RuleHeaderInjection`; reflector `reflector_headers_test.go` | `auth_and_headers.json` | ✅ |
| R10 | `${plugin:}` per-request | `plugin_test.go` (exec, discovery, failure — **in isolation**) | none | ⚠️ **G13** |
| R11 | `valid` header gating | `matcher_test.go:TestMatchRule_ValidHeaderRequirement` (13 cases) | `valid_headers.json` | ✅ |
| R12 | `$vars` fail-fast | `accept_file_test.go:TestAcceptFileValidate` | none | ✅ |
| R13 | `%2F` preserved | `router_test.go:TestRouterBackend_EncodedSlashPreserved`, `acceptfile/router_test.go:TestBuildTargetURL` | `paths_and_encoding.json` | ✅ |
| R14 | query forwarded | `router_test.go:TestRouterBackend_QueryStringForwarded` | `basic_matching.json` | ✅ |
| R15 | **`_POOL` end-to-end** | `pool_test.go` (PoolManager **in isolation**) | none | ❌ **G4** |
| R16 | round-trip of unknown keys | `render_golden_test.go` (10 golden files) | n/a | ✅ |

Adjacent, not in the requirement table:

| Concern | Coverage | Verdict |
| --- | --- | --- |
| `X-Cortex-Target-Host` stripped before upstream | reflector: `reflector_boundary_test.go` ×4. Tunnel: none | ❌ **G2** |
| Directory traversal rejected | snyk-broker has a test; Go has none | ❌ **G7** |
| Fragment (`#`) stripped before matching | snyk-broker ×4; Go none | ❌ **G10** |
| Rule with no `method` | none | ❌ **G9** |
| `auth.scheme` token/raw/basic-with-token | none | ❌ **G5** |
| Body/query `valid` rejected or honoured | none | ❌ **G6** |
| Second conformance runner (live broker) | **does not exist** | ❌ **G12** |

---

## 7. Gaps

Ordered by what blocks customer testing of the tunnel.

### G1 — Wildcard origins are unimplemented on the tunnel path (blocks `accept.google.json`)

`acceptfile.Router` has no notion of a wildcard origin. `buildTargetURL` parses
`https://*.googleapis.com`, finds a non-empty scheme and host, and **returns
successfully**:

```
Route("GET", "/storage/v1/b", {"X-Cortex-Target-Host": "storage.googleapis.com"})
  → err=<nil>  url=https://*.googleapis.com/storage/v1/b
```

`X-Cortex-Target-Host` is neither consumed nor validated. Every Google request
over the tunnel dials a literal `*.googleapis.com`.

The whole policy exists next door in `reflector.go` — family parsing, one-label
matching, absent/duplicate/out-of-policy rejection, WS refusal, TLS-verification
requirement — with thorough tests. None of it is reachable from `Router`.

Compounding it: reflector mode **panics at render** when a wildcard origin is
used without the reflector (`relay_instance_manager.go:467-469`). The tunnel has
no equivalent guard, so instead of a loud failure it produces a broken request.

**Fix**: move `parseOrigin`/`wildcardOrigin`/`resolveTargetHost` into the
`acceptfile` package and have `Router.Route` apply them; add conformance cases;
until then, reject a wildcard origin at tunnel-router construction.

### G2 — `X-Cortex-Target-Host` reaches the upstream on the tunnel path

The reflector deletes it before any forwarding, logging or rejection path
(`reflector.go:354-355`), and `reflector_boundary_test.go` pins that in four
tests including the rejection and WebSocket paths. `Router.Route` copies every
caller header verbatim:

```
outgoing headers = map[Authorization:[Bearer caller-supplied]
                       Connection:[keep-alive]
                       X-Cortex-Target-Host:[evil.example.com]]
```

Internal routing metadata must not survive the hop. Note also that `Connection`
and other hop-by-hop headers are forwarded, and a caller-supplied
`Authorization` survives when the rule declares no `auth` — worth an explicit
decision either way.

### G3 — `*` in `path` means different things in the two engines (breaks shipped `accept.gitlab.json`)

snyk-broker compiles with `path-to-regexp@1.9.0`, where `*` becomes `(.*)` and
**crosses `/`**. Go uses `path.Match`, where `*` **stops at `/`**.

Verified against both engines:

| Pattern | Request | snyk-broker | Go |
| --- | --- | --- | --- |
| `/*/info/refs` | `/myrepo/info/refs` | match | match |
| `/*/info/refs` | `/mygroup/myrepo/info/refs` | match | **no match** |
| `/*/info/refs` | `/a/b/c/x.git/info/refs` | match | **no match** |
| `/api/*` | `/api/a/b/c` | match | match |
| `/api/*` | `/api` | **no match** | match |

`accept.gitlab.json` ships `/*/info/refs`, `/*/git-upload-pack`,
`/*/git-receive-pack` — the git-over-HTTP rules added in `5db358b` for
scaffolder clones. GitLab nested groups (`group/subgroup/project.git/...`) are
the common case. Over the broker those clone; **over the tunnel they 404**, and
`accept_file_gitlab_test.go` will not notice because it only asserts the *shape*
of the JSON, never that a path matches.

The `/api/*` row is the mirror image: Go is more permissive than the allowlist
author wrote.

**Fix**: implement snyk-broker `*` semantics in `matchesPath` (or translate the
pattern to a regexp), and add fixture cases for nested paths and for bare-prefix
non-match.

### G4 — `${VAR}` pools never work through the Router

`AcceptFileRuleWrapper.Origin()` calls `os.ExpandEnv` first (`accept_file.go:230`).
With only `POOLED_API_POOL` set, `${POOLED_API}` expands to the empty string, the
URL parse yields a bare scheme, and `Router.Route` then hands `"https:"` to
`PoolManager.ResolvePoolVars`, which has no `${...}` left to see:

```
rule.Origin() = "https:"
Route → failed to build target URL: origin "https:" has no scheme or host   (×3)
```

`varIsSet` deliberately accepts `VAR_POOL` as satisfying a required var
(`resolver.go:288`), so such a file **passes load validation** and then fails
every request. `pool_test.go` exercises `PoolManager` directly and stays green,
which is exactly why this is invisible.

**Fix**: resolve pools before/inside `Origin()` (or have `Origin()` return the
unexpanded string and let the Router own all expansion), plus an end-to-end
round-robin test through `Route`.

### G5 — Auth schemes diverge from snyk-broker

| `auth` | snyk-broker | Go `applyAuth` |
| --- | --- | --- |
| `{scheme: "token", token: "abc"}` | `Token abc` | `Bearer abc` |
| `{scheme: "raw", token: "abc"}` | `abc` | `raw abc` |
| `{scheme: "basic", token: "dXNlcjpwYXNz"}` | `Basic dXNlcjpwYXNz` | `Basic Og==` (base64 of `":"`) |
| unknown scheme | no header at all | `<scheme> <token>` |

The `basic` + `token` row is the worst: the token is dropped and an empty
credential is sent. No Axon accept file uses these today, so nothing shipped is
broken — but "drop-in replacement for snyk-broker" is not true for a
customer-authored file, and there is no test for any of the four rows.

**Fix**: match `auth-header.ts` exactly, or reject unsupported schemes at load.
Either way, cover all four rows in the fixtures.

### G6 — Body and query `valid` filters are silently ignored, and the ignore fails open

```
rule: valid: [{queryParam: "proxyMe", values: ["please"]}]
  GET /q/thing?proxyMe=nope  → snyk-broker: rejected · Go: matched

rule: valid: [{path: "proxy.*", value: "please"}]
  POST /b/thing (no body)    → snyk-broker: rejected · Go: matched
```

`AcceptFileRuleWrapper.Valid()` reads only entries with a `header` key
(`accept_file.go:310-338`); everything else is dropped on the floor. A rule whose
*purpose* is to narrow an allowlist becomes an unconditional allow.

Not supporting these filters is fine (§5.2). Accepting a file that uses them and
then widening the allowlist is not.

**Fix**: reject at load with a clear error naming the unsupported validator kind
— cheap, and it converts a silent security regression into a startup failure.

### G7 — No directory-traversal rejection

snyk-broker rejects any request where `path.normalize(req.url) !== req.url`, and
has the test `should block when path includes directory traversal`. Go has no
equivalent:

```
GET /api/../admin       → matched /api/*  → https://up.example/api/../admin
GET /api/%2E%2E/admin   → matched /api/*  → https://up.example/api/%2E%2E/admin
```

Whether the upstream collapses those is the upstream's business; the allowlist
should not be the thing relying on it.

### G8 — `valid` header comparison is more permissive than snyk-broker

Go uses `strings.EqualFold` for both header name and value, and treats an empty
`values` array as "the header must merely exist" (`matcher.go:38-50`).
snyk-broker's `validateHeaders` compares values **case-sensitively** and an empty
`values` array rejects everything (`[].includes(v)` is always false).

`matcher_test.go` pins the lenient behaviour as intended, so this needs a
decision rather than a fix. Whichever is chosen should be stated in the
conformance README as a documented exception.

### G9 — A rule with no `method` never matches

snyk-broker defaults `method` to `get`. `Method()` returns `""`, and
`matchesMethod("", "GET")` is false, so the rule is dead:

```
{"path": "/api/*", "origin": "..."} + GET /api/x → no matching accept file rule
```

Cheap either way — default to GET, or reject at load. Silence is the wrong
answer.

### G10 — Fragments are not stripped

snyk-broker discards everything after `#` before matching and has four tests
about it (`should not allow access to sensitive files by putting the manifest
after a fragment`). Go percent-encodes it into the path:

```
GET /api/x#/../y → https://up.example/api/x%23/../y
```

Lower risk over the tunnel — a fragment does not normally survive to a
`:path` pseudo-header — but it is a real difference and free to close.

### G11 — `public` rules are parsed and then ignored

`Render` ensures a `public` array exists; `Router` only ever consults
`PrivateRules()`. If inbound traffic is genuinely out of scope (§5.2), a file
that declares `public` rules should say so at load rather than appear to work.

### G12 — The second conformance runner does not exist

`agent/test/conformance/README.md` states:

> 2. The snyk-broker docker E2E harness (`agent/test/relay/`) is the second
>    runner: it plays the same cases through a live broker […] Divergence —
>    including pre-existing quirks of the Node broker — fails the build.

Nothing under `agent/test/` references `conformance` except that README.
`agent/test/relay/` contains `relay_test.sh`, `relay_test.grpc.sh` and
`relay_scenarios.sh`, none of which read the fixtures. The one-definition rule
is documented but unenforced — which is precisely how G3, G5 and G6 became
possible.

**Fix**: either build the runner, or amend the README to describe what is
actually enforced. The first is much better; the fixtures are already in the
right shape for it.

### G13 — Plugin behaviour is untested at the request boundary

`plugin_test.go` covers `Plugin.Execute`, `FindPlugin`, non-executable files, and
resolver substitution — all in isolation. Nothing covers a plugin as it is
actually used, and three behaviours follow from the code with no test:

1. **Discovery runs per request.** `Router.Route` calls `rule.Headers()`, which
   calls `CreateResolver` → `findPlugins` → `FindPlugin` for every request, then
   forks a process per plugin header. That is an `exec.LookPath` + `os.Stat` +
   `fork/exec` per request per header on the hot path — worth a load-test
   assertion given the tunnel has been load-tested.
2. **A missing plugin panics the process at request time.** `findPlugins` calls
   `logger.Panic` (`resolver.go:102-107`), and neither `grpctunnel` nor
   `snykbroker` has a `recover()`. A plugin removed after startup takes the agent
   down rather than failing one route.
3. **A failing plugin leaks its placeholder into the header.** On execution
   error `CreateResolver`'s closure logs and continues, leaving the literal
   `{{plugin:github-app-token}}` in the value — so the upstream receives
   `Authorization: Bearer {{plugin:github-app-token}}` and answers 401 with no
   local signal beyond a log line.

Also: `Plugin.Execute` uses `exec.Command` with **no timeout or context**
(`plugin.go:57`). A hung credential helper hangs the request indefinitely.

---

## 8. How the Go-side observations were produced

A temporary `_test.go` in `agent/server/snykbroker/acceptfile` built rules
through the real path used by the tunnel client — `NewAcceptFile` → `Render` →
`NewAcceptFile` → `PrivateRules` minus `/__axon/*` → `NewRouter` — then called
`Route` and printed the result. `matchesPath` was called directly for the path
table. It was removed after the audit; recreate from the tables above if needed.

snyk-broker semantics were read from source, and the `path-to-regexp` rows in G3
confirmed by compiling the same patterns with `path-to-regexp@1.9.0`:

```
/*/info/refs => ^\/((?:.*))\/info\/refs(?:\/(?=$))?$
```

---

## 9. Suggested order of work

| Priority | Gaps | Rationale |
| --- | --- | --- |
| P0 — before customer testing | G1, G3 | Two shipped accept files (`google`, `gitlab`) do not work correctly over the tunnel |
| P0 — security boundary | G2, G6 | One leaks internal routing metadata upstream; one silently widens an allowlist |
| P1 — correctness | G4, G5, G9 | Documented/implied features that do not work; cheap to fix or reject loudly |
| P1 — process | G12 | Without the second runner the one-definition rule is unenforced |
| P2 — hardening | G7, G10, G13 | Traversal, fragments, plugin failure modes and timeout |
| P2 — decide and document | G8, G11 | Deliberate divergences that should be written down |

Every P0/P1 item should land as a `agent/test/conformance/*.json` case first,
per the rule the README already sets.

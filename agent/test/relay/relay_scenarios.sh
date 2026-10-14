# Shared end-to-end scenarios, run identically against both relay transports.
#
# snyk-broker and the gRPC tunnel are meant to be interchangeable: the same
# accept files, the same rewriting, the same credential injection. The only
# way that claim stays true is if the same assertions run against both, so
# these live here rather than being copied into each transport's script.
# They had already drifted once — the gRPC copy had quietly lost the agent
# endpoint and GitLab credential-injection checks.
#
# Contract for callers, set before sourcing:
#   DISPATCH_URL   ingress for this transport, ending in /broker/<token>
#   TOKEN          the broker token
#   PROXY          1 when running the proxied topology
#   EXPECT_RELAY_MODE  the transport this script drives, as __axon/info reports it
#   COMPOSE_FILES  compose -f flags, for log dumps on failure
#   curlw          a curl wrapper that fails the run on transport errors
#
# Scenarios that are genuinely transport-specific — snyk's raw WebSocket
# tunnel, the gRPC stream handshake, which container to SIGKILL — stay in the
# per-transport scripts. Everything here must hold for both.

# Dumps whatever context is likely to explain a failure, then exits.
scenario_fail() {
    echo "FAIL: $1"
    shift
    for extra in "$@"; do
        echo "$extra"
    done
    echo "=== axon-relay logs (last 50) ==="
    docker compose $COMPOSE_FILES logs --tail=50 axon-relay 2>&1 || true
    exit 1
}

# The agent's own endpoint, served locally rather than relayed. Proves the
# agent is up and configured before any assertion about relaying means
# anything.
scenario_axon_endpoints() {
    echo "Checking axon endpoints..."
    local info alias integration
    info=$(curlw "$DISPATCH_URL/__axon/info")

    alias=$(echo "$info" | jq -r '.alias')
    if [ "$alias" != "axon-test" ]; then
        scenario_fail "Expected alias 'axon-test', got '$alias'" "=== Info response: $info ==="
    fi

    integration=$(echo "$info" | jq -r '.integration')
    if [ "$integration" != "github" ]; then
        scenario_fail "Expected integration 'github', got '$integration'" "=== Info response: $info ==="
    fi

    # The backend reads these to track what the fleet is actually running, so
    # they have to be right per transport rather than merely present. Asserting
    # the transport here is what makes this scenario differ between the two
    # scripts that share it — everything else must match.
    local mode build
    mode=$(echo "$info" | jq -r '.relay_mode')
    if [ "$mode" != "$EXPECT_RELAY_MODE" ]; then
        scenario_fail "Expected relay_mode '$EXPECT_RELAY_MODE', got '$mode'" "=== Info response: $info ==="
    fi

    build=$(echo "$info" | jq -r '.build_version')
    if [ -z "$build" ] || [ "$build" = "null" ]; then
        scenario_fail "Expected a build_version, got '$build'" "=== Info response: $info ==="
    fi

    # The compose file sets REMOTE_LOGS_VISIBLE, so /info must advertise the
    # logs endpoint; the backend reads this before it asks for logs.
    local remote_logs
    remote_logs=$(echo "$info" | jq -r '.remote_logs')
    if [ "$remote_logs" != "true" ]; then
        scenario_fail "Expected remote_logs true, got '$remote_logs'" "=== Info response: $info ==="
    fi

    echo "Success: agent reports integration, relay_mode=$mode, build_version=$build, remote_logs=$remote_logs"
}

# Pages the agent's captured logs back through the relay. The query string
# has to survive the transport, page 0 has to be the newest block, and a
# pinned now has to make page 1 disjoint from page 0.
scenario_remote_logs() {
    echo "Checking remote logs paging..."
    local p0 p1 next count sorted
    p0=$(curlw "$DISPATCH_URL/__axon/logs?size=3")

    count=$(echo "$p0" | jq -r '.lines | length')
    if [ "$count" != "3" ]; then
        scenario_fail "Expected a full first page of 3 captured log lines, got '$count'" "=== Page 0: $p0 ==="
    fi
    # Startup alone logs more than three lines, so there must be a continuation.
    next=$(echo "$p0" | jq -r '.next // empty')
    if [ -z "$next" ]; then
        scenario_fail "Expected a continuation token on the first page" "=== Page 0: $p0 ==="
    fi

    # Each line is a JSON document and the page is chronological.
    sorted=$(echo "$p0" | jq -r '[.lines[] | fromjson | .ts] | . == sort')
    if [ "$sorted" != "true" ]; then
        scenario_fail "Expected lines in chronological order" "=== Page 0: $p0 ==="
    fi

    p1=$(curlw "$DISPATCH_URL/__axon/logs?size=3&next=$next")
    if [ "$(echo "$p1" | jq -r '.lines | length')" != "3" ]; then
        scenario_fail "Expected a full second page via the continuation token" "=== Page 1: $p1 ==="
    fi

    local overlap
    overlap=$(comm -12 <(echo "$p0" | jq -r '.lines[]' | sort) <(echo "$p1" | jq -r '.lines[]' | sort))
    if [ -n "$overlap" ]; then
        scenario_fail "Pages 0 and 1 overlap" "=== Overlap: $overlap ==="
    fi

    # The second page is older than the first.
    local p0_first p1_last
    p0_first=$(echo "$p0" | jq -r '.lines[0] | fromjson | .ts')
    p1_last=$(echo "$p1" | jq -r '.lines[-1] | fromjson | .ts')
    if [ "$(jq -n --argjson a "$p1_last" --argjson b "$p0_first" '$a <= $b')" != "true" ]; then
        scenario_fail "Expected the second page to be older than the first" "p0 first ts: $p0_first" "p1 last ts: $p1_last"
    fi

    echo "Success: remote logs page back through the relay with a continuation token"
}

# A plain relayed GET: python-server serves /tmp, so a file written here must
# come back byte-for-byte through the transport.
scenario_broker_passthrough() {
    echo "Checking relay broker passthrough..."
    local filename result
    filename="token-$(date +%s)"
    echo "$TOKEN" > "/tmp/$filename"
    echo "$TOKEN" > /tmp/axon-test-token

    result=$(curlw "$DISPATCH_URL/$filename")
    if [ "$result" != "$TOKEN" ]; then
        scenario_fail "Expected '$TOKEN', got '$result'"
    fi
    echo "Success: text passthrough"
}

# A payload large enough to be split across frames in either transport, so
# chunking and reassembly are covered rather than assumed. Compared by
# checksum: a corrupted byte anywhere fails.
#
# $1 optional extra curl args (snyk-broker needs a response-mode header).
scenario_binary_passthrough() {
    echo "Checking binary file relay passthrough..."
    local name original downloaded orig_sum dl_sum
    name="binary-test-$(date +%s).bin"
    dd if=/dev/urandom of="/tmp/$name" bs=1024 count=1536 2>/dev/null
    original="/tmp/$name"
    downloaded="/tmp/${name}.downloaded"
    orig_sum=$(sha256sum "$original" | awk '{print $1}')

    # Direct curl, not curlw: shell variables corrupt binary data.
    if ! curl -s -f "$@" -o "$downloaded" "$DISPATCH_URL/$name"; then
        scenario_fail "curl failed to download the binary file"
    fi

    dl_sum=$(sha256sum "$downloaded" | awk '{print $1}')
    if [ "$orig_sum" != "$dl_sum" ]; then
        scenario_fail "Binary checksum mismatch" \
            "  original:   $orig_sum ($(wc -c < "$original") bytes)" \
            "  downloaded: $dl_sum ($(wc -c < "$downloaded") bytes)"
    fi
    echo "Success: 1.5MB binary checksum verified ($orig_sum)"
}

# The /gitlab/* rule in accept-client.json carries:
#   auth: { scheme: basic, username: ${GITLAB_USERNAME:oauth2}, password: ${GITLAB_TOKEN} }
# GITLAB_USERNAME is unset, so the username falls back to "oauth2" and the
# relay must inject Basic base64(oauth2:gitlab-test-token) toward the echo
# origin, which reflects headers back. This is the injection the git-over-HTTP
# clone path depends on, and no unit test on the accept file's shape can
# demonstrate it actually reaches the wire.
scenario_gitlab_basic_auth() {
    echo "Checking GitLab scaffolder Basic-auth injection..."
    local result expected
    result=$(curlw "$DISPATCH_URL/gitlab/myrepo.git/info/refs?service=git-upload-pack")
    expected="Basic $(printf '%s' 'oauth2:gitlab-test-token' | base64)"

    if ! echo "$result" | grep -qF "$expected"; then
        scenario_fail "Injected GitLab Basic auth ($expected) not found in echoed headers" "$result"
    fi
    echo "Success: accept rule injected Basic auth on the git smart-HTTP path"
}

# Fetches a real HTTPS origin, exercising TLS and — in the proxied topology —
# the proxy and its CA. Leaves the response in HTTPS_RESULT for the proxy
# assertions, which read headers off this same request.
scenario_https_relay() {
    echo "Checking HTTPS relay (GitHub README)..."
    if ! HTTPS_RESULT=$(curlw -f -v "$DISPATCH_URL/cortexapps/axon/refs/heads/main/README.md" 2>&1); then
        scenario_fail "Expected to read the axon README over HTTPS" "$HTTPS_RESULT"
    fi
    echo "Success: HTTPS relay"
}

# In the proxied topology the agent is network-isolated from the origin, so
# traffic that did not go through the proxy could not have succeeded at all.
# These headers prove it took the intended path.
scenario_proxy_headers() {
    echo "Checking relay HTTP_PROXY config..."
    if ! echo "$HTTPS_RESULT" | grep -qi "x-proxy-mitmproxy"; then
        scenario_fail "Expected 'x-proxy-mitmproxy' header, got none" "$HTTPS_RESULT"
    fi
    echo "Success: found 'x-proxy-mitmproxy' header"

    if ! echo "$HTTPS_RESULT" | grep -qi "x-axon-relay-instance:"; then
        scenario_fail "Expected 'x-axon-relay-instance' header, got none" "$HTTPS_RESULT"
    fi
    echo "Success: found 'x-axon-relay-instance' header"
}

# Header injection from both the accept file and a plugin, observed through
# the echo origin.
scenario_injected_headers() {
    echo "Checking echo endpoint with injected headers..."
    local result
    if ! result=$(curlw -f -v "$DISPATCH_URL/echo/foobar" 2>&1); then
        scenario_fail "Expected to echo 'foobar', got an error" "$result"
    fi

    if ! echo "$result" | grep -q "added-fake-server"; then
        scenario_fail "Expected injected header value, not found" "$result"
    fi
    echo "Success: found injected header value"

    if ! echo "$result" | grep -q "HOME=/root"; then
        scenario_fail "Expected injected plugin header value, not found" "$result"
    fi
    echo "Success: found injected plugin header value"
}

# The negative of scenario_proxy_headers: with no proxy configured, nothing
# should be stamping proxy headers on the response.
scenario_no_proxy_headers() {
    echo "Checking relay non-proxy config..."
    if echo "$HTTPS_RESULT" | grep -qi "x-proxy-mitmproxy"; then
        scenario_fail "Expected no 'x-proxy-mitmproxy' header, got one" "$HTTPS_RESULT"
    fi
    echo "Success: no proxy header present, as expected"
}

# The full shared suite, in dependency order: prove the agent is up, then
# relaying, then that rules and credentials survive the trip.
#
# $1 optional extra curl args for the binary download.
run_shared_scenarios() {
    scenario_axon_endpoints
    scenario_remote_logs
    scenario_broker_passthrough
    scenario_binary_passthrough "$@"
    scenario_gitlab_basic_auth
    scenario_https_relay

    if [ "$PROXY" == "1" ]; then
        scenario_proxy_headers
        scenario_injected_headers
    else
        scenario_no_proxy_headers
    fi
}

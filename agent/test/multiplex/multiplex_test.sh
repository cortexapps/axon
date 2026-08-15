#!/bin/bash
# Spike: can one ingress serve both relay transports?
#
# Two agents on different tokens, one per transport, both told the same
# BROKER_SERVER_URL — nginx. The question is whether nginx can tell their
# connections apart well enough that traffic aimed at either token reaches
# the right agent and comes back.
#
# The proof is identity, not just success: each agent reports a distinct
# alias and stamps a distinct x-axon-relay-instance, so a response says which
# agent produced it. A misrouted request would still return 200 from the
# wrong agent, and only the identity check would catch it.
set -u

cd "$(dirname "$0")" || exit 1

export NGINX_PORT=${NGINX_PORT:-58480}
export SNYK_PORT=${SNYK_PORT:-58481}
export GRPC_HTTP_PORT=${GRPC_HTTP_PORT:-58482}

SNYK_TOKEN=tok-snyk
GRPC_TOKEN=tok-grpc

COMPOSE="docker compose"
FAILED=0

cleanup() {
    echo
    echo "=== nginx access log (what each transport actually asked for) ==="
    $COMPOSE logs nginx 2>&1 | grep -E 'proto=|upstream=' | tail -30
    echo
    echo "Cleanup: stopping stack"
    $COMPOSE down -v --remove-orphans >/dev/null 2>&1
    rm -f /tmp/mux-*.txt
}
trap cleanup EXIT

fail() {
    echo "FAIL: $*"
    FAILED=1
}

echo "=== Building and starting stack ==="
if ! $COMPOSE up -d --build; then
    echo "FAIL: compose up failed"
    $COMPOSE logs --tail 50
    exit 1
fi

# Both agents must be registered before any assertion about routing means
# anything: an unconnected token looks identical to a misrouted one.
echo "=== Waiting for both agents to connect through nginx ==="
COUNTER=60
while [ $COUNTER -gt 0 ]; do
    snyk_up=0
    grpc_up=0

    # snyk-broker reports its connected clients on /healthcheck.
    if curl -sf --max-time 3 "http://localhost:$SNYK_PORT/healthcheck" >/dev/null 2>&1; then
        snyk_up=1
    fi
    # The tunnel server reports stream count on /healthz.
    streams=$(curl -sf --max-time 3 "http://localhost:$GRPC_HTTP_PORT/healthz" 2>/dev/null \
        | sed -n 's/.*"streams":\([0-9]*\).*/\1/p')
    [ -n "$streams" ] && [ "$streams" -gt 0 ] 2>/dev/null && grpc_up=1

    if [ "$snyk_up" = 1 ] && [ "$grpc_up" = 1 ]; then
        echo "Both transports are up (grpc streams: $streams)"
        break
    fi
    sleep 2
    COUNTER=$((COUNTER-1))
done

if [ $COUNTER -eq 0 ]; then
    echo "FAIL: agents did not both connect in time (snyk=$snyk_up grpc=$grpc_up)"
    $COMPOSE logs --tail 60 agent-snyk agent-grpc nginx
    exit 1
fi

# ---------------------------------------------------------------------------
# 1. Each token reaches its own agent, addressed through nginx.
# ---------------------------------------------------------------------------
echo
echo "=== Routing by token, through nginx ==="

check_alias() {
    local label="$1" base="$2" token="$3" want="$4"
    local body alias
    body=$(curl -sf --max-time 10 "$base/broker/$token/__axon/info" 2>&1)
    if [ -z "$body" ]; then
        fail "$label: no response for $token"
        return
    fi
    alias=$(echo "$body" | sed -n 's/.*"alias":"\([^"]*\)".*/\1/p')
    if [ "$alias" != "$want" ]; then
        fail "$label: $token reached alias '$alias', wanted '$want'"
        echo "  body: $body"
        return
    fi
    echo "OK  $label: $token -> $alias"
}

check_alias "via nginx  " "http://localhost:$NGINX_PORT" "$SNYK_TOKEN" "mux-snyk"
check_alias "via nginx  " "http://localhost:$NGINX_PORT" "$GRPC_TOKEN" "mux-grpc"

# Same assertions straight at each backend. If these pass and the nginx ones
# fail, the demux is at fault rather than the transports.
check_alias "direct     " "http://localhost:$SNYK_PORT" "$SNYK_TOKEN" "mux-snyk"
check_alias "direct     " "http://localhost:$GRPC_HTTP_PORT" "$GRPC_TOKEN" "mux-grpc"

# ---------------------------------------------------------------------------
# 2. Real payloads travel the full path and come back intact.
# ---------------------------------------------------------------------------
echo
echo "=== Relaying content through each transport ==="

check_fetch() {
    local label="$1" base="$2" token="$3" want_instance="$4"
    local name content out body instance
    name="mux-$token-$(date +%s%N).txt"
    content="payload-for-$token"
    echo "$content" > "/tmp/$name"

    out=$(curl -sf --max-time 20 -D - "$base/broker/$token/$name" 2>&1)
    body=$(echo "$out" | tail -1 | tr -d '\r')
    if [ "$body" != "$content" ]; then
        fail "$label: $token returned '$body', wanted '$content'"
        return
    fi

    # Which agent actually served it. Both transports stamp this header, so a
    # cross-routed response is visible rather than plausible.
    instance=$(echo "$out" | grep -i '^x-axon-relay-instance:' | tr -d '\r' | awk '{print $2}')
    if [ "$instance" != "$want_instance" ]; then
        fail "$label: $token served by '$instance', wanted '$want_instance'"
        return
    fi
    echo "OK  $label: $token relayed by $instance"
}

check_fetch "via nginx  " "http://localhost:$NGINX_PORT" "$SNYK_TOKEN" "agent-snyk"
check_fetch "via nginx  " "http://localhost:$NGINX_PORT" "$GRPC_TOKEN" "agent-grpc"

# ---------------------------------------------------------------------------
# 3. Neither transport can answer for the other's token.
# ---------------------------------------------------------------------------
# The strongest evidence the demux is real: routing is not accidentally
# working because both backends happen to reach both agents.
echo
echo "=== Cross-token isolation ==="

cross_check() {
    local label="$1" base="$2" token="$3"
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$base/broker/$token/__axon/info" 2>&1)
    if [ "$code" = "200" ]; then
        fail "$label: $token answered by the wrong backend (200)"
        return
    fi
    echo "OK  $label: $token refused by the other backend (HTTP $code)"
}

cross_check "snyk backend" "http://localhost:$SNYK_PORT" "$GRPC_TOKEN"
cross_check "grpc backend" "http://localhost:$GRPC_HTTP_PORT" "$SNYK_TOKEN"

echo
if [ "$FAILED" = "0" ]; then
    echo "MULTIPLEX TEST PASSED: one ingress served both transports"
else
    echo "MULTIPLEX TEST FAILED"
fi
exit $FAILED

#!/bin/bash
set -e

# End-to-end test for the gRPC tunnel relay stack.
# This mirrors relay_test.sh but uses the gRPC tunnel server instead of snyk-broker.
#
# Components:
#   - [server-side] grpc-tunnel-server: gRPC tunnel server with HTTP dispatch endpoint
#   - [server-side] cortex-fake: mimics the Cortex registration API
#   - [client-side] axon-relay: agent in gRPC tunnel mode (RELAY_MODE=grpc-tunnel)
#   - [client-side] python-server: mimics an API that Cortex is calling out to
#   - [optional] mitmproxy: HTTP proxy for proxy-mode testing

export TOKEN=0e481b34-76ac-481a-a92f-c94a6cf6f6c1
export GRPC_PORT=50152

if [ "$PROXY" == "1" ]; then
    echo "TESTING WITH PROXY"
    export ENVFILE=proxy.env
    export HTTP_PORT=58180
else
    echo "TESTING WITHOUT PROXY"
    export ENVFILE=noproxy.env
    export HTTP_PORT=58180
fi

COMPOSE="docker compose -f docker-compose.grpc.yml"

function cleanup {
    echo "Cleanup: Stopping docker-compose"
    $COMPOSE down
    rm -f /tmp/token-* /tmp/axon-test-token /tmp/binary-test-*.bin /tmp/binary-test-*.downloaded
}
trap cleanup EXIT

echo "Starting docker compose (gRPC tunnel)..."
$COMPOSE up -d
sleep 5

function get_container_status {
    result_status=$(docker inspect -f '{{.State.Status}}' $1)
    echo "Status $1 = $result_status" >&2
    echo $result_status
}

if [ -n "$DEBUG" ]; then
    echo "Debug mode enabled, sleeping indefinitely"
    while true; do
        sleep 5
    done
fi

COUNTER=30
SERVER_STATUS=$(get_container_status relay-grpc-tunnel-server-1)
AXON_STATUS=$(get_container_status relay-axon-relay-1)

while [ "$SERVER_STATUS" != "running" ] || [ "$AXON_STATUS" != "running" ]; do
    if [ $COUNTER -eq 0 ]; then
        echo "Containers did not start in time"
        $COMPOSE logs
        exit 1
    fi

    echo "Waiting for containers to start"
    sleep 1
    SERVER_STATUS=$(get_container_status relay-grpc-tunnel-server-1)
    AXON_STATUS=$(get_container_status relay-axon-relay-1)
    COUNTER=$((COUNTER-1))
done

# Wait for grpc-tunnel-server healthz (exposed to host via HTTP_PORT).
echo "Waiting for grpc-tunnel-server healthz..."
COUNTER=30
while ! curl -sf http://localhost:$HTTP_PORT/healthz > /dev/null 2>&1; do
    if [ $COUNTER -eq 0 ]; then
        echo "grpc-tunnel-server healthz did not pass in time"
        $COMPOSE logs grpc-tunnel-server
        exit 1
    fi
    sleep 1
    COUNTER=$((COUNTER-1))
done
echo "grpc-tunnel-server is healthy"

# Wait for at least one tunnel stream to register (agent connects via gRPC).
echo "Waiting for tunnel stream registration..."
COUNTER=60
while true; do
    HEALTH=$(curl -sf http://localhost:$HTTP_PORT/healthz 2>/dev/null || echo '{}')
    STREAMS=$(echo "$HEALTH" | grep -o '"streams":[0-9]*' | grep -o '[0-9]*' || echo "0")
    if [ "$STREAMS" -gt 0 ]; then
        echo "Tunnel has $STREAMS active stream(s)"
        break
    fi
    if [ $COUNTER -eq 0 ]; then
        echo "No tunnel streams registered in time"
        echo "Server health: $HEALTH"
        $COMPOSE logs
        exit 1
    fi
    sleep 1
    COUNTER=$((COUNTER-1))
done

real_curl=$(which curl)

function curlw {
    [ -n "$DEBUG" ] && echo "Executing: $real_curl $@" >&2
    if ! curl_result=$($real_curl -s "$@" 2>&1); then
        echo "Curl command failed: $@ ==> $curl_result"
        exit 1
    else
        [ -n "$DEBUG" ] && echo "curl $@ ==> $curl_result" >&2
    fi
    echo "$curl_result"
}

# Dispatch URL: grpc-tunnel-server HTTP port at /broker/{token}/{path}
DISPATCH_URL="http://localhost:$HTTP_PORT/broker/$TOKEN"

echo "Checking relay broker passthrough..."
# Test relay of a text file through the gRPC tunnel.
# python-server serves files from /tmp.
FILENAME="token-$(date +%s)"
echo "$TOKEN" > /tmp/$FILENAME
echo "$TOKEN" > /tmp/axon-test-token
result=$(curlw $DISPATCH_URL/$FILENAME)

if [ "$result" != "$TOKEN" ]; then
    echo "FAIL: Expected $TOKEN, got $result"
    $COMPOSE logs
    exit 1
fi
echo "Success: Text file relay through gRPC tunnel"

echo "Checking binary file relay passthrough..."
BINARY_FILENAME="binary-test-$(date +%s).bin"
dd if=/dev/urandom of="/tmp/$BINARY_FILENAME" bs=1024 count=1536 2>/dev/null
ORIGINAL_CHECKSUM=$(sha256sum "/tmp/$BINARY_FILENAME" | awk '{print $1}')

BINARY_DOWNLOAD="/tmp/${BINARY_FILENAME}.downloaded"
curl -s -f -o "$BINARY_DOWNLOAD" "$DISPATCH_URL/$BINARY_FILENAME"
DOWNLOAD_STATUS=$?
if [ $DOWNLOAD_STATUS -ne 0 ]; then
    echo "FAIL: curl failed to download binary file (exit code $DOWNLOAD_STATUS)"
    $COMPOSE logs
    exit 1
fi

DOWNLOADED_CHECKSUM=$(sha256sum "$BINARY_DOWNLOAD" | awk '{print $1}')
if [ "$ORIGINAL_CHECKSUM" != "$DOWNLOADED_CHECKSUM" ]; then
    echo "FAIL: Binary checksum mismatch"
    echo "  Original:   $ORIGINAL_CHECKSUM ($(wc -c < /tmp/$BINARY_FILENAME) bytes)"
    echo "  Downloaded: $DOWNLOADED_CHECKSUM ($(wc -c < $BINARY_DOWNLOAD) bytes)"
    exit 1
else
    echo "Success: Binary file (1.5MB) checksum verified ($ORIGINAL_CHECKSUM)"
fi

# Validate HTTPS relay by fetching the Axon README from GitHub.
echo "Checking HTTPS relay (GitHub README)..."
if ! proxy_result=$(curlw -f -v $DISPATCH_URL/cortexapps/axon/refs/heads/main/README.md 2>&1); then
    echo "FAIL: Expected to be able to read the axon readme from GitHub, but got error"
    echo "$proxy_result"
    $COMPOSE logs
    exit 1
fi
echo "Success: HTTPS relay through gRPC tunnel"

if [ "$PROXY" == "1" ]; then
    echo "Checking relay HTTP_PROXY config..."
    if ! echo "$proxy_result" | grep -i "x-proxy-mitmproxy"; then
        echo "FAIL: Expected 'x-proxy-mitmproxy' header, got nothing"
        exit 1
    else
        echo "Success: Found 'x-proxy-mitmproxy' header"
    fi

    echo "Checking echo endpoint with injected headers..."
    if ! proxy_result=$(curlw -f -v $DISPATCH_URL/echo/foobar 2>&1); then
        echo "FAIL: Expected to echo 'foobar' via the proxy, but got error"
        echo "$proxy_result"
        exit 1
    fi

    if ! echo "$proxy_result" | grep -q "added-fake-server"; then
        echo "FAIL: Expected injected header value but not found"
        echo "$proxy_result"
        exit 1
    else
        echo "Success: Found expected injected header value in result"
    fi

    if ! echo "$proxy_result" | grep -q "HOME=/root"; then
        echo "FAIL: Expected injected plugin header value but not found"
        echo "$proxy_result"
        exit 1
    else
        echo "Success: Found expected injected plugin header value in result"
    fi

    # Verify gRPC tunnel streams are active (replaces WebSocket tunnel check).
    echo "Checking gRPC tunnel streams..."
    axon_logs=$($COMPOSE logs axon-relay 2>&1)
    if ! echo "$axon_logs" | grep -q "Tunnel stream established"; then
        echo "FAIL: Expected 'Tunnel stream established' in agent logs but not found"
        echo "=== Axon Relay Logs (last 50) ==="
        echo "$axon_logs" | tail -50
        exit 1
    else
        echo "Success: gRPC tunnel stream established"
    fi
else
    echo "Checking relay non-proxy config..."
    if echo "$proxy_result" | grep -i "x-proxy-mitmproxy"; then
        echo "FAIL: Expected no 'x-proxy-mitmproxy' header, got one"
        exit 1
    else
        echo "Success: Did not find 'x-proxy-mitmproxy' header (as expected)"
    fi
fi

echo "Success! gRPC tunnel e2e test passed!"

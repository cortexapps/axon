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
# Host ports are kept BELOW the ephemeral range (32768-60999 on Linux,
# 49152-65535 on macOS). The test opens many outbound connections, and one of
# them transiently claiming a listener's port as its source port makes the
# next `compose up` fail with "address already in use". load_test.sh hit this
# and pins its ports the same way.
export GRPC_PORT=17152
export HTTP_PORT=17180

if [ "$PROXY" == "1" ]; then
    echo "TESTING WITH PROXY"
    export ENVFILE=proxy.env
    # Base docker-compose.grpc.yml has axon-relay on internal network only
    # This enforces that gRPC connections MUST go through mitmproxy
    export COMPOSE_FILES="-f docker-compose.grpc.yml"
else
    echo "TESTING WITHOUT PROXY"
    export ENVFILE=noproxy.env
    # Add external network so axon-relay can connect directly to grpc-tunnel-server
    export COMPOSE_FILES="-f docker-compose.grpc.yml -f docker-compose.grpc.noproxy.yml"

    # Also set the HTTP_PORT to a different value to ensure we respect that port
    export HTTP_PORT=17280
fi

function cleanup {
    echo "Cleanup: Stopping docker-compose"
    docker compose $COMPOSE_FILES down
    rm -f /tmp/token-* /tmp/axon-test-token /tmp/binary-test-*.bin /tmp/binary-test-*.downloaded
}
trap cleanup EXIT

echo "Starting docker compose (gRPC tunnel)..."
docker compose $COMPOSE_FILES up -d
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
        docker compose $COMPOSE_FILES logs
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
        docker compose $COMPOSE_FILES logs grpc-tunnel-server
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
        docker compose $COMPOSE_FILES logs
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

# Ingress for this transport: the tunnel server's HTTP dispatch port.
# Everything the shared scenarios assert is addressed relative to it, so the
# same checks run here as against snyk-broker.
export DISPATCH_URL="http://localhost:$HTTP_PORT/broker/$TOKEN"
export EXPECT_RELAY_MODE=grpc-tunnel
source ./relay_scenarios.sh

run_shared_scenarios

if [ "$PROXY" == "1" ]; then
    # The point of the proxied topology for this transport: the agent's
    # OUTBOUND gRPC dial has to work through an HTTP proxy, via CONNECT.
    # That is the deployment shape most customers actually have, and it is
    # the one thing snyk-broker's websocket path cannot tell us anything
    # about.
    #
    # Network isolation already makes it structurally true — axon-relay is on
    # the internal network only, so it cannot reach the tunnel server except
    # through mitmproxy. But asserting only on "a stream appeared" would
    # report a proxy regression as a confusing connection timeout. These two
    # checks separate the questions: did we dial through the proxy, and did
    # the tunnel then work.
    echo "Checking gRPC outbound connection through the HTTP proxy..."
    axon_logs=$(docker compose $COMPOSE_FILES logs axon-relay 2>&1)

    if ! echo "$axon_logs" | grep -q "gRPC connection dialed through HTTP proxy"; then
        echo "FAIL: Agent did not route its gRPC dial through the HTTP proxy"
        echo "  Expected 'gRPC connection dialed through HTTP proxy' in the agent logs."
        echo "  Without it, any stream below connected by some other path."
        echo "=== Axon Relay Logs (last 50) ==="
        echo "$axon_logs" | tail -50
        exit 1
    fi
    echo "Success: gRPC dial routed through the HTTP proxy (CONNECT)"

    if ! echo "$axon_logs" | grep -q "Tunnel stream established"; then
        echo "FAIL: Expected 'Tunnel stream established' in agent logs but not found"
        echo "=== Axon Relay Logs (last 50) ==="
        echo "$axon_logs" | tail -50
        exit 1
    fi
    echo "Success: gRPC tunnel stream established over the proxied connection"
fi

echo "=== gRPC tunnel reconnection after SIGKILL ==="

# Force-kill the grpc-tunnel-server container to simulate a non-graceful disconnect.
# This tears down the TCP connection without sending a gRPC GoAway frame, which
# is what happens when the tunnel server crashes or the network drops.
echo "Force-killing grpc-tunnel-server container..."
docker kill --signal=KILL relay-grpc-tunnel-server-1

# Wait for the container to be fully dead
sleep 2
SERVER_STATUS=$(get_container_status relay-grpc-tunnel-server-1)
if [ "$SERVER_STATUS" == "running" ]; then
    echo "FAIL: grpc-tunnel-server should be dead after SIGKILL"
    exit 1
fi
echo "grpc-tunnel-server is stopped (status=$SERVER_STATUS)"

# Restart the tunnel server
echo "Restarting grpc-tunnel-server container..."
docker compose $COMPOSE_FILES up -d grpc-tunnel-server

# Wait for the tunnel server to be healthy again
COUNTER=30
while [ $COUNTER -gt 0 ]; do
    SERVER_STATUS=$(get_container_status relay-grpc-tunnel-server-1)
    if [ "$SERVER_STATUS" == "running" ]; then
        if curl -s -f http://localhost:$HTTP_PORT/healthz > /dev/null 2>&1; then
            break
        fi
    fi
    echo "Waiting for grpc-tunnel-server to be healthy ($COUNTER)..."
    sleep 1
    COUNTER=$((COUNTER-1))
done

if [ $COUNTER -eq 0 ]; then
    echo "FAIL: grpc-tunnel-server did not become healthy in time"
    docker compose $COMPOSE_FILES logs grpc-tunnel-server
    exit 1
fi
echo "grpc-tunnel-server is back up"

# Give the axon relay time to detect the disconnect and reconnect.
# The gRPC tunnel client uses exponential backoff so 15s should be enough
# for the first reconnect attempt.
echo "Waiting for axon relay to reconnect..."
COUNTER=60
while [ $COUNTER -gt 0 ]; do
    HEALTH=$(curl -sf http://localhost:$HTTP_PORT/healthz 2>/dev/null || echo '{}')
    STREAMS=$(echo "$HEALTH" | grep -o '"streams":[0-9]*' | grep -o '[0-9]*' || echo "0")
    if [ "$STREAMS" -gt 0 ]; then
        break
    fi
    echo "Waiting for tunnel stream re-establishment ($COUNTER)..."
    sleep 1
    COUNTER=$((COUNTER-1))
done

if [ $COUNTER -eq 0 ]; then
    echo "FAIL: Tunnel stream did not re-establish in time"
    docker compose $COMPOSE_FILES logs --tail=80 axon-relay
    exit 1
fi
echo "Tunnel re-established with $STREAMS active stream(s)"

# Now verify the relay is working again by sending a request through the tunnel
FILENAME="token-reconnect-$(date +%s)"
echo "$TOKEN" > /tmp/$FILENAME
result=$(curlw $DISPATCH_URL/$FILENAME)

if [ "$result" != "$TOKEN" ]; then
    echo "FAIL: Expected $TOKEN after reconnect, got '$result'"
    echo "=== Axon Relay Logs (last 80) ==="
    docker compose $COMPOSE_FILES logs --tail=80 axon-relay
    exit 1
fi
echo "Success: gRPC tunnel passthrough works after SIGKILL + restart"

echo "Success! gRPC tunnel e2e test passed!"

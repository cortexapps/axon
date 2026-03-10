#! /bin/bash
set -e

# This is a full end-to-end test of the relay broker system that does the following:
# 1. Setups a a docker compose with the these components
#    - [server-side] snyk broker, which mimics to broker that lives on the cortex side
#    - [server-side] a go webserver that mimics the cortex registration API
#    - [client-side] A axon relay, which an instance of relay in customer's data enviroment
#    - [client-side] a python webserver that mimics an API that cortex is calling out to
# 2. Starts the server-side components to mimic the cortex API
# 3. Starts the client-side components to mimic the customer's data enviroment
# 4. Sends a request to the snyk broker, which should end up calling the python API
#
# The python API simply reads a file that we write to /tmp which includes the token
# Once this is validated, the test is considered successful and the docker-compose is stopped

# Start a snyk broker, capture the ID
export TOKEN=0e481b34-76ac-481a-a92f-c94a6cf6f6c1
export SERVER_PORT=57341

if [ "$PROXY" == "1" ]
then
    echo "TESTING WITH PROXY"
    export ENVFILE=proxy.env
    # Base docker-compose.yml has axon-relay on internal network only
    # This enforces that WebSocket connections MUST go through mitmproxy
    # docker-compose.proxy.yml enables HTTPS broker connection through proxy (TLS-through-proxy)
    export COMPOSE_FILES="-f docker-compose.yml -f docker-compose.proxy.yml"

    # Generate TLS certificates if needed (for CI environments)
    ./generate-certs.sh
else
    echo "TESTING WITHOUT PROXY"
    export ENVFILE=noproxy.env
    # Add external network so axon-relay can connect directly to snyk-broker
    export COMPOSE_FILES="-f docker-compose.yml -f docker-compose.noproxy.yml"

    # just for fun also set the HTTP_PORT to a different value to ensure
    # we respect that port as well
    export HTTP_PORT=58080
fi

# Create an exit trap to stop the broker when the script exits
function cleanup {
  echo "Cleanup: Stopping docker-compose"
  docker compose $COMPOSE_FILES down
  rm -f /tmp/token-* /tmp/axon-test-token /tmp/binary-test-*.bin /tmp/binary-test-*.downloaded
}
trap cleanup EXIT

echo "Starting docker compose ..."
docker compose $COMPOSE_FILES up -d
sleep 5

# Loop until healthcheck for server broker passes
function get_container_status {
    result_status=$(docker inspect -f '{{.State.Status}}' $1)
    echo "Status $1 = $result_status" >&2
    echo $result_status
}

# Now we send a request to the server broker, and expect it to be relayed to the axon relay out
# to a local http port

if [ -n "$DEBUG" ]
then
  echo "Debug mode enabled, sleeping indefinitely"
  while true
  do
      sleep 5
  done
fi

COUNTER=30
SNYK_STATUS=$(get_container_status relay-snyk-broker-1)
AXON_STATUS=$(get_container_status relay-axon-relay-1)
    
while [ "$SNYK_STATUS" != "running" ] || [ "$AXON_STATUS" != "running" ]; do
    
    if [ $COUNTER -eq 0 ]; then
        echo "Containers did not start in time"
        docker compose $COMPOSE_FILES logs
        exit 1
    fi
    
    echo "Waiting for containers to start"
    sleep 1
    SNYK_STATUS=$(get_container_status relay-snyk-broker-1)
    AXON_STATUS=$(get_container_status relay-axon-relay-1)
    COUNTER=$((COUNTER-1))
done

real_curl=$(which curl)

function curlw {
    [ -n "$DEBUG" ] && echo "Executing: $real_curl $@" >&2
    if ! curl_result=$($real_curl -s -H "x-broker-ws-response: true" "$@" 2>&1)
    then
        echo "Curl command failed: $@ ==> $curl_result"
        exit 1
    else 
        [ -n "$DEBUG" ] && echo "curl $@ ==> $curl_result" >&2
    fi
    echo "$curl_result"
}

echo "Checking axon endpoints..."
# First make sure we can call the status endpoint which is implemented by 
# the agent, so this is localhost right at the agent

info_result=$(curlw http://localhost:$SERVER_PORT/broker/$TOKEN/__axon/info)
result=$(echo "$info_result" | jq -r '.alias')
if [ "$result" != "axon-test" ]; then
    echo "FAIL: Expected alias 'axon-relay', got '$result'"
    exit 1
fi
result=$(echo "$info_result" | jq -r '.integration')
if [ "$result" != "github" ]; then
    echo "FAIL: Expected integration type 'github', got '$result'"
    exit 1
fi


echo "Checking relay broker passthrough..."
# Now we check that we can invoke a local service via the broker.  In this case
# its just a python -m http.server that we run in the docker-compose file against /tmp
#
# The file we look for is at /tmp/token, mounted into the python-server container, which
# is a fake service that we use to test the relay broker
#
FILENAME="token-$(date +%s)"
echo "$TOKEN" > /tmp/$FILENAME
echo "$TOKEN" > /tmp/axon-test-token
result=$(curlw http://localhost:$SERVER_PORT/broker/$TOKEN/$FILENAME)

if [ "$result" != "$TOKEN" ]; then
    echo "FAIL: Expected $TOKEN, got $result"
    exit 1
fi

echo "Checking binary file relay passthrough..."
BINARY_FILENAME="binary-test-$(date +%s).bin"
dd if=/dev/urandom of="/tmp/$BINARY_FILENAME" bs=1024 count=1024 2>/dev/null
ORIGINAL_CHECKSUM=$(sha256sum "/tmp/$BINARY_FILENAME" | awk '{print $1}')

# Must use curl -o directly (not curlw) — shell variables corrupt binary data
BINARY_DOWNLOAD="/tmp/${BINARY_FILENAME}.downloaded"
curl -s -f -H "x-broker-ws-response: true" -o "$BINARY_DOWNLOAD" "http://localhost:$SERVER_PORT/broker/$TOKEN/$BINARY_FILENAME"
DOWNLOAD_STATUS=$?
if [ $DOWNLOAD_STATUS -ne 0 ]; then
    echo "FAIL: curl failed to download binary file (exit code $DOWNLOAD_STATUS)"
    exit 1
fi

DOWNLOADED_CHECKSUM=$(sha256sum "$BINARY_DOWNLOAD" | awk '{print $1}')
if [ "$ORIGINAL_CHECKSUM" != "$DOWNLOADED_CHECKSUM" ]; then
    echo "FAIL: Binary checksum mismatch"
    echo "  Original:   $ORIGINAL_CHECKSUM ($(stat -c%s /tmp/$BINARY_FILENAME) bytes)"
    echo "  Downloaded: $DOWNLOADED_CHECKSUM ($(stat -c%s $BINARY_DOWNLOAD) bytes)"
    exit 1
else
    echo "Success: Binary file (1MB) checksum verified ($ORIGINAL_CHECKSUM)"
fi

# To validate this we call out to the AXON readme, which hits an HTTPS server
# so we validate proxy and cert handling
if ! proxy_result=$(curlw -f -v http://localhost:$SERVER_PORT/broker/$TOKEN/cortexapps/axon/refs/heads/main/README.md 2>&1)
then
    echo "FAIL: Expected to be able to read the axon readme from github, but got error"
    echo "$proxy_result"
    exit 1
fi

#
# Now we check that HTTP_PROXY and friends support works correctly, both with
# it configured (with a cert) and with it not
# 
if [ "$PROXY" == "1" ]; then
    echo "Checking relay HTTP_PROXY config..."
    if ! (echo "$proxy_result" | grep -i "x-proxy-mitmproxy")
    then
        echo "FAIL: Expected 'x-proxy-mitmproxy' header, got nothing"
        exit 1
    else
        echo "Success: Found 'x-proxy-mitmproxy' header"
    fi

    if ! (echo "$proxy_result" | grep -i "x-axon-relay-instance:")
    then
        echo "FAIL: Expected 'x-axon-relayinstance' header, got nothing"
        exit 1
    else
        echo "Success: Found 'x-axon-relayinstance' header"
    fi


    if ! proxy_result=$(curlw -f -v http://localhost:$SERVER_PORT/broker/$TOKEN/echo/foobar 2>&1)
    then
        echo "FAIL: Expected to echo 'foobar' via the proxy, but got error"
        echo "$proxy_result"
        exit 1
    fi
    # Make sure result has the header value
    if ! echo "$proxy_result" | grep -q "added-fake-server"; then
        echo "FAIL: Expected injected header value but not found"
        echo "$proxy_result"
        exit 1
    else
        echo "Success: Found expected injected header value in result"
    fi

    # Make sure the plugin header is also injected
    if ! echo "$proxy_result" | grep -q "HOME=/root"; then
        echo "FAIL: Expected injected plugin header value but not found"
        echo "$proxy_result"
        exit 1
    else
        echo "Success: Found expected injected plugin header value in result"
    fi

    # Verify that the reflector's raw WebSocket tunnel was established.
    # "WebSocket tunnel established" is logged by the reflector when a real Upgrade: websocket
    # request is received and a TCP tunnel is created (not just primus's application-layer WS).
    #
    # NOTE: WebSocket proxy usage is ENFORCED by network isolation (docker-compose.proxy.yml).
    # axon-relay is on the "internal" network only, snyk-broker is on "external" network only.
    # The only path is: axon-relay -> mitmproxy (bridges both) -> snyk-broker.
    # If WebSocket code bypasses the proxy, the connection FAILS at the network level.
    echo "Checking WebSocket tunnel..."
    axon_logs=$(docker compose $COMPOSE_FILES logs axon-relay 2>&1)

    if ! echo "$axon_logs" | grep -q "WebSocket tunnel established"; then
        echo "FAIL: Expected 'WebSocket tunnel established' in reflector logs but not found"
        echo "  The broker client may not be upgrading to raw WebSocket (staying on XHR polling)"
        echo "=== Axon Relay Logs (last 50) ==="
        echo "$axon_logs" | tail -50
        exit 1
    else
        echo "Success: WebSocket tunnel established through reflector"
    fi
else
    echo "Checking relay non proxy config..."
    if echo "$result" | grep -i "x-proxy-mitmproxy"
    then
        echo "FAIL: Expected no 'x-proxy-mitmproxy' header, got one"
        exit 1
    else
        echo "Success: Did not find 'x-proxy-mitm-proxy' header (as expected)"
    fi
fi

echo "=== Broker reconnection after SIGKILL ==="

# Force-kill the snyk-broker container to simulate a non-graceful disconnect.
# This tears down the TCP connection without sending a WS close frame, which
# is what happens when the broker server crashes or the network drops.
echo "Force-killing snyk-broker container..."
docker kill --signal=KILL relay-snyk-broker-1

# Wait for the container to be fully dead
sleep 2
BROKER_STATUS=$(get_container_status relay-snyk-broker-1)
if [ "$BROKER_STATUS" == "running" ]; then
    echo "FAIL: snyk-broker should be dead after SIGKILL"
    exit 1
fi
echo "snyk-broker is stopped (status=$BROKER_STATUS)"

# Restart the broker server
echo "Restarting snyk-broker container..."
docker compose up -d snyk-broker

# Wait for the broker to be healthy again
COUNTER=30
while [ $COUNTER -gt 0 ]; do
    BROKER_STATUS=$(get_container_status relay-snyk-broker-1)
    if [ "$BROKER_STATUS" == "running" ]; then
        # Also check the healthcheck endpoint
        if curl -s -f http://localhost:$SERVER_PORT/healthcheck > /dev/null 2>&1; then
            break
        fi
    fi
    echo "Waiting for snyk-broker to be healthy ($COUNTER)..."
    sleep 1
    COUNTER=$((COUNTER-1))
done

if [ $COUNTER -eq 0 ]; then
    echo "FAIL: snyk-broker did not become healthy in time"
    docker compose logs snyk-broker
    exit 1
fi
echo "snyk-broker is back up"

# Give the axon relay time to detect the disconnect and reconnect.
# The relay uses exponential backoff (5s, 10s, ...) so 15s should be enough
# for the first reconnect attempt.
echo "Waiting 15s for axon relay to reconnect..."
sleep 15

# Now verify the relay is working again by sending a request through the broker
FILENAME="token-reconnect-$(date +%s)"
echo "$TOKEN" > /tmp/$FILENAME
result=$(curlw http://localhost:$SERVER_PORT/broker/$TOKEN/$FILENAME)

if [ "$result" != "$TOKEN" ]; then
    echo "FAIL: Expected $TOKEN after reconnect, got '$result'"
    echo "=== Axon Relay Logs (last 80) ==="
    docker compose logs --tail=80 axon-relay
    exit 1
fi
echo "Success: Broker passthrough works after SIGKILL + restart"

echo "Success! Done!"
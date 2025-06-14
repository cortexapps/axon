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
else 
    echo "TESTING WITHOUT PROXY"
    export ENVFILE=noproxy.env
fi

# Create an exit trap to stop the broker when the script exits
function cleanup {

  echo "Cleanup: Stopping docker-compose"
  docker compose down
}
trap cleanup EXIT

echo "Starting docker compose ..."
docker compose up -d
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
        docker compose logsgi
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
    if ! curl_result=$($real_curl -s "$@" 2>&1)
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


# To validate this we call out to the AXON readme, which hits an HTTPS server
# so we validate proxy and cert handling
if ! result=$(curlw -f -v http://localhost:$SERVER_PORT/broker/$TOKEN/cortexapps/axon/refs/heads/main/README.md 2>&1)
then
    echo "FAIL: Expected to be able to read the axon readme from github, but got error"
    echo "$result"
    exit 1
fi

#
# Now we check that HTTP_PROXY and friends support works correctly, both with
# it configured (with a cert) and with it not
# 
if [ "$PROXY" == "1" ]; then
    echo "Checking relay HTTP_PROXY config..."
    if ! (echo "$result" | grep -i "x-proxy-mitmproxy")
    then
        echo "FAIL: Expected 'x-proxy-mitmproxy' header, got nothing"
        exit 1
    else
        echo "Success: Found 'x-proxy-mitmproxy' header"
    fi

    if ! (echo "$result" | grep -i "x-axon-relay-instance:")
    then
        echo "FAIL: Expected 'x-axon-relayinstance' header, got nothing"
        exit 1
    else
        echo "Success: Found 'x-axon-relayinstance' header"
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

echo "Success! Done!"
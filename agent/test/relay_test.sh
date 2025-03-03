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
SNYK_STATUS=$(get_container_status test-snyk-broker-1)
NEURON_STATUS=$(get_container_status test-axon-relay-1)
    
while [ "$SNYK_STATUS" != "running" ] || [ "$NEURON_STATUS" != "running" ]; do
    
    if [ $COUNTER -eq 0 ]; then
        echo "Containers did not start in time"
        docker compose logsgi
        exit 1
    fi
    
    echo "Waiting for containers to start"
    sleep 1
    SNYK_STATUS=$(get_container_status test-snyk-broker-1)
    NEURON_STATUS=$(get_container_status test-axon-relay-1)
    COUNTER=$((COUNTER-1))
done

echo "Sending request to broker server..."
# The file we look for is at /tmp/token, mounted into the python-server container
FILENAME="token-$(date +%s)"
echo "$TOKEN" > /tmp/$FILENAME
result=$(curl -s -q http://localhost:$SERVER_PORT/broker/$TOKEN/$FILENAME)

if [ "$result" != "$TOKEN" ]; then
    echo "FAIL: Expected $TOKEN, got $result"
    exit 1
fi
echo "Success!"
#! /bin/bash

app_cmd=$1
shift 1

touch /tmp/app.log
touch /tmp/agent.log
tail -f /tmp/app.log -f /tmp/agent.log  &
export AXON_APP=1
run_agent() {
    echo "Starting Cortex Axon Agent"
    while true;
    do
        /agent/start 2>&1 | tee -a /tmp/agent.log
        echo "Cortex Axon Agent exited, restarting"
        tail -n 50 /tmp/agent.log
        sleep 1
    done
}


run_agent &

# Give the agent a bit to start up
sleep 2

echo "Starting Axon App: $app_cmd"

(exec "$app_cmd"  2>&1 | tee -a /tmp/app.log) &
APP_PID=$!



wait $APP_PID

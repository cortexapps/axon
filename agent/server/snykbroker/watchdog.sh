#!/bin/sh

# Usage: watchdog.sh <pid1> <pid2>
# This script monitors the first process and kills the second process if the first process exits.

# Check if two arguments are provided
if [ "$#" -ne 2 ]; then
    echo "Usage: $0 <pid1> <pid2>"
    exit 1
fi

PID1=$1
PID2=$2

# Function to check if a process is running
is_running() {
    ps -p $1 > /dev/null 2>&1
}

# Monitor the first process
while is_running $PID1; do

    if ! is_running $PID2; then
        echo "Process $PID2 is not running."
        exit 0
    fi
    sleep 1
done

# If the first process exits, kill the second process
if is_running $PID2; then
    echo "Process $PID1 has exited. Killing process $PID2."
    kill $PID2
else
    echo "Process $PID1 has exited. Process $PID2 is not running."
fi
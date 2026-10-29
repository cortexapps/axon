#! /bin/sh

# The -grpc image has no snyk-broker, so it can only run the gRPC tunnel.
if ! command -v "${SNYK_BROKER_PATH:-snyk-broker}" >/dev/null 2>&1
then
    export AXON_RELAY_TRANSPORT=grpc-tunnel
fi

if [ "$1" = "serve" ]
then
    shift
    exec /agent/start $@
else 
    exec /agent/cortex-axon-agent $@
fi

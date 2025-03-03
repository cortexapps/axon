#! /bin/sh

if [ "$1" = "serve" ]
then
    shift
    exec /agent/start $@
else 
    exec /agent/cortex-axon-agent $@
fi

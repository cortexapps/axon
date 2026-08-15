#!/bin/bash
set -u

# Runs the load harness once per gRPC connection model and prints a
# side-by-side summary. Every model gets the same topology, the same offered
# load and the same chaos rate, so the only variable is how tunnel streams
# map onto TCP connections:
#
#   pool   adaptive watermark, one connection per stream (today's default)
#   conns  fixed CONNS connections, one stream each
#   mux    fixed CONNS connections, STREAMS_PER_CONN streams multiplexed
#          over each (the snyk-broker shape: few connections, many calls)
#   direct fixed MUX_CONNS connections, round-robin across every server,
#          streams opened on demand with an idle reserve
#
# pool, conns and mux are sized to the SAME concurrent stream count, so the
# comparison between them isolates connection count rather than concurrency.
#
# direct is deliberately NOT held to that stream count. Its argument is that
# a fixed concurrency number is the thing to remove: streams follow demand,
# and the ceiling exists only so an overloaded agent pushes back instead of
# accepting work it cannot do. Pinning it to STREAMS would test the claim by
# assuming its opposite. It runs at MUX_CONNS connections so the pair
# direct-vs-mux isolates exactly the proposed change — same connection count,
# on-demand streams instead of a fixed multiplex.
#
# Knobs (env): SERVERS AGENTS_PER_TOKEN LOADGENS WORKERS DURATION
#              CHAOS_INTERVAL STREAMS MODELS IDLE_STREAMS MAX_STREAMS

cd "$(dirname "$0")" || exit 1

export SERVERS=${SERVERS:-5}
export AGENTS_PER_TOKEN=${AGENTS_PER_TOKEN:-2}
export LOADGENS=${LOADGENS:-2}
export WORKERS=${WORKERS:-30}
export DURATION=${DURATION:-6m}
export CHAOS_INTERVAL=${CHAOS_INTERVAL:-30}

# Concurrent streams each agent keeps open, held equal across models.
STREAMS=${STREAMS:-16}
# Connections used by the multiplexed model (snyk-broker uses 2).
MUX_CONNS=${MUX_CONNS:-2}

# Per-server cap counts connections. Keep it high enough that it never
# binds before the model's own connection count does: the "conns" model
# wants STREAMS connections spread over SERVERS instances.
export MAX_STREAMS_PER_SERVER=${MAX_STREAMS_PER_SERVER:-4}

# "direct" opens streams on demand, so the per-token ceiling must sit above
# what the offered load actually needs or it, not the model, sets the limit.
export MAX_STREAMS_PER_TOKEN=${MAX_STREAMS_PER_TOKEN:-128}
# Idle reserve and per-agent stream ceiling for "direct".
DIRECT_IDLE=${DIRECT_IDLE:-4}
DIRECT_MAX=${DIRECT_MAX:-32}

MODELS=${MODELS:-"pool conns mux direct"}

echo "=== Connection-model comparison ==="
echo "topology: servers=$SERVERS agents/token=$AGENTS_PER_TOKEN loadgens=$LOADGENS workers=$WORKERS"
echo "load:     duration=$DURATION chaos every ${CHAOS_INTERVAL}s"
echo "streams:  $STREAMS concurrent per agent (pool/conns/mux); direct: ${DIRECT_IDLE} idle, ${DIRECT_MAX} max, on demand"
echo "models:   $MODELS"
echo

FAILED=0
for model in $MODELS; do
    case "$model" in
        pool)
            # Adaptive: floor 2, ceiling STREAMS. Grows into the same
            # ceiling the fixed models sit at.
            export CONN_MODE=pool MIN_SLOTS=2 MAX_SLOTS=$STREAMS
            export CONNS=$STREAMS STREAMS_PER_CONN=1
            ;;
        conns)
            export CONN_MODE=conns CONNS=$STREAMS STREAMS_PER_CONN=1
            ;;
        mux)
            per=$(( STREAMS / MUX_CONNS ))
            [ "$per" -lt 1 ] && per=1
            export CONN_MODE=mux CONNS=$MUX_CONNS STREAMS_PER_CONN=$per
            ;;
        direct)
            # Same connection count as mux; streams follow demand instead of
            # being fixed, and round-robin spreads them over every server.
            export CONN_MODE=direct CONNS=$MUX_CONNS STREAMS_PER_CONN=1
            export IDLE_STREAMS=$DIRECT_IDLE MAX_STREAMS=$DIRECT_MAX
            ;;
        *)
            echo "unknown model: $model"; exit 1 ;;
    esac

    export RUN_TAG="$model"
    echo "############################################################"
    if [ "$model" = "direct" ]; then
        echo "### direct: conns=$CONNS idleStreams=$IDLE_STREAMS maxStreams=$MAX_STREAMS"
    else
        echo "### $model: conns=$CONNS streamsPerConn=$STREAMS_PER_CONN"
    fi
    echo "############################################################"
    ./load_test.sh > "reports/run-$model.log" 2>&1
    code=$?
    if [ "$code" != "0" ]; then
        echo "MODEL $model FAILED (exit $code) — see reports/run-$model.log"
        tail -20 "reports/run-$model.log"
        FAILED=1
    else
        echo "MODEL $model passed"
    fi
    echo
done

echo "=== Summary ==="
python3 summarize_models.py $MODELS

[ "$FAILED" != "0" ] && { echo "One or more models failed"; exit 1; }
exit 0

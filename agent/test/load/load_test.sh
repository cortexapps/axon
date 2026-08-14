#!/bin/bash
set -u

# gRPC tunnel load & chaos test.
#
# Stands up T load generators → X tunnel servers → N agents (across 3
# broker tokens) → echo upstream, with a dispatcher-mock playing Cortex's
# routing role, runs sustained randomized load, optionally SIGKILLs and
# restarts agents/servers during the run, and validates:
#   - zero end-to-end integrity failures (byte-exact both directions)
#   - availability >= MIN_SUCCESS_PCT despite chaos
#   - steady-state recovery after chaos stops (every token routable)
#
# Not wired into per-PR CI (it's a multi-minute soak); a nightly workflow
# can invoke `make -C agent load-test`.
#
# Knobs (env):
#   SERVERS=3 AGENTS_PER_TOKEN=2 LOADGENS=2 WORKERS=16 DURATION=3m
#   CHAOS=1 CHAOS_INTERVAL=20 MIN_SUCCESS_PCT=99 DISPATCHER_PORT=18900

SERVERS=${SERVERS:-3}
AGENTS_PER_TOKEN=${AGENTS_PER_TOKEN:-2}
LOADGENS=${LOADGENS:-2}
export DURATION=${DURATION:-3m}
export WORKERS=${WORKERS:-16}
export MIN_SUCCESS_PCT=${MIN_SUCCESS_PCT:-99}
# Keep this BELOW the ephemeral port range (49152-65535 on macOS/Linux):
# the harness opens many outbound connections during a run, and one of
# them transiently claiming the dispatcher's port as a source port makes
# the next `compose up` fail with "address already in use".
export DISPATCHER_PORT=${DISPATCHER_PORT:-18900}
CHAOS=${CHAOS:-1}
CHAOS_INTERVAL=${CHAOS_INTERVAL:-20}

# Agent connection model under test. Exported so compose picks them up.
export CONN_MODE=${CONN_MODE:-pool}
export CONNS=${CONNS:-8}
export STREAMS_PER_CONN=${STREAMS_PER_CONN:-8}
# Tag report/log filenames so comparison runs don't overwrite each other.
RUN_TAG=${RUN_TAG:-$CONN_MODE}

COMPOSE="docker compose -f docker-compose.load.yml"
DISPATCHER="http://localhost:${DISPATCHER_PORT}"
TOKENS=(tok-a tok-b tok-c)

mkdir -p reports
rm -f reports/loadgen-*.json samples.log chaos.log

CHAOS_PID=""
SAMPLER_PID=""
function cleanup {
    echo "Cleanup: stopping chaos/sampler and docker compose"
    [ -n "$CHAOS_PID" ] && kill "$CHAOS_PID" 2>/dev/null
    [ -n "$SAMPLER_PID" ] && kill "$SAMPLER_PID" 2>/dev/null
    $COMPOSE down -v --remove-orphans >/dev/null 2>&1
}
trap cleanup EXIT

function token_hash {
    # sha256sum isn't present on stock macOS; shasum is.
    if command -v sha256sum >/dev/null 2>&1; then
        echo -n "$1" | sha256sum | awk '{print $1}'
    else
        echo -n "$1" | shasum -a 256 | awk '{print $1}'
    fi
}

function pick_random_line {
    # `shuf | head -1` equivalent that doesn't need GNU coreutils.
    awk -v seed="$RANDOM" 'BEGIN{srand(seed)} {a[NR]=$0} END{if(NR) print a[int(rand()*NR)+1]}'
}

function routable {
    # routable <raw-token>: 0 if the dispatcher knows >=1 server for it
    local hash count
    hash=$(token_hash "$1")
    count=$(curl -sf "$DISPATCHER/servers/$hash" | grep -o '"[^"]*"' | grep -vc servers)
    [ "${count:-0}" -gt 0 ]
}

if lsof -nP -iTCP:"$DISPATCHER_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "FAIL: DISPATCHER_PORT $DISPATCHER_PORT is already in use:"
    lsof -nP -iTCP:"$DISPATCHER_PORT" -sTCP:LISTEN
    echo "Set DISPATCHER_PORT to a free port below 49152 and retry."
    exit 1
fi

echo "=== Starting stack: servers=$SERVERS agents/token=$AGENTS_PER_TOKEN loadgens=$LOADGENS duration=$DURATION chaos=$CHAOS ==="
echo "=== Connection model: mode=$CONN_MODE conns=$CONNS streamsPerConn=$STREAMS_PER_CONN (tag: $RUN_TAG) ==="
$COMPOSE up -d --build \
    --scale grpc-tunnel-server="$SERVERS" \
    --scale agent-a="$AGENTS_PER_TOKEN" \
    --scale agent-b="$AGENTS_PER_TOKEN" \
    --scale agent-c="$AGENTS_PER_TOKEN" \
    --scale loadgen="$LOADGENS" || { echo "compose up failed"; exit 1; }

echo "=== Waiting for all tokens to be routable ==="
COUNTER=90
while true; do
    ok=1
    for t in "${TOKENS[@]}"; do
        routable "$t" || { ok=0; break; }
    done
    [ "$ok" = 1 ] && break
    COUNTER=$((COUNTER-1))
    if [ "$COUNTER" -le 0 ]; then
        echo "FAIL: tokens not routable in time"
        curl -s "$DISPATCHER/state" || true
        $COMPOSE logs --tail 40 grpc-tunnel-server agent-a
        exit 1
    fi
    sleep 2
done
echo "All tokens routable; load generators will start on their own."

# --- observability sampler ---
(
    while true; do
        {
            echo "--- $(date -u +%H:%M:%S)"
            curl -sf --max-time 4 "$DISPATCHER/probe"
            echo
            # one agent's slot-pool view (streams/busy/target)
            AGENT=$($COMPOSE ps -q agent-a | head -1)
            if [ -n "$AGENT" ]; then
                docker exec "$AGENT" curl -sf --max-time 3 \
                    "http://localhost:80/__axon/broker/systemcheck" 2>/dev/null
                echo
            fi
        } >> samples.log 2>&1
        sleep 10
    done
) &
SAMPLER_PID=$!

# --- chaos loop ---
if [ "$CHAOS" = "1" ]; then
(
    sleep 15  # let the load warm up first
    i=0
    while true; do
        i=$((i+1))
        # Alternate victims between a random agent and a random server.
        # Bounded degradation: victims are restarted after a short outage,
        # and we only take one victim at a time.
        if [ $((i % 2)) = 0 ]; then
            SVC="grpc-tunnel-server"
            # never kill the last server
            COUNT=$($COMPOSE ps -q $SVC | wc -l)
            [ "$COUNT" -le 1 ] && { sleep "$CHAOS_INTERVAL"; continue; }
        else
            case $((i % 3)) in
                0) SVC="agent-a" ;;
                1) SVC="agent-b" ;;
                2) SVC="agent-c" ;;
            esac
        fi
        VICTIM=$($COMPOSE ps -q "$SVC" | pick_random_line)
        [ -z "$VICTIM" ] && { sleep "$CHAOS_INTERVAL"; continue; }
        NAME=$(docker inspect -f '{{.Name}}' "$VICTIM")
        if [ $((i % 4)) -lt 2 ]; then
            echo "$(date -u +%H:%M:%S) SIGKILL $NAME" >> chaos.log
            docker kill "$VICTIM" >/dev/null 2>&1
            sleep 5
            docker start "$VICTIM" >/dev/null 2>&1
        else
            echo "$(date -u +%H:%M:%S) restart $NAME" >> chaos.log
            docker restart -t 2 "$VICTIM" >/dev/null 2>&1
        fi
        sleep "$CHAOS_INTERVAL"
    done
) &
CHAOS_PID=$!
fi

echo "=== Load running for $DURATION (chaos: $CHAOS, see chaos.log) ==="
FAILED=0
# -a: a generator that already exited (e.g. failed fast) must still be
# waited on, or its non-zero exit silently disappears and we "pass".
for c in $($COMPOSE ps -aq loadgen); do
    CODE=$(docker wait "$c")
    NAME=$(docker inspect -f '{{.Name}}' "$c")
    echo "loadgen $NAME exited with $CODE"
    [ "$CODE" != "0" ] && FAILED=1
done

# --- stop chaos, quiesce, validate recovery ---
[ -n "$CHAOS_PID" ] && kill "$CHAOS_PID" 2>/dev/null && CHAOS_PID=""
echo "=== Quiescing 90s, then validating steady-state recovery ==="
sleep 90

for t in "${TOKENS[@]}"; do
    if routable "$t"; then
        echo "recovery OK: $t routable"
    else
        echo "FAIL: token $t not routable after quiesce"
        curl -s "$DISPATCHER/state" || true
        FAILED=1
    fi
done

echo "=== Reports ==="
for f in reports/loadgen-*.json; do
    [ -f "$f" ] || continue
    echo "--- $f"
    cat "$f"
    echo
done
CHAOS_EVENTS=$(wc -l < chaos.log 2>/dev/null | tr -d ' ' || echo 0)
echo "=== Chaos events: ${CHAOS_EVENTS:-0} (chaos.log) ==="
if [ "$CHAOS" = "1" ] && [ "${CHAOS_EVENTS:-0}" = "0" ]; then
    # Chaos silently doing nothing turns this into a plain load test that
    # always passes; treat it as a harness failure, not a green run.
    echo "FAIL: chaos was enabled but no chaos events fired"
    FAILED=1
fi
echo "=== Pool samples in samples.log ==="

# Keep each model's artifacts so comparison runs can be diffed afterwards.
mkdir -p "reports/$RUN_TAG"
cp -f reports/loadgen-*.json "reports/$RUN_TAG/" 2>/dev/null
cp -f chaos.log samples.log "reports/$RUN_TAG/" 2>/dev/null
echo "=== Artifacts archived to reports/$RUN_TAG/ ==="

if [ "$FAILED" != "0" ]; then
    echo "LOAD TEST FAILED"
    exit 1
fi
echo "LOAD TEST PASSED"

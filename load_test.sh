#!/bin/bash

BASE_URL="http://localhost:8089"
ENDPOINTS=("users" "payments" "auth" "analytics")

echo "🚀 Starting High-Velocity Chaotic Load Test..."
echo "Target: $BASE_URL"

while true; do
    # Pick a random endpoint
    IDX=$(( RANDOM % 4 ))
    TARGET=${ENDPOINTS[$IDX]}

    # Generate random latency request (0ms to 800ms)
    SLEEP_MS=$(( RANDOM % 800 ))

    # Randomly decide to be a "normal" user or a "spike"
    CHANCE=$(( RANDOM % 100 ))

    if [ $CHANCE -gt 90 ]; then
        # Traffic Spike! 🌊
        # echo "🌊 Spike on $TARGET!"
        for i in {1..15}; do
            curl -s "$BASE_URL/$TARGET?sleep=$SLEEP_MS" > /dev/null &
        done
    else
        # Normal traffic
        curl -s "$BASE_URL/$TARGET?sleep=$SLEEP_MS" > /dev/null &
    fi

    # Manage concurrency: don't let background jobs pile up infinitely
    # If more than 50 jobs running, wait until count drops
    while (( $(jobs -r -p | wc -l) > 50 )); do
        sleep 0.1
    done

    # Variable sleep between batches (very fast: 0.01s to 0.1s)
    sleep 0.0$(( ( RANDOM % 9 ) + 1 ))
done
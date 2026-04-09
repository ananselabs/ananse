#!/bin/sh
# Sustained load test with fluctuating random traffic
# Run from inside a mesh pod: kubectl exec -n ananse deployment/auth -c auth -- sh /load-test.sh

DURATION_SECONDS=${1:-600}  # Default 10 minutes
END_TIME=$(($(date +%s) + DURATION_SECONDS))

SERVICES="analytics:5004 auth:5001 users:5002 payments:5003 echo:4199/echo"
ERROR_CODES="200 200 200 200 200 500 503 429"  # 5/8 success, 3/8 errors

random_range() {
    min=$1
    max=$2
    awk -v min="$min" -v max="$max" 'BEGIN{srand(); print int(min+rand()*(max-min+1))}'
}

pick_random() {
    echo "$1" | tr ' ' '\n' | awk 'BEGIN{srand()} {a[NR]=$0} END{print a[int(rand()*NR)+1]}'
}

echo "Starting load test for $DURATION_SECONDS seconds..."
echo "End time: $(date -d @$END_TIME 2>/dev/null || date -r $END_TIME)"

wave=0
while [ $(date +%s) -lt $END_TIME ]; do
    wave=$((wave + 1))

    # Fluctuating burst size (5-30 concurrent requests)
    burst_size=$(random_range 5 30)

    # Random delay between waves (100-500ms)
    wave_delay=$(random_range 100 500)

    # Occasional spike (10% chance of 3x traffic)
    spike_roll=$(random_range 1 10)
    if [ "$spike_roll" -eq 1 ]; then
        burst_size=$((burst_size * 3))
        echo "Wave $wave: SPIKE! $burst_size requests"
    fi

    for i in $(seq 1 $burst_size); do
        # Pick random service
        svc=$(pick_random "$SERVICES")
        host=$(echo "$svc" | cut -d: -f1)
        port_path=$(echo "$svc" | cut -d: -f2)

        # Random latency (0-150ms)
        sleep_ms=$(random_range 0 150)

        # Random status code (mostly 200, some errors)
        code=$(pick_random "$ERROR_CODES")

        # Build URL
        if [ "$host" = "echo" ]; then
            url="http://${host}:${port_path}?sleep=${sleep_ms}"
        else
            url="http://${host}:${port_path}/?sleep=${sleep_ms}&code=${code}"
        fi

        # Fire request in background
        wget -qO- "$url" > /dev/null 2>&1 &
    done

    # Don't let background jobs pile up too much
    if [ $((wave % 10)) -eq 0 ]; then
        wait
        remaining=$((END_TIME - $(date +%s)))
        echo "Wave $wave complete, ${remaining}s remaining"
    fi

    # Sleep between waves (converted to seconds)
    sleep_sec=$(awk "BEGIN{print $wave_delay/1000}")
    sleep "$sleep_sec"
done

wait
echo "Load test complete. Ran $wave waves."

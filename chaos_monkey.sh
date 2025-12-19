#!/bin/bash

SERVICES=("auth" "users" "payments" "analytics")

# Function to restart a service
start_service() {
    service=$1
    echo "⚡ $(date '+%H:%M:%S'): Resurrecting $service..."
    nohup go run services/$service/main.go > /dev/null 2>&1 &
}

# Function to kill a service
kill_service() {
    service=$1
    echo "💀 $(date '+%H:%M:%S'): Killing $service..."
    pkill -f "go run services/$service/main.go"
}

echo "🐵 Chaos Monkey is watching... (Press Ctrl+C to stop)"

while true; do
    # Wait for a random interval before causing havoc (5-15 seconds)
    WAIT_TIME=$(( ( RANDOM % 10 ) + 5 ))
    sleep $WAIT_TIME

    # Pick a random victim
    INDEX=$(( RANDOM % 4 ))
    SERVICE=${SERVICES[$INDEX]}

    # Kill the service
    kill_service $SERVICE

    # Leave it dead for a random duration (3-8 seconds)
    # This forces the proxy to notice (health check interval is 10s, but retries might happen sooner)
    # Note: If health check is 10s, we might want downtime to be slightly longer or shorter to test race conditions.
    DOWNTIME=$(( ( RANDOM % 6 ) + 3 ))
    sleep $DOWNTIME

    # Bring it back
    start_service $SERVICE
done

#!/bin/bash
# set -e # Temporarily disabled to debug early exit

CONFIG_FILE="config/config.yml"
BACKUP_FILE="config/config.yml.bak"
PROXY_PID_FILE="proxy.pid"

# Save original config
cp "$CONFIG_FILE" "$BACKUP_FILE"

cleanup() {
    echo "---------------------------------------------------"
    echo "CLEANUP"
    echo "---------------------------------------------------"
    mv "$BACKUP_FILE" "$CONFIG_FILE"
    pkill -P $$ || true
    pkill -f "ananse-auth" || true
    pkill -f "ananse-users" || true
    pkill -f "ananse-payments" || true
    pkill -f "ananse-analytics" || true
    pkill -f "ananse-proxy" || true
    rm -f ananse-auth ananse-users ananse-payments ananse-analytics ananse-proxy stress-tool "$PROXY_PID_FILE"
}
trap cleanup EXIT

echo "Building Components..."
go build -o ananse-auth services/auth/main.go
go build -o ananse-users services/users/main.go
go build -o ananse-payments services/payments/main.go
go build -o ananse-analytics services/analytics/main.go
go build -o ananse-proxy proxy/main.go
go build -o stress-tool tools/stress/main.go

echo "Starting Services..."
./ananse-auth &
./ananse-users &
./ananse-payments &
./ananse-analytics &
sleep 2
./ananse-proxy &
PROXY_PID=$!
echo $PROXY_PID > "$PROXY_PID_FILE"
sleep 2

check_alive() {
    if ! kill -0 $PROXY_PID 2>/dev/null; then
        echo "❌ PROXY DIED! Test Failed."
        exit 1
    fi
}

echo "==================================================="
echo "SCENARIO 1: The Flood (Mixed Load)"
echo "==================================================="
echo "Running mixed normal, error-forcing, and slow-forcing traffic..."

# Run with a 6-second timeout (1s buffer over the 5s internal duration)
# We remove the '&' and 'wait' logic here to let 'timeout' handle the lifecycle
if timeout 6s ./stress-tool -workers 50 -duration 5s -mode mixed; then
    echo "Stress Tool exited normally."
else
    EXIT_CODE=$?
    if [ $EXIT_CODE -eq 124 ]; then
        echo "⚠️  Stress Tool timed out (Force killed by script)"
    else
        echo "Stress Tool failed with code $EXIT_CODE"
    fi
fi

check_alive
echo "✅ Survived The Flood."
echo "Finished Scenario 1."

echo "==================================================="
echo "SCENARIO 2: The Malformed (Garbage Input)"
echo "==================================================="
echo "Sending non-compliant HTTP garbage..."
./stress-tool -workers 20 -duration 5s -mode malformed &
STRESS_PID=$!

wait $STRESS_PID
STRESS_EXIT_CODE=$?
echo "Stress Tool (Scenario 2) exited with code: $STRESS_EXIT_CODE"
check_alive
echo "✅ Survived Malformed Input."
echo "Finished Scenario 2."

echo "==================================================="
echo "SCENARIO 3: The Grim Reaper (Backend Death)"
echo "==================================================="
echo "Starting traffic..."
./stress-tool -workers 30 -duration 10s -mode mixed &
STRESS_PID=$!

echo "Killing Analytics service..."
pkill -f "ananse-analytics"
sleep 2
echo "Restarting Analytics service..."
./ananse-analytics &
sleep 2
echo "Killing Auth service..."
pkill -f "ananse-auth"
sleep 2
echo "Restarting Auth service..."
./ananse-auth &

wait $STRESS_PID
STRESS_EXIT_CODE=$?
echo "Stress Tool (Scenario 3) exited with code: $STRESS_EXIT_CODE"
check_alive
echo "✅ Survived Backend Flapping."
echo "Finished Scenario 3."

echo "==================================================="
echo "SCENARIO 4: Config Chaos"
echo "==================================================="
echo "Starting traffic..."
./stress-tool -workers 30 -duration 10s -mode mixed &
STRESS_PID=$!

echo "Injecting valid and invalid configs rapidly..."
for i in {1..3}; do
    # Invalid
    echo "Writing INVALID config..."
    echo "proxy: { BROKEN" > "$CONFIG_FILE"
    sleep 1
    
    # Valid (Small)
    echo "Writing VALID (Small) config..."
    cat > "$CONFIG_FILE" <<EOF
proxy:
  port: 8089
  metrics_port: 9090
  health_check_interval: 1
services:
  - name: analytics-upstream
    endpoints:
      - address: "localhost:5004"
    routes:
      - path: "/analytics"
        methods: ["GET"]
EOF
    sleep 1
    
    # Valid (Restored)
    echo "Writing VALID (Full) config..."
    cat "$BACKUP_FILE" > "$CONFIG_FILE"
    sleep 1
done

wait $STRESS_PID
STRESS_EXIT_CODE=$?
echo "Stress Tool (Scenario 4) exited with code: $STRESS_EXIT_CODE"
check_alive
echo "✅ Survived Config Chaos."
echo "Finished Scenario 4."

echo "==================================================="
echo "SCENARIO 5: Slowloris"
echo "==================================================="
echo "Holding connections open..."
./stress-tool -workers 50 -duration 5s -mode slowloris &
STRESS_PID=$!

wait $STRESS_PID
STRESS_EXIT_CODE=$?
echo "Stress Tool (Scenario 5) exited with code: $STRESS_EXIT_CODE"
check_alive
echo "✅ Survived Slowloris."
echo "Finished Scenario 5."

echo "All Berserk Tests Passed."

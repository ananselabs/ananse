#!/bin/bash

# Kill any existing instances of these services
pkill -f "go run services/auth/main.go"
pkill -f "go run services/users/main.go"
pkill -f "go run services/payments/main.go"
pkill -f "go run services/analytics/main.go"
pkill -f "go run proxy/main.go"

echo "Starting Auth Service (5001)..."
go run services/auth/main.go &

echo "Starting Users Service (5002)..."
go run services/users/main.go &

echo "Starting Payments Service (5003)..."
go run services/payments/main.go &

echo "Starting Analytics Service (5004)..."
go run services/analytics/main.go &

# Wait a bit for services to come up
sleep 2

echo "Starting Proxy (8089)..."
go run proxy/main.go &

echo "All services started!"
echo "Press Ctrl+C to stop all services (this script just launched them in background, use 'pkill -f go' or similar to clean up if you exit)."

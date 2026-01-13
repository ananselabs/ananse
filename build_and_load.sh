#!/bin/bash
set -e

# If an argument is provided, use it as the service list. Otherwise, use all.
if [ -n "$1" ]; then
    SERVICES="$1"
    MODE="single"
else
    SERVICES="analytics auth echo payments users proxy controlplane"
    MODE="all"
fi

echo "🚀 Starting Build & Load process for: $SERVICES"

for SERVICE in $SERVICES; do
    echo "------------------------------------------------"
    echo "📦 Building $SERVICE:latest..."
    
    # Build image
    if [ "$SERVICE" == "proxy" ]; then
        docker build -t $SERVICE:latest -f proxy/Dockerfile .
    elif [ "$SERVICE" == "controlplane" ]; then
        docker build -t $SERVICE:latest -f controlplane/Dockerfile .
    else
        docker build -t $SERVICE:latest -f services/$SERVICE/Dockerfile .
    fi

    echo "🚚 Loading $SERVICE:latest into Kind cluster..."
    kind load docker-image $SERVICE:latest
done

echo "------------------------------------------------"
echo "✅ Build and Load complete!"

echo "♻️  Restarting pods..."
if [ "$MODE" == "all" ]; then
    kubectl delete pods --all -n ananse
else
    # Delete only the pods matching the service label
    # Note: Special handling for proxy/controlplane which share a pod labeled 'proxy-gateway'
    if [[ "$SERVICES" == "proxy" || "$SERVICES" == "controlplane" ]]; then
        kubectl delete pods -l app=proxy-gateway -n ananse
    else
        kubectl delete pods -l app=$SERVICES -n ananse
    fi
fi

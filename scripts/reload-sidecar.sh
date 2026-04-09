#!/bin/bash
set -e

CLUSTER="ananse"
NAMESPACE="ananse"
APP_LABEL="app=analytics"
DEBUG_PORT=2345

# Use timestamp to ensure unique tag
TAG="debug-$(date +%s)"
IMAGE="anthony4m/ananse-proxy:$TAG"

# Check mode
if [ "$1" = "startup" ]; then
    DOCKERFILE="proxy/Dockerfile.proxy.debug-startup"
    echo "=== STARTUP DEBUG MODE ==="
else
    DOCKERFILE="proxy/Dockerfile.proxy.debug"
    echo "=== NORMAL DEBUG MODE ==="
fi

# Kill any existing port-forward
pkill -f "port-forward.*${DEBUG_PORT}" 2>/dev/null || true

echo "Building proxy image: $IMAGE"
# Use DOCKER_BUILDKIT=0 to avoid multi-arch manifest issues with Kind
DOCKER_BUILDKIT=0 docker build --no-cache -f $DOCKERFILE -t $IMAGE .

echo "Loading image to Kind cluster..."
kind load docker-image $IMAGE --name $CLUSTER

# Update injector config
echo "Updating injector config..."
kubectl patch configmap ananse-injector-config -n ananse-system \
    --type merge -p "{\"data\":{\"SIDECAR_IMAGE\":\"$IMAGE\"}}" 2>/dev/null || true

# Need to restart controlplane to pick up config change
kubectl rollout restart deployment/controlplane -n ananse-system
sleep 3

echo "Restarting pod..."
kubectl delete pod -n $NAMESPACE -l $APP_LABEL --wait=false

if [ "$1" = "startup" ]; then
    echo "Waiting for container to start (not Ready - app paused)..."
    sleep 3

    while true; do
        POD_NAME=$(kubectl get pod -n $NAMESPACE -l $APP_LABEL -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
        if [ -n "$POD_NAME" ]; then
            STATUS=$(kubectl get pod -n $NAMESPACE $POD_NAME -o jsonpath='{.status.containerStatuses[?(@.name=="ananse-proxy")].state.running}' 2>/dev/null)
            if [ -n "$STATUS" ]; then
                break
            fi
        fi
        echo "Waiting for container..."
        sleep 2
    done
else
    echo "Waiting for pod to be ready (timeout 120s)..."
    kubectl wait --for=condition=Ready pod -n $NAMESPACE -l $APP_LABEL --timeout=120s || {
        echo "Pod not ready yet, but continuing anyway..."
        POD_NAME=$(kubectl get pod -n $NAMESPACE -l $APP_LABEL -o jsonpath='{.items[0].metadata.name}')
    }
    POD_NAME=$(kubectl get pod -n $NAMESPACE -l $APP_LABEL -o jsonpath='{.items[0].metadata.name}')
fi

echo "Starting port-forward to $POD_NAME..."
kubectl port-forward -n $NAMESPACE $POD_NAME $DEBUG_PORT:$DEBUG_PORT &
PF_PID=$!

sleep 2

echo ""
echo "================================================"
if [ "$1" = "startup" ]; then
    echo "STARTUP DEBUG MODE"
    echo "App is PAUSED waiting for debugger!"
    echo "Connect GoLand NOW to start execution."
else
    echo "NORMAL DEBUG MODE"
    echo "App is running. Attach debugger anytime."
fi
echo ""
echo "Pod: $POD_NAME"
echo "Image: $IMAGE"
echo "Debugger: localhost:$DEBUG_PORT"
echo ""
echo "Press Ctrl+C to stop"
echo "================================================"
echo ""

trap "kill $PF_PID 2>/dev/null; echo 'Stopped'" EXIT
wait $PF_PID

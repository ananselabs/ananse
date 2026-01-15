#!/bin/bash
set -e

# --- 1. CLUSTER CHECK ---
CLUSTER_NAME=$(kind get clusters | head -n 1)

if [ -z "$CLUSTER_NAME" ]; then
    echo "❌ ERROR: No Kind cluster found running."
    echo "   Please run: 'kind create cluster'"
    exit 1
fi

echo "✅ Detected Kind cluster: '$CLUSTER_NAME'"

# Ensure kubectl is talking to the correct Kind cluster
# Kind contexts are usually named "kind-<cluster_name>"
kubectl cluster-info --context "kind-$CLUSTER_NAME" > /dev/null 2>&1
if [ $? -eq 0 ]; then
    export KUBECONFIG_CONTEXT="kind-$CLUSTER_NAME"
    echo "🔗 Kubectl context switched to: kind-$CLUSTER_NAME"
else
    echo "⚠️  Warning: Could not switch context automatically. Verifying current context..."
fi

# --- 2. SETUP SERVICES ---
if [ -n "$1" ]; then
    SERVICES="$1"
    MODE="selected"
else
    SERVICES="analytics auth echo payments users proxy controlplane"
    MODE="all"
fi

echo "🚀 Starting Build & Load process for: $SERVICES"

# --- 3. BUILD & LOAD LOOP ---
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

    echo "🚚 Loading $SERVICE:latest into cluster '$CLUSTER_NAME'..."
    kind load docker-image $SERVICE:latest --name "$CLUSTER_NAME"
done

echo "------------------------------------------------"
echo "✅ Build and Load complete!"

# --- 4. RESTART PODS ---
echo "♻️  Restarting pods in namespace 'ananse'..."

# Ensure namespace exists to prevent errors
if ! kubectl get namespace ananse --context "kind-$CLUSTER_NAME" > /dev/null 2>&1; then
    echo "⚠️  Namespace 'ananse' not found. Skipping restart."
    exit 0
fi

if [ "$MODE" == "all" ]; then
    kubectl --context "kind-$CLUSTER_NAME" delete pods --all -n ananse
else
    # We must loop here because $SERVICES might be "auth analytics"
    # and "app=auth analytics" is not a valid selector.
    for SERVICE in $SERVICES; do
        if [[ "$SERVICE" == "proxy" || "$SERVICE" == "controlplane" ]]; then
            echo "   ⟳ Restarting proxy-gateway..."
            kubectl --context "kind-$CLUSTER_NAME" delete pods -l app=proxy-gateway -n ananse --ignore-not-found
        else
            echo "   ⟳ Restarting $SERVICE..."
            kubectl --context "kind-$CLUSTER_NAME" delete pods -l app=$SERVICE -n ananse --ignore-not-found
        fi
    done
fi

echo "🎉"
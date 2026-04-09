#!/bin/bash
set -e

# Mesh mTLS certificates for sidecar-to-sidecar communication
NAMESPACE=${1:-ananse}
SECRET_NAME=ananse-mesh-certs
DAYS=365
CERT_DIR=$(mktemp -d)

echo "Generating mesh certificates in $CERT_DIR..."

cd "$CERT_DIR"

# 1. Generate CA
openssl genrsa -out ca.key 2048
openssl req -x509 -new -nodes -key ca.key -days $DAYS -out ca.crt -subj "/CN=ananse-mesh-ca"

# 2. Create extensions file for mesh cert (wildcard for all services)
cat > san.ext << EOF
subjectAltName=DNS:*.${NAMESPACE}.svc.cluster.local,DNS:*.${NAMESPACE}.svc,DNS:localhost
extendedKeyUsage=serverAuth,clientAuth
EOF

# 3. Generate mesh cert (used by all sidecars)
openssl genrsa -out tls.key 2048
openssl req -new -key tls.key -out mesh.csr -subj "/CN=ananse-mesh"
openssl x509 -req -in mesh.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out tls.crt -days $DAYS -extfile san.ext

# 4. Create K8s secret with all three files (tls.crt, tls.key, ca.crt)
kubectl create secret generic $SECRET_NAME \
    --from-file=tls.crt=tls.crt \
    --from-file=tls.key=tls.key \
    --from-file=ca.crt=ca.crt \
    -n $NAMESPACE \
    --dry-run=client -o yaml | kubectl apply -f -

echo ""
echo "Created secret '$SECRET_NAME' in namespace '$NAMESPACE'"
echo ""

# 5. Verify
kubectl get secret $SECRET_NAME -n $NAMESPACE -o jsonpath='{.data}' | jq -r 'keys[]'

# Cleanup
cd -
rm -rf "$CERT_DIR"

echo ""
echo "Done! Mesh certs ready."

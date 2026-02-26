#!/bin/bash

SERVICE=ananse-webhook
NAMESPACE=ananse-system
SECRET_NAME=ananse-webhook-certs
DAYS=365

# Generate CA
openssl genrsa -out ca.key 2048
openssl req -x509 -new -nodes -key ca.key -days $DAYS -out ca.crt -subj "/CN=ananse-ca"

# Create extensions file for SAN
cat > san.ext << EOF
subjectAltName=DNS:${SERVICE}.${NAMESPACE}.svc,DNS:${SERVICE}.${NAMESPACE}.svc.cluster.local
EOF

# Generate server cert
openssl genrsa -out server.key 2048
openssl req -new -key server.key -out server.csr -subj "/CN=${SERVICE}.${NAMESPACE}.svc"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out server.crt -days $DAYS -extfile san.ext

# Cleanup
rm -f san.ext server.csr ca.srl

# Create K8s secret
kubectl create secret tls $SECRET_NAME --cert=server.crt --key=server.key -n $NAMESPACE --dry-run=client -o yaml | kubectl apply -f -

# Output caBundle for webhook
echo ""
echo "caBundle (base64):"
cat ca.crt | base64 | tr -d '\n'
echo ""
#!/usr/bin/env bash
# Provision TLS for the memory-policy VMI admission webhook.
#
# The lab cluster has no cert-manager, so we do the simplest sound
# thing: a self-signed CA, a serving cert for
# memory-tier-operator.<ns>.svc, a TLS secret mounted by the operator
# Deployment, and the CA patched into the MutatingWebhookConfiguration
# caBundle. Re-running rotates everything atomically (secret is
# applied, caBundle replaced).
#
# Requires: openssl, kubectl (or oc), cluster admin.
set -euo pipefail

NS="${NS:-kubevirt-mt}"
SVC="${SVC:-memory-tier-operator}"
SECRET="${SECRET:-memory-tier-operator-webhook}"
MWC="${MWC:-memory-policy-vmi-mutation}"

KCTL="${KCTL:-$(command -v kubectl || command -v oc)}"

DIR="$(mktemp -d)"
trap 'rm -rf "$DIR"' EXIT

# 1. Self-signed CA (10y).
openssl genrsa -out "$DIR/ca.key" 3072 2>/dev/null
openssl req -x509 -new -nodes -key "$DIR/ca.key" \
  -subj "/CN=memory-tier-operator-ca" -days 3650 -out "$DIR/ca.crt"

# 2. Serving cert for the in-cluster Service DNS name.
openssl genrsa -out "$DIR/tls.key" 3072 2>/dev/null
openssl req -new -key "$DIR/tls.key" \
  -subj "/CN=${SVC}.${NS}.svc" -out "$DIR/tls.csr"
cat > "$DIR/ext.cnf" <<EOF
subjectAltName = DNS:${SVC}.${NS}.svc,DNS:${SVC}.${NS}.svc.cluster.local
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
EOF
openssl x509 -req -in "$DIR/tls.csr" \
  -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" -CAcreateserial \
  -days 3650 -extfile "$DIR/ext.cnf" -out "$DIR/tls.crt" 2>/dev/null

# 3. TLS secret (overwrite on rotate).
"$KCTL" create secret tls "$SECRET" -n "$NS" \
  --cert="$DIR/tls.crt" --key="$DIR/tls.key" \
  --dry-run=client -o yaml | "$KCTL" apply -f -

# 4. Patch the CA bundle into the MutatingWebhookConfiguration.
CABUNDLE="$(base64 -w0 "$DIR/ca.crt")"
"$KCTL" patch mutatingwebhookconfiguration "$MWC" --type='json' \
  -p="[{\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"$CABUNDLE\"}]"

echo "webhook TLS provisioned:"
"$KCTL" get secret "$SECRET" -n "$NS" -o name
"$KCTL" get mutatingwebhookconfiguration "$MWC" \
  -o jsonpath='{.metadata.name} caBundle={.webhooks[0].clientConfig.caBundle}'
echo

#!/usr/bin/env bash
# Build and push the memory-tier-operator image to the OpenShift
# internal registry (via its default route).
#
# Usage:   ./hack/push-image.sh [tag]        (default tag: v8)
#
# Requires: oc logged in as cluster-admin (KUBECONFIG pointing at the
# cluster), podman, and a Go toolchain (the Containerfile expects
# prebuilt ./operator and ./driver binaries).
#
# The default route serves the cluster ingress certificate, so the
# podman login/push below use --tls-verify=false. Pushes authenticate
# with a short-lived token of the image-pusher service account (never
# printed).
set -euo pipefail

TAG="${1:-v8}"
REGISTRY_NS="kubevirt-dev"
IMAGE="memory-tier-operator"

# 1. One-time (idempotent) cluster setup: enable the internal registry
#    with defaultRoute and PVC storage on the default StorageClass.
oc patch configs.imageregistry.operator.openshift.io cluster --type merge \
  -p '{"spec":{"managementState":"Managed","storage":{"pvc":{"claim":null}},"defaultRoute":true}}' >/dev/null

ROUTE="$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}')"
FULL="${ROUTE}/${REGISTRY_NS}/${IMAGE}:${TAG}"
echo "pushing to ${FULL}"

# 2. Push credentials: image-pusher SA with registry-editor in the
#    registry namespace plus the image-builder cluster role.
oc create serviceaccount image-pusher -n "${REGISTRY_NS}" \
  --dry-run=client -o yaml | oc apply -f - >/dev/null
oc create rolebinding image-pusher-registry-editor -n "${REGISTRY_NS}" \
  --clusterrole=registry-editor --serviceaccount="${REGISTRY_NS}:image-pusher" \
  --dry-run=client -o yaml | oc apply -f - >/dev/null
oc create clusterrolebinding image-pusher-image-builder \
  --clusterrole=system:image-builder --serviceaccount="${REGISTRY_NS}:image-pusher" \
  --dry-run=client -o yaml | oc apply -f - >/dev/null

# 3. Build the binaries the Containerfile COPYs in.
if ! command -v go >/dev/null && [ -x "${HOME}/.local/go/bin/go" ]; then
  export PATH="${HOME}/.local/go/bin:${PATH}"
fi
command -v go >/dev/null || { echo "go toolchain required (builds ./operator ./driver)" >&2; exit 1; }
go build -o operator ./cmd/operator
go build -o driver ./cmd/driver

# 4. Build and push the image.
podman build -t "${FULL}" .
printf '%s' "$(oc create token image-pusher -n "${REGISTRY_NS}" --duration=4h)" |
  podman login --tls-verify=false -u image-pusher --password-stdin "${ROUTE}" >/dev/null
podman push --tls-verify=false "${FULL}"
echo "pushed ${FULL}"

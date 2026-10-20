#!/usr/bin/env bash
# Grant image-pull rights for the kubevirt registry namespace to a
# workload namespace's service accounts.
#
# virt-launcher pods run as the VM namespace's default service account,
# and cri-o authenticates internal-registry pulls AS the pod's service
# account — so every namespace that runs VMs needs the puller grant in
# the registry namespace the fork's images live in (kubevirt).
#
# Usage: hack/grant-image-pull.sh <workload-namespace>
set -euo pipefail

NS="${1:?usage: grant-image-pull.sh <workload-namespace>}"
REGISTRY_NS="${REGISTRY_NS:-kubevirt}"
KCTL="${KCTL:-oc}"

"$KCTL" create rolebinding "kubevirt-image-puller-${NS}" -n "$REGISTRY_NS" \
  --clusterrole=system:image-puller \
  --group="system:serviceaccounts:${NS}" \
  --dry-run=client -o yaml | "$KCTL" apply -f -
echo "granted: system:serviceaccounts:${NS} can pull from registry ns ${REGISTRY_NS}"

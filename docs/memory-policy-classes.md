# memory-policy-classes branch direction

This branch is the post-Phase-0 direction for the memory-tier operator:
**administrator-defined memory policy classes** replacing the DRA-claims
path on main. It exists because the Phase 0 evidence (see the gameplan
tracking area) showed the tiering behavior is deliverable with existing
Kubernetes and KubeVirt mechanisms — no DRA, no new API surface — and
because the requirements discussions asked for an infrastructure-owned
QoS model where the workload expresses need, not mechanism.

## What is here

- `api/v1alpha1/memorypolicy_types.go` — the MemoryPolicy CRD
  (cluster-scoped classes: `mode: DRAMOnly | Tiered`, `backing:
  Hugepages | MemLock`, cluster `default`, per-namespace
  allow/forbid/force/default `selection` rules). Deepcopy methods are
  hand-maintained (no controller-gen in this checkout) — keep in sync.
- `config/crd/bases/memorypolicies...yaml` — the CRD manifest.
- `internal/policy/` — the resolution package (pure logic, unit
  tested): class lookup, AAA selection (forced wins; forbidden or
  non-allowed selections are admission rejections, never silent
  downgrades), namespace-default over cluster-default, backing
  defaulting (Hugepages 2Mi).
- `internal/render/` (next commit) — the VMI mutator rendering.

## What is NOT here (deliberately)

- The DRA driver and claims path remain on `main`, deprecated in
  favor of this direction but preserved as working history.
- No scheduler changes, no kubevirt changes: the rendering targets
  existing fields only (`spec.domain.resources` requests/limits,
  `spec.domain.memory.hugepages`, and the KubeVirt 1.10-era
  `spec.domain.memory.memLock: Required`).

## Prerequisites the administrator owns (documented, not automated here)

- Memory Manager Static with `reservedMemory` declared for the DRAM
  NUMA node only (structural DRAM placement for aligned pods —
  measured working for integer- and fractional-CPU Guaranteed pods).
- For Hugepages-backed classes: the node's hugepage pool (boot-time;
  the RT/NFV PerformanceProfile path productizes this).

## Status

Prototype: unit-tested offline, not deployed. Phase 1 validation items
(ledger exhaustion/rejection, capacity release, the enforcement window
relative to first guest-RAM faults) are open and listed in the feature
design document in the gameplan tracking area.

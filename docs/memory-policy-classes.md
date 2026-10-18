# Memory policy classes: unwired prototype, not a replacement service

This branch explores administrator-owned simple workload intent for higher VM
density using CXL. Workload owners express need; infrastructure owns the
mechanism. It does **not** establish that existing fields alone deliver the
complete policy, that DRA must be rejected, or that the backing choice is only
an operational preference.

## What exists

- `api/v1alpha1/memorypolicy_types.go` and the CRD manifest describe classes
  with `DRAMOnly | Tiered`, `Hugepages | MemLock`, cluster defaults and
  namespace allow/forbid/force/default rules. These are proposed API types;
  deepcopy methods are hand-maintained.
- `internal/policy/` resolves names/defaults/forced paths and rejects
  disallowed explicit selections in pure functions. It is namespace-based,
  not user-specific authorization. Cross-class rule conflicts and consistency
  of forced/default versus permission paths need contract/security review.
- `internal/render/` mutates a VMI object in memory. It sets annotations,
  guest backing fields and CPU/memory request/limit shape. Matching VMI
  requests/limits does not by itself prove the final launcher QoS after
  KubeVirt overhead, sidecars and other admission mutations.
- **No admission handler calls these functions.** `cmd/operator/` still
  registers the existing DRA reconciler. The driver remains executable in
  this branch; it is not merely deprecated history on another branch.
- There is no new shared scheduler DRAM budget, synchronous pre-fault
  enforcement contract, VM collector or VMI residency status integration.

## Rendering is not a complete DRAM-only mechanism

The default backing is Hugepages 2Mi in the resolver. That is prototype
behavior, **not a stakeholder product decision**.

| Library option | Actual mutation | Missing obligation |
|---|---|---|
| Hugepages | `spec.domain.memory.hugepages` plus VMI resource shape | DRAM-only pool and actual guest use, consistent final pod resources, shared accounting/overhead, pre-fault placement and lifecycle |
| MemLock | `spec.domain.memory.reservedOverhead.memLock: Required` plus VMI resource shape | Explicit lock request and success, tier placement, shared physical promises, safe failure/security and lifecycle |
| Tiered | Annotations only; leaves guest spec unchanged | This no-op is not a guarantee of CXL eligibility, fixed split, promotion or hot-page retention |

Pinned KubeVirt v1.10.0-beta.0 implementation
`c418f32d6fefd864f19d3a7f436dd6bfa8827036` routes the permission field to
unlimited `RLIMIT_MEMLOCK`, behind Alpha `ReservedOverheadMemlock`; its
ordinary backing converter does not emit `<locked/>`. The MemLock library
branch is therefore **permission configuration, not an implemented locking
backing**. Generic mlock observations cannot validate it.

Kubernetes v1.37.1 `f78e722310e50bcaca9276be22276d9e91d91308` initializes
Memory Manager state for all topology nodes. Omitting a node from
`reservedMemory` reserves zero system memory there, not zero allocatable
memory. Static is not a DRAM allowlist, and fractional CPU supplies no
exclusive-CPU hint. A tested single-topology assignment is not a portable
contract. Final constraints must be effective before backing faults; the
existing driver's asynchronous reconciliation does not establish that order.

A hugepage resource names page size, not DRAM/CXL. A DRAM-only pool can be one
candidate structural partition, with ordinary QEMU/host overhead counted
separately. Pool resize/maintenance and compatible migration capacity must be
reviewed; do not assume every resize always needs a reboot.

## Architecture and measurement work still open

Compare VMI admission, launcher-pod admission, supported plugin/domain hooks,
internal DRA and upstream tier contracts. The pinned Alpha Plugin supports
admission references and GuestDefinition/node hooks, not a generic RenderPod
hook; migration round-trip behavior needs scrutiny. Updating a class does not
automatically permit arbitrary backing changes during migration.

Capacity needs one physical constraint across class aliases and ordinary
consumers, native total-memory accounting and destination migration holds.
Current driver aliases duplicate advertised DRAM without a shared constraint.
NRT publication/scoring alone does not enforce local NUMA tier placement;
Reserve/cache configuration, fail-closed staleness/bypass and node admission
need a coherent contract.

Start observability with a minimal read-only identity-bound bundle/report.
Keep intent, reservation, actual location and service levels separate;
anonymous cgroup counters are not exact guest-RAM or hugetlb residency.
No status service is implemented here.

## Status and next gates

Offline library tests exist (11 policy + 9 render test functions at base
`215af0816bc9350aa6d5f029bd2244592133b24d`). They verify function behavior,
not deployed enforcement or an independently accepted system. This readiness
pass changes documentation/comments only; it does not silently disable,
replace or wire a mechanism.

The gameplan HPSTRAT-40 drafts contain pinned source traces, the revised
feature design, architecture alternatives, measurement contract and two
unexecuted demonstration plans. Stakeholder agreement precedes wiring;
actual-binary no-pressure backing/collector/pre-fault checks precede any
separately authorized pressure experiment. Capacity refusal/release,
restart/migration, multi-CPU-NUMA and physical-CXL workload/SLO validation
remain open. The unconditional runtime gate is withdrawn; owner credential
rotation/history decisions and independent review remain external gates.

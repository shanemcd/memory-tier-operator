# memory-tier-operator

Experimental memory-tiering components for Kubernetes and KubeVirt. The goal is
higher useful VM density with CXL, while allowing administrator-owned workload
policy and a credible DRAM-only option. **This checkout is not a completed or
runtime-validated implementation of that policy.**

## What runs, and what is only a library

- `cmd/operator/` still registers the existing `MemoryTiering`/DRA reconciler;
  `cmd/driver/` publishes discovered memory resources and starts asynchronous
  cgroup-mask reconciliation during claim preparation.
- The `memory-policy-classes` branch also contains class types, a CRD manifest,
  pure selection logic in `internal/policy/` and VMI rendering functions in
  `internal/render/`. As of v10 these are **wired into a live mutating admission
  webhook** (`internal/webhook/`): VMI CREATE in namespaces labeled
  `memorytiering.shanemcd.com/governed=true` is resolved fail-closed (unknown
  or forbidden class ⇒ deny) and rendered onto the VMI; policy identity labels
  are stamped for observability. Deployed and demonstrated on the lab cluster —
  see the design repo's `memory-policy-design.md` and `demo-evidence/`.
  They still do not replace the running DRA path, reserve a new common DRAM
  budget, or implement guest-residency status.
- See [class prototype boundaries](docs/memory-policy-classes.md) before using
  examples. Installation/deployment manifests are experimental assets, not an
  authorization to apply them or a supported production configuration.

## Separate the obligations

A protected-memory promise needs capacity before acceptance, tier-correct
allocation before backing faults, and backing-specific persistence afterward.
A class name, memory request, cgroup mask and current page location answer
separate questions. Permission to use CXL does not require a fixed split or
promise that actively touched pages always remain on DRAM.

`memory.current` and ordinary memory requests are not DRAM-only quotas. Movement
can change location without reducing charge in private-anonymous workloads;
other reclaimable content can reduce charge. Historical pod-own `memory.high`
experiments demoted actively touched data too. Neither a hot-set claim size nor
a class annotation provides a runtime percentage floor.

`cpuset.mems` is an allocation constraint, not a persistence wall. The current
asynchronous prepare/reconcile path does not prove enforcement before QEMU's
first backing faults. CPU/CPU-less discovery heuristics and node-number masks
are lab-shaped, not a general tier classifier.

## Backing candidates, not interchangeable implementations

- A DRAM-only hugetlb pool and matching KubeVirt guest hugepage resource/backing
  are one candidate. `hugepages-2Mi` names page size, not tier. Pool locality,
  actual guest allocation, ordinary QEMU/host overhead, capacity release and
  migration overlap must be checked. Generic hugepage wall observations are
  not validation of this complete operator policy.
- The class renderer's `MemLock` option writes
  `spec.domain.memory.reservedOverhead.memLock: Required`. In pinned KubeVirt
  v1.10.0-beta.0 (`c418f32d6fefd864f19d3a7f436dd6bfa8827036`), this Alpha-gated
  field raises `RLIMIT_MEMLOCK`; it does **not** request `<locked/>` ordinary
  guest backing. A real explicit lock-request route, initial tier placement,
  shared capacity, safe failure and lifecycle behavior are additional work.
- Memory Manager Static is a NUMA planner, not a tier selector. Kubernetes
  v1.37.1 (`f78e722310e50bcaca9276be22276d9e91d91308`) includes all topology
  nodes; omission from `reservedMemory` means zero system reserve, not
  exclusion. Observed node-0 assignments do not establish portable DRAM-only
  placement. Fractional CPU supplies no exclusive-CPU hint in that path.

No backing is selected for the product by the library's defaulting behavior.
Supported fields, plugin/admission integration, internally injected DRA and
upstream tier contracts remain architecture alternatives. Avoiding workload
implementation details does not categorically reject internal DRA.

## Shared capacity and observability gaps

The driver publishes `dram-0` and `tiered-0` with the same full physical DRAM
capacity, without shared counters/native-resource mapping. Independent alias
budgets can overpromise common memory. Ordinary native memory accounting is
not a DRAM guarantee when system-RAM CXL is included; it also does not
transparently account every nonclaim DRAM consumer for a separate DRA promise.
API features for consumable capacity, shared counters and native-resource
mapping require a coherent version/gate/accounting design, not field presence.

Cgroup anonymous per-node counters are not guest-RAM-only residency and are
not complete hugetlb/file/memfd accounting. Separate intent, reservations,
actual backing/location, overhead and unknown coverage before integrating a
production status API. NRT publishes locality information; scoring a worker
is not choosing/enforcing a NUMA tier inside it.

## Offline checks and evidence scope

From a recorded source tree, without cluster access:

```sh
go build ./...
go test ./internal/policy/ ./internal/render/
```

The two packages have 20 test functions at baseline `215af0816bc9350aa6d5f029bd2244592133b24d`.
Build/tests prove the stated library behavior, not admission wiring, correct
physical budgets, final launcher QoS, guest locking or runtime VM guarantees.
Record working-tree differences/digests as well as HEAD when testing local
changes; do not label dirty source as exactly a clean commit.

Historical evidence is in the gameplan HPSTRAT-40 tracking area's
`experiments/poc-phase0/` and earlier lab records. Source-corrected design,
architecture alternatives, measurement contract, emulated functional matrix
and physical-CXL value plan are in that area's `drafts/` directory. The
unconditional Phase 0 runtime gate remains withdrawn. Emulated CXL is backed
by host RAM without host-NUMA binding and uses a test-only cache-invalidation
bypass: no physical latency/bandwidth/coherency, equal-SLO density or economic
claim follows from those experiments.

Further work requires stakeholder agreement before implementation wiring,
separate runtime/pressure authorization, chosen support versions/backing,
physical hardware/workload/SLO owners and independent review. Credential
rotation/history exposure follow-up remains with owners; no such action is
automatically authorized by this prototype or its tests.

## License

Apache 2.0

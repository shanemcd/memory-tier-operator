# memory-tier-operator

Host-transparent memory tiering for Kubernetes: let workloads claim
DRAM or CXL memory as schedulable resources, with kernel-enforced
placement and an optional hard wall.

## The model

The kernel has two orthogonal memory axes, and this operator exposes
both:

**Placement** — where pages are *born*. Set via `cpuset.mems` on the
workload's cgroup: first-touch allocation goes to the local (DRAM)
node, and falls back to the CXL node when DRAM is full (if the mask
allows it).

**Persistence** — whether pages can *move after birth*. Ordinary
memory is managed by the kernel: hot pages stay on DRAM, cold pages
demote to CXL under memory pressure. Hugepage-backed memory is a hard
wall: it is never on any LRU list, so the kernel cannot reclaim or
demote it.

Every tier is a composition of these two axes. Nothing else is
invented.

## The tiers

| Tier | Claim | VM persistence | What you get |
|---|---|---|---|
| default | *none* | ordinary | Cold tail demotes to CXL under pressure (84% measured). No API needed. |
| `cxl-tier` | capacity on CXL | ordinary | Born on CXL, never promotes. Density-first. |
| `tiered` | hot-set on DRAM | ordinary | Born on DRAM, falls back to CXL, kernel splits by access behavior (87% measured). |
| `dram-tier` + wall | capacity on DRAM | hugepages | Born on DRAM **and** never demotable. The hard wall. |

The `tiered` claim size is the **hot-set budget**: an admission-time
reservation on the node's DRAM ledger. The kernel enforces the actual
split by access pattern — pages being touched stay on DRAM, cold
pages spill — so the budget is a reservation, not a runtime guarantee.

The `dram-tier` + hugepages combination is the only hard wall: the
claim gates placement (`cpuset.mems=0`) and the hugepage backing gates
persistence (never on LRUs). Both knobs are needed — a wall without a
placement gate pins pages wherever they happened to fault, and a
placement gate without a wall lets cold pages demote.

## Installing

```sh
kubectl apply -f deploy/install.yaml
```

This creates the CRD, RBAC, and the operator deployment. Create a
`MemoryTiering` CR to reconcile the DeviceClasses and the driver
DaemonSet:

```yaml
apiVersion: memorytier.shanemcd.com/v1alpha1
kind: MemoryTiering
metadata:
  name: cluster
  namespace: memory-tier-operator
spec:
  driverImage: <image>
```

The operator creates the `dram-tier`, `cxl-tier`, and `tiered`
DeviceClasses and a privileged DaemonSet that discovers each node's
NUMA/CXL topology, publishes it as DRA ResourceSlices, and enforces
placement per claim.

## Using tiers

### Pods

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: dram-worker
spec:
  containers:
  - name: work
    image: ...
    resources:
      requests:
        cpu: "1"
  resourceClaims:
  - name: memory
    resourceClaimTemplateName: dram-worker-memory
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: dram-worker-memory
spec:
  spec:
    devices:
      requests:
      - name: memory
        exactly:
          deviceClassName: dram-tier
          allocationMode: All
          capacity:
            requests:
              memorytier.openshift.io/memory: 1Gi
```

Verify on the node:

```sh
cat /sys/fs/cgroup/kubepods.slice/*/kubepods-*pod<uid>.slice/cpuset.mems
# "0" for dram-tier, "1" for cxl-tier, "0-1" for tiered
```

### Virtual machines (KubeVirt)

VMs use the standard `spec.resourceClaims` field — no KubeVirt
modifications required. The combinations:

| Claim | VM memory backing | Guest RAM placement | Guest RAM persistence | QEMU overhead |
|---|---|---|---|---|
| none | ordinary | DRAM (first-touch) | demotes under pressure | demotes |
| `cxl-tier` | ordinary | CXL only | terminal tier, never promotes | demotes |
| `tiered` | ordinary | DRAM, fallback to CXL | hot stays, cold demotes (87%) | demotes |
| `dram-tier` | ordinary | DRAM only | hot stays, cold demotes | demotes |
| `dram-tier` | hugepages | DRAM only | **hard wall — never demotes** | demotes (not hugepage-backed) |
| `tiered` | hugepages | DRAM, fallback CXL | hard wall, but may land on CXL if DRAM is full at fault time | demotes |
| `cxl-tier` | hugepages | CXL only | hard wall on CXL | demotes |

The `dram-tier` + hugepages row is the tier-100 recipe. Example:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: my-vm-dram-wall
spec:
  spec:
    devices:
      requests:
      - name: memory
        exactly:
          deviceClassName: dram-tier
          allocationMode: All
          capacity:
            requests:
              memorytier.openshift.io/memory: 1Gi
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: pinned-vm
spec:
  runStrategy: Always
  template:
    spec:
      domain:
        memory:
          guest: 1Gi
          hugepages:
            pageSize: "2Mi"
        resources:
          requests:
            memory: "1Gi"
      resourceClaims:
      - name: memory-tier
        resourceClaimTemplateName: my-vm-dram-wall
      volumes: ...
```

Requirements for the hugepage wall:

- The node must have hugepages provisioned **on the DRAM NUMA node**
  (`/sys/devices/system/node/node0/hugepages/hugepages-2048kB/nr_hugepages`).
  The pool placement is the placement half of the wall; provisioning
  on the CXL node would wall the VM onto the slow tier.
- The node must register `hugepages-2Mi` as an extended resource
  (kubelet detects sysfs; a restart picks up runtime allocation).
- KubeVirt requests the hugepages resource automatically when
  `domain.memory.hugepages` is set.

Measured: a 1 GiB hugepage-backed VM under node pressure that demoted
4.4 GiB across the node — the guest RAM (hugetlb pool, node 0) never
moved; QEMU's ordinary overhead (not hugepage-backed) demoted
normally. Persistence is per-allocation, not per-cgroup.

## Design notes

### Capacity accounting

- **DRAM**: the DRA claim is a placement gate. The stock scheduler's
  pod memory request remains the binding constraint on DRAM usage.
- **CXL**: the DRA pool is the only capacity accounting — CXL memory
  is invisible to the stock scheduler (not in node allocatable).
  Claims decrement the ledger; exhaustion blocks scheduling without
  substitution.

### Kernel facts (measured)

- Demotion is node-level and global. It explicitly ignores cgroup
  cpusets (`mm/vmscan.c`: "Demotion ignores all cpuset and mempolicy
  settings"). The mask gates allocation, not migration.
- Hot pages never demote — reclaim only selects unreferenced pages.
  The latency-relevant working set stays on DRAM regardless of claims.
- `memory.min` does not floor node residency: protection is
  charge-based, demotion is charge-neutral. A cgroup under its floor
  by residency is never below it by charge; the reclaim scan walks
  through.
- `mlock` is a per-process wall (unevictable pages never enter the
  reclaim scan), but cannot be applied to another process's memory.
  For VMs, the equivalent is QEMU's locked memory backing — not
  exposed by KubeVirt today; hugepages are the available wall.
- `numa_demotion_enabled` ships `false` on RHEL. The driver enables
  it on nodes with a CXL tier. Setting it to `false` on a node makes
  every mask absolute (nothing ever moves) at the cost of all tiering.
- Guest-hot pages are visible to the host's aging via EPT accessed-bit
  folding: a VM's actively-touched memory stays on DRAM under pressure
  while its cold tail spills.

### The driver

A per-node DaemonSet that:

1. Discovers the NUMA/CXL topology from `/sys` (CPU nodes = DRAM
   tier; CPU-less nodes with committed CXL regions = CXL tier)
2. Publishes it as DRA ResourceSlices (byte-granular capacity, shared
   devices, one pool per node)
3. Registers with the kubelet as a DRA plugin
4. On claim preparation, writes `cpuset.mems` on the pod's cgroup —
   the placement axis — and maintains it against kubelet cgroup
   setup via a reconciler

The `tiered` device is published only on nodes with a CXL tier, so
tiered claims cannot schedule on DRAM-only nodes.

### What is deliberately not here

- No webhook, annotation, or secondary API — `spec.resourceClaims`
  is the one way to express tier intent
- No userspace page-migration daemon — the kernel's own reclaim and
  demotion machinery handles runtime placement
- No per-VM runtime floor — `memory.min` does not provide one (see
  kernel facts), and a daemon-based controller was evaluated and
  rejected on performance grounds elsewhere

## Status

Alpha. Developed and verified against QEMU-emulated CXL Type-3 devices
(a CPU-less NUMA node with a committed region, on a custom kernel with
`CONFIG_CXL_REGION_INVALIDATION_TEST=y`). All tier combinations have
been verified end-to-end on stock KubeVirt. A per-VM runtime
percentage floor (the ratio between tier-100 and tiered) would require
either a per-memory-backend `locked` property in QEMU upstream or a
userspace controller; neither exists today.

## License

Apache 2.0

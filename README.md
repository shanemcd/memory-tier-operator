# memory-tier-operator

Host-transparent memory tiering for Kubernetes: expose DRAM and
CXL-attached memory as separately claimable resources, and enforce the
claimed tier on the workload's memory.

## What it does

Most servers have more memory than their workloads actually touch. A
host with 1 TiB of DRAM running 12 × 64 GiB VMs is "full," while the
hot set might be 300 GiB. CXL-attached memory changes the economics —
it's ~2–3× slower than DRAM but approaching disk pricing — and the
Linux kernel already knows how to place pages on specific NUMA nodes.

This operator bridges Kubernetes scheduling and Linux page placement:

1. **Discovery**: a per-node daemon reads the NUMA topology
   (`/sys/devices/system/node/*/`) and identifies CXL-attached memory
   (online memory on CPU-less NUMA nodes with a committed CXL region).
2. **Publishing**: each node's tiers are published as
   [DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
   ResourceSlices — a DRAM device and a CXL device, each with
   byte-granular capacity (`allowMultipleAllocations` +
   `DRAConsumableCapacity`, Kubernetes ≥ 1.36).
3. **Scheduling**: the in-tree scheduler plugin evaluates claims against
   the slices. A pod requesting `dram-tier` with a 1 Gi capacity
   decrements the DRAM pool; when the pool is exhausted, the pod stays
   Pending — the scheduler does not silently substitute CXL for DRAM.
4. **Enforcement**: when the kubelet asks the driver to prepare a claim,
   the driver writes the claimed tier's NUMA mask (`cpuset.mems`) on the
   pod's cgroup. The kernel then keeps the pod's pages inside the
   promised tier — verified to hold under memory pressure.

```
pod claims 1Gi dram-tier
  → scheduler: DRAM pool has capacity → allocate, pin to node
    → kubelet: NodePrepareResources(claim)
      → driver: cpuset.mems=0 on the pod's cgroup
        → kernel: anonymous pages land on NUMA node 0 only
```

## Installing

Prerequisites:
- Kubernetes ≥ 1.36 with `DRAConsumableCapacity` (beta, on by default)
- CXL-attached memory onlined as System RAM on its own NUMA node
  (the standard configuration for CXL Type-3 memory expanders)

```sh
kubectl apply -f deploy/install.yaml
kubectl apply -f deploy/example.yaml
```

The operator creates the `dram-tier`, `cxl-tier`, and `tiered` DeviceClasses and
the driver DaemonSet. On nodes with CXL memory, you should see a
ResourceSlice appear:

```sh
kubectl get resourceslices
```

## Using tiers

A pod claims a tier by referencing the DeviceClass:

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
      # Note: the claim replaces the memory request as the scheduling
      # signal for tiered workloads
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
              memorytier.openshift.io/memory: 1Gi    # the driver's capacity domain
```

Verify the placement on the node:

```sh
# find the pod's cgroup and check the mask + per-node pages
cat /sys/fs/cgroup/kubepods.slice/*/kubepods-*pod<uid>.slice/cpuset.mems
grep anon /sys/fs/cgroup/kubepods.slice/*/kubepods-*pod<uid>.slice/memory.numa_stat
```

For DRAM-only claims: `cpuset.mems` reads `0` (the DRAM NUMA node) and
`anon N0=<size> N1=0`. For CXL-only: mask `1`, pages on node 1.

### The ratio view (how much of a VM's memory is fast?)

The three classes answer one question with increasing strictness —
"what fraction of this workload's memory must be on fast memory?" —
and the honest answer comes in two layers:

- **The claim** is admission control and allocation policy: how much
  DRAM hot-set budget you are entitled to, and where new pages may be
  born.
- **The kernel** decides the actual split, by real access behavior:
  hot pages stay on DRAM, cold pages demote to CXL under memory
  pressure. This layer is global — it applies to every workload on
  the node, claimed or not.

Measured on the lab (a 2 GiB Fedora VM whose guest workload holds a
120 MiB hot set and a 480 MiB cold tail, claimed as `tiered`):

```sh
# One bounded reclaim on the virt-launcher cgroup demotes the cold
# tail; the kernel stops at the requested amount.
echo 800M > /sys/fs/cgroup/.../virt-launcher-.../memory.reclaim
grep anon .../memory.numa_stat
# round 1: anon N0=462MiB N1=796MiB   (63% on CXL)
# round 2: anon N0=162MiB N1=1097MiB  (87% on CXL — the cold tail)
```

The DRAM floor converged to the actual hot set (162 MiB ≈ the 120 MiB
hot loop plus a minimal active set) while the guest kept running
normally. The ratio is therefore *dialable by pressure* and *bounded
by behavior*: a workload with a 20% hot set ends up ~80% on CXL, which
is the point — DRAM hosts hot sets, CXL absorbs cold tails.

Mapping the ratio vocabulary onto the classes:

| Ratio intent | Class | What you actually get |
|---|---|---|
| 100% fast | `dram-tier` | Pages are *born* on DRAM and hot pages *stay* there; cold pages may still be demoted by the kernel (there is no per-workload migration fence) |
| N% fast (0<N<100) | `tiered`, claim = N% of guest RAM | A DRAM hot-set reservation (admission) plus allocation fallback; the kernel keeps the hot set on DRAM and spills the cold tail |
| Don't care | no claim at all | DRAM-first allocation by default, cold tail demotes anyway — the default behavior needs no API (measured: 84% of an unclaimed VM's cold memory demoted) |

The one thing no class can promise is a *hard* per-workload floor on
DRAM residency for cold pages: the kernel's demotion path explicitly
ignores cgroup cpusets (mm/vmscan.c: "Demotion ignores all cpuset and
mempolicy settings"), so genuinely cold pages of any workload can
move. What is guaranteed is the part that matters for latency: pages
being touched stay on DRAM.

Measured, the per-VM policy matrix is:

| Mechanism | Gates allocation | Gates demotion |
|---|---|---|
| `cpuset.mems` | yes (birth node, fallback) | no — ignored by design |
| `memory.min` | no | no — protection is charge-based, demotion is charge-neutral |
| `mlock` (unevictable) | no | **yes — a hard wall** (never enters the reclaim scan) |
| `numa_demotion_enabled=false` | no | yes, but host-wide |

`memory.min` looks like it should floor residency and doesn't: a
cgroup's usage stays charged wherever its pages live, so it never
registers as "below min" and the reclaim scan walks through the floor
(measured under both direct reclaim and node pressure). `mlock` is the
real wall — a 600 MiB locked allocation held completely still on DRAM
under pressure that demoted 4.4 GiB across the node — but it is binary
per allocation, so it gives you tier-100, not a ratio. For VMs it maps
to QEMU's locked memory backing (a small KubeVirt API addition; not
exposed today) or hugepages (supported natively).

### Tiered claims

The `tiered` class spans both nodes: the claim is a DRAM hot-set
budget, the enforcement mask covers DRAM **and** CXL, and the kernel
demotes cold pages to the CXL node under memory pressure while hot
pages stay on DRAM:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: tiered-worker-memory
spec:
  spec:
    devices:
      requests:
      - name: memory
        exactly:
          deviceClassName: tiered
          allocationMode: All
          capacity:
            requests:
              memorytier.openshift.io/memory: 1Gi    # the hot-set bound
```

The tiered device is published only on nodes with a CXL tier, so
tiered claims cannot land on DRAM-only nodes. Size the claim for the
hot set, not the peak: the workload's cold tail spills into the CXL
demotion pool automatically. DRAM-only claims remain structurally
protected — a page can only demote within its cgroup's allowed nodes,
so a `mems=0` pod never leaks onto CXL.

The driver enables the kernel's demotion path
(`/sys/kernel/mm/numa/demotion_enabled`) on CXL-capable nodes at
startup — it ships disabled by default.

## KubeVirt integration

Virtual machines use the standard DRA `spec.resourceClaims` field —
**no KubeVirt modifications required**. A VM that wants its memory on
a specific tier writes a claim reference directly:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
spec:
  template:
    spec:
      domain:
        memory:
          guest: 512Mi
      resourceClaims:
      - name: memory-tier
        resourceClaimTemplateName: my-vm-cxl-512mi
      volumes: ...
```

The referenced ResourceClaimTemplate is created in the VM's namespace
(by the user, or by an operator):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: my-vm-cxl-512mi
spec:
  spec:
    devices:
      requests:
      - name: memory
        exactly:
          deviceClassName: cxl-tier
          allocationMode: All
          capacity:
            requests:
              memorytier.openshift.io/memory: 512Mi
```

The scheduler places the VM only on nodes with free CXL capacity, and
the driver enforces `cpuset.mems` on the virt-launcher pod. QEMU's
guest memory lands on the promised NUMA node.

There is deliberately no annotation, webhook, or other secondary API:
`spec.resourceClaims` is the one way to express tier intent.

## Design notes

### What the tiers do

The driver publishes what it finds — no pool sizing, no capacity
knobs:

- The **DRAM device** carries the node's full DRAM. The pod's memory
  request already tracks usage through the stock scheduler; the DRA
  claim is a placement gate (which nodes run the driver), not a
  capacity system.
- The **CXL device** carries the node's full CXL memory. CXL sits on a
  separate NUMA node that the stock scheduler doesn't count against
  allocatable, so the DRA pool is its only capacity accounting. Claims
  decrement it; when it's exhausted, the scheduler stops placing
  CXL-opted workloads on that node.

The MemoryTiering spec is operational only (`driverImage`,
`nodeSelector`) — it does not slice or partition memory.

### Other implementation notes

- **The reconciler**: the kubelet clears `cpuset.mems` on pod cgroups
  during its own setup, after the DRA prepare call. The driver therefore
  runs a per-claim reconciler that re-writes the mask on drift (a
  one-shot write loses the race — this was verified empirically).
- **The driver runs privileged**: it needs the kubelet plugin
  directories (registration + checkpoint state) and cgroup write access.
  On OpenShift, grant the `privileged` SCC to the driver's service
  account.
- **Attribute names must be C identifiers**: the driver publishes its
  attributes under the `memorytier.openshift.io` domain (the identifier
  the current build registers with the kubelet) — attribute names use
  underscores (`numa_node`), not hyphens, which the API server rejects.

## Status

Alpha. Developed and verified against QEMU-emulated CXL Type-3 devices
(a CPU-less NUMA node with committed region, on a custom kernel with
`CONFIG_CXL_REGION_INVALIDATION_TEST=y`). The operator, driver, and
KubeVirt integration have been verified end-to-end: DRAM-only and
CXL-only pods and VMs place their memory on the promised tier, the
placement holds under memory pressure, and pool exhaustion correctly
blocks scheduling without substitution. Tiered claims have been
verified at the VM level: a tiered VM's cold guest RAM demotes to the
CXL node under reclaim while a DRAM-only VM on the same node under
the same pressure keeps its memory on DRAM.

Known limitation: a pod's first page faults can land on the CXL node
(before the enforcement mask is applied, under full DRAM pressure) —
bounded by the early-boot footprint.

Not yet implemented: live migration of tier intent, metrics/alerts,
and OLM packaging.

## License

Apache 2.0 — see [LICENSE](LICENSE).

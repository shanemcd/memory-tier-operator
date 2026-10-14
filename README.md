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

The operator creates the `dram-tier` and `cxl-tier` DeviceClasses and
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

## KubeVirt integration

A [KubeVirt fork](https://github.com/shanemcd/kubevirt/tree/memory-tier)
(branch `memory-tier`) adds a VM-level API so virtual machines can opt
in without writing claims by hand:

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
spec:
  template:
    spec:
      domain:
        memory:
          guest: 256Mi
          tier: DRAM-only   # or CXL-only
```

virt-controller translates the tier into a ResourceClaimTemplate
carrying the guest RAM as capacity; the VM schedules only where the
tier is available, and QEMU's guest memory lands on the promised NUMA
node.

## Design notes

### Capacity accounting: two tiers, two models

The DRAM and CXL tiers use fundamentally different capacity models:

**DRAM tier** — the stock scheduler already tracks DRAM usage via
`resources.requests.memory` against the node's allocatable. A DRAM-tier
claim does not replace that accounting; the pod's memory request carries
the workload's real usage. The DRA claim is a *placement gate* — it
ensures the pod lands on a node running the memory-tier driver — not a
capacity system. The driver therefore exposes the node's **full DRAM**
as the pool by default, and the stock scheduler is the binding
constraint.

Setting `dramPoolSize` explicitly creates a *placement quota*: it limits
how many DRAM-opted workloads can pile onto one node, even when DRAM is
available. This is useful for spreading DRAM-sensitive workloads across
nodes.

**CXL tier** — CXL memory is on a separate NUMA node that the stock
scheduler does not count against allocatable. No other system tracks
it. The DRA pool *is* the capacity accounting for CXL: the driver
exposes the node's actual CXL capacity, and claims decrement it. The
pod's memory request carries only the launcher overhead (CPU, page
tables), not the guest RAM that lives on the CXL node.

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
blocks scheduling without substitution.

Not yet implemented: `tiered` (union-mask) claims, live migration of
tier intent, metrics/alerts, and OLM packaging.

## License

Apache 2.0 — see [LICENSE](LICENSE).

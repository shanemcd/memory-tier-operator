// Package draplugin implements the kubelet-facing DRA plugin: the
// enforcement point where tier intent becomes page placement.
//
// PrepareResourceClaims receives the allocated claims for a pod about to
// start on this node. It maps each allocated device to its NUMA node (the
// driver's own published topology), finds the pod's cgroup, and writes
// cpuset.mems so the kernel's first-touch and demotion machinery keeps the
// pod's pages inside the promised tier.
package draplugin

import (
	"context"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
)

// Attribute names published by the driver's slices.
const (
	AttrType     = "memorytier.openshift.io/type"
	AttrNumaNode = "memorytier.openshift.io/numa_node"
	AttrDomain   = "memorytier.openshift.io"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	// The kubelet's pod cgroups live under a QoS sub-slice; UIDs are
	// dash-escaped to underscores in slice names.
	podSlicePattern = "kubepods-*-pod%s.slice"
	cpusetMems      = "cpuset.mems"
	cgroupPoll      = 250 * time.Millisecond
)

// Plugin implements kubeletplugin.DRAPlugin.
type Plugin struct {
	NodeName string
	Client   kubernetes.Interface

	// DeviceNodes maps published device names to their NUMA node
	// (maintained by the discovery loop; e.g. "dram-0" -> 0, "cxl-0" -> 1).
	DeviceNodes map[string]int

	mu       sync.Mutex
	Prepared map[types.UID]*PreparedInfo
}

// PreparedInfo records the enforcement applied for a claim.
type PreparedInfo struct {
	PodUID types.UID
	Mask   string
	stop   chan struct{}
}

// PrepareResourceClaims is the enforcement point.
func (p *Plugin) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	logger := klog.FromContext(ctx)
	out := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		result := kubeletplugin.PrepareResult{Devices: []kubeletplugin.Device{}}

		// Union of NUMA nodes across the claim's allocated devices.
		nodes := []int{}
		for _, res := range claim.Status.Allocation.Devices.Results {
			if node, ok := p.DeviceNodes[res.Device]; ok {
				nodes = append(nodes, node)
			} else {
				logger.Info("allocated device not in published topology", "device", res.Device)
			}
		}
		mask := nodesToMask(nodes)

		// Find the pod that references this claim (direct reference;
		// ResourceClaimTemplate references carry the claim via the
		// generated name and resolve the same way).
		pod, err := p.podForClaim(ctx, claim)
		if err != nil {
			logger.Error(err, "cannot find pod for claim; enforcement deferred to idempotent re-prepare", "claim", claim.Name)
			out[claim.UID] = result
			continue
		}

		logger.Info("prepare",
			"claim", claim.Name,
			"namespace", claim.Namespace,
			"pod", pod.Name,
			"nodes", nodes,
			"mask", mask,
		)

		if mask != "" && pod != nil {
			// A one-shot write loses the race: the kubelet's own cgroup
			// setup runs AFTER prepare and clears cpuset.mems back to empty
			// (its unpinned state). Enforcement must be a reconciler that
			// re-writes the mask whenever it drifts, for the claim's
			// lifetime. It also covers the cgroup not existing yet.
			info := &PreparedInfo{PodUID: pod.UID, Mask: mask, stop: make(chan struct{})}
			p.mu.Lock()
			if old := p.Prepared[claim.UID]; old != nil {
				close(old.stop)
			}
			p.Prepared[claim.UID] = info
			p.mu.Unlock()
			go p.reconcileMask(claim.UID, info)
		}
		out[claim.UID] = result
	}
	return out, nil
}

// UnprepareResourceClaims rolls the enforcement back (restores the full
// node mask) — best effort; the cgroup usually dies with the pod anyway.
func (p *Plugin) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	logger := klog.FromContext(ctx)
	out := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		p.mu.Lock()
		info := p.Prepared[claim.UID]
		delete(p.Prepared, claim.UID)
		p.mu.Unlock()
		if info != nil {
			close(info.stop)
			full := p.fullMask()
			if err := writeMems(podCgroupPath(info.PodUID), full); err == nil {
				logger.Info("unprepare: mask restored (reconciler stopped)", "claim", claim.Name, "mask", full)
			}
		} else {
			logger.Info("unprepare", "claim", claim.Name, "namespace", claim.Namespace)
		}
		out[claim.UID] = nil
	}
	return out, nil
}

// HandleError processes background errors (slice publishing etc.).
func (p *Plugin) HandleError(ctx context.Context, err error, msg string) {
	klog.FromContext(ctx).Error(err, msg)
}

// podForClaim lists pods in the claim's namespace and finds the one that
// references the claim by name, scheduled to this node.
func (p *Plugin) podForClaim(ctx context.Context, claim *resourceapi.ResourceClaim) (*corev1.Pod, error) {
	pods, err := p.Client.CoreV1().Pods(claim.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", claim.Namespace, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != p.NodeName {
			continue
		}
		for _, ref := range pod.Spec.ResourceClaims {
			if ref.ResourceClaimName != nil && *ref.ResourceClaimName == claim.Name {
				return pod, nil
			}
			// Claims materialized from a ResourceClaimTemplate get a
			// generated name (<pod>-<claim-ref>-<suffix>) and the pod's
			// resourceClaimName back-reference is populated only after
			// preparation — match the generated prefix instead.
			if strings.HasPrefix(claim.Name, pod.Name+"-"+ref.Name+"-") {
				return pod, nil
			}
		}
	}
	return nil, fmt.Errorf("no pod on %s references claim %s/%s", p.NodeName, claim.Namespace, claim.Name)
}

// reconcileMask keeps the pod cgroup's cpuset.mems equal to the promised
// mask: it waits for the cgroup to appear, survives the kubelet's own
// cgroup setup clearing the value, and exits when the claim is unprepared
// or the cgroup disappears (pod deleted).
func (p *Plugin) reconcileMask(claimUID types.UID, info *PreparedInfo) {
	logger := klog.Background().WithValues("claimUID", claimUID, "podUID", info.PodUID, "mask", info.Mask)
	path := ""
	for {
		select {
		case <-info.stop:
			return
		default:
		}
		if path == "" {
			path = podCgroupPath(info.PodUID)
			if path == "" {
				// Not created yet — or the pod is gone. Distinguish by
				// age: give up quietly after the wait window.
				time.Sleep(cgroupPoll)
				continue
			}
			logger.Info("pod cgroup found; enforcing", "path", path)
		}
		cur, err := os.ReadFile(filepath.Join(path, cpusetMems))
		if err != nil {
			// Cgroup removed: pod is gone.
			logger.Info("pod cgroup gone; reconciler exiting")
			return
		}
		if strings.TrimSpace(string(cur)) != info.Mask {
			if err := os.WriteFile(filepath.Join(path, cpusetMems), []byte(info.Mask), 0o644); err != nil {
				logger.Error(err, "cpuset.mems write failed; retrying")
			} else {
				logger.Info("cpuset.mems enforced")
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// fullMask returns "0-1"-style mask over all known nodes.
func (p *Plugin) fullMask() string {
	nodes := make([]int, 0, len(p.DeviceNodes))
	for _, n := range p.DeviceNodes {
		nodes = append(nodes, n)
	}
	return nodesToMask(nodes)
}

// podCgroupPath resolves the pod's cgroup v2 directory, or "" if absent.
func podCgroupPath(podUID types.UID) string {
	escaped := strings.ReplaceAll(string(podUID), "-", "_")
	// QoS layer first (the common case), then directly under kubepods.
	for _, pattern := range []string{
		filepath.Join(cgroupRoot, "kubepods.slice", "*", fmt.Sprintf(podSlicePattern, escaped)),
		filepath.Join(cgroupRoot, "kubepods.slice", fmt.Sprintf(podSlicePattern, escaped)),
	} {
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

// writeMems writes cpuset.mems into the cgroup directory.
func writeMems(cgroupDir, mask string) error {
	if cgroupDir == "" {
		return fmt.Errorf("empty cgroup path")
	}
	path := filepath.Join(cgroupDir, cpusetMems)
	return os.WriteFile(path, []byte(mask), 0o644)
}

// nodesToMask turns {0} -> "0", {1} -> "1", {0,1} -> "0-1".
func nodesToMask(nodes []int) string {
	if len(nodes) == 0 {
		return ""
	}
	unique := map[int]bool{}
	for _, n := range nodes {
		unique[n] = true
	}
	sorted := make([]int, 0, len(unique))
	for n := range unique {
		sorted = append(sorted, n)
	}
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, n := range sorted {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

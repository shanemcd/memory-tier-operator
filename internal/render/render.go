// Package render applies a resolved memory policy class to a VMI.
//
// Pure logic, unit-tested offline. The rendering targets EXISTING
// KubeVirt fields only — the zero-new-API route measured in Phase 0:
//
//   - DRAMOnly renders the Guaranteed resource shape (requests ==
//     limits for cpu and memory — fractional CPU is fine and still
//     Guaranteed; the Memory Manager then places the pod on the
//     reservedMemory-declared DRAM node) plus the class backing:
//     hugepages (spec.domain.memory.hugepages) or memory locking
//     (spec.domain.memory.memLock: Required — the KubeVirt 1.10
//     field). Both are measured persistence walls.
//   - Tiered renders nothing on the spec — the kernel's demotion is
//     the mechanism — and records the decision on annotations for
//     observability.
//
// The class owns the mechanism: conflicting user-specified settings
// (a different backing, or a non-Guaranteed resource shape) are
// errors — never silently overwritten, never silently downgraded.
package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	k8sv1 "k8s.io/api/core/v1"

	memoryv1alpha1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	"github.com/shanemcd/memory-tier-operator/internal/policy"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// Annotation keys recording the rendering outcome (observability).
const (
	RenderedPolicyAnnotation = "memorytiering.shanemcd.com/rendered-policy"
	RenderedSourceAnnotation = "memorytiering.shanemcd.com/rendered-source"
)

// defaultCPU is the request/limit the renderer sets when the VMI
// specifies none: it only needs to make the pod shape Guaranteed
// (requests == limits), not to reserve real CPU.
const defaultCPU = "100m"

// RenderVMI applies the resolved class to the VMI spec.
func RenderVMI(vmi *kubevirtv1.VirtualMachineInstance, r *policy.Resolved) error {
	if vmi == nil || r == nil || r.Policy == nil {
		return fmt.Errorf("render: nil vmi or unresolved policy")
	}
	if vmi.Annotations == nil {
		vmi.Annotations = map[string]string{}
	}
	vmi.Annotations[RenderedPolicyAnnotation] = r.Policy.Name
	vmi.Annotations[RenderedSourceAnnotation] = string(r.Source)

	switch r.Policy.Spec.Mode {
	case memoryv1alpha1.Tiered:
		// Explicit no-op on the spec: kernel demotion is the
		// mechanism. User-specified backing on a tiered VM is
		// user-owned mechanism and is left alone.
		return nil
	case memoryv1alpha1.DRAMOnly:
		return renderDRAMOnly(vmi, r)
	default:
		return fmt.Errorf("render: unknown mode %q", r.Policy.Spec.Mode)
	}
}

// memLockOf reads spec.domain.memory.reservedOverhead.memLock.
func memLockOf(mem *kubevirtv1.Memory) *kubevirtv1.MemLockRequirement {
	if mem == nil || mem.ReservedOverhead == nil {
		return nil
	}
	return mem.ReservedOverhead.MemLock
}

// setMemLock writes spec.domain.memory.reservedOverhead.memLock.
func setMemLock(mem *kubevirtv1.Memory, v kubevirtv1.MemLockRequirement) {
	if mem.ReservedOverhead == nil {
		mem.ReservedOverhead = &kubevirtv1.ReservedOverhead{}
	}
	mem.ReservedOverhead.MemLock = &v
}

func renderDRAMOnly(vmi *kubevirtv1.VirtualMachineInstance, r *policy.Resolved) error {
	if err := renderBacking(vmi, r.DefaultedBacking()); err != nil {
		return err
	}
	return renderGuaranteedShape(vmi)
}

// renderBacking sets the persistence wall. The class owns it: an
// existing user-specified conflicting backing is an error.
func renderBacking(vmi *kubevirtv1.VirtualMachineInstance, b memoryv1alpha1.DRAMBacking) error {
	mem := vmi.Spec.Domain.Memory
	if mem == nil {
		mem = &kubevirtv1.Memory{}
		vmi.Spec.Domain.Memory = mem
	}
	switch b.Type {
	case memoryv1alpha1.HugepagesBacking:
		if ml := memLockOf(mem); ml != nil && *ml == kubevirtv1.MemLockRequired {
			return fmt.Errorf("render: class backing is Hugepages but the VMI already sets memLock: Required (conflicting user-specified backing)")
		}
		size := "2Mi"
		if b.HugepageSize != nil && !b.HugepageSize.IsZero() {
			size = b.HugepageSize.String()
		}
		if mem.Hugepages != nil && mem.Hugepages.PageSize != size {
			return fmt.Errorf("render: class backing is hugepages %s but the VMI already sets %s (conflicting user-specified backing)",
				size, mem.Hugepages.PageSize)
		}
		if mem.Hugepages == nil {
			mem.Hugepages = &kubevirtv1.Hugepages{PageSize: size}
		}
	case memoryv1alpha1.MemLockBacking:
		if mem.Hugepages != nil {
			return fmt.Errorf("render: class backing is MemLock but the VMI already sets hugepages %s (conflicting user-specified backing)",
				mem.Hugepages.PageSize)
		}
		if ml := memLockOf(mem); ml != nil && *ml == kubevirtv1.MemLockNotRequired {
			return fmt.Errorf("render: class backing is MemLock but the VMI explicitly sets memLock: NotRequired (conflicting user-specified backing)")
		}
		setMemLock(mem, kubevirtv1.MemLockRequired)
	default:
		return fmt.Errorf("render: unknown backing type %q", b.Type)
	}
	return nil
}

// renderGuaranteedShape ensures requests == limits for cpu and memory.
// Absent values are derived (memory from guest; cpu from the default);
// present values are respected when they already satisfy the shape and
// rejected when they conflict with it.
func renderGuaranteedShape(vmi *kubevirtv1.VirtualMachineInstance) error {
	domain := &vmi.Spec.Domain
	if domain.Resources.Requests == nil {
		domain.Resources.Requests = k8sv1.ResourceList{}
	}
	if domain.Resources.Limits == nil {
		domain.Resources.Limits = k8sv1.ResourceList{}
	}
	req, lim := domain.Resources.Requests, domain.Resources.Limits

	// memory: derive from guest when absent
	if _, ok := req[k8sv1.ResourceMemory]; !ok {
		if domain.Memory == nil || domain.Memory.Guest == nil {
			return fmt.Errorf("render: DRAM-only class needs a memory size (spec.domain.memory.guest) to render the Guaranteed shape")
		}
		req[k8sv1.ResourceMemory] = *domain.Memory.Guest
	}
	if _, ok := lim[k8sv1.ResourceMemory]; !ok {
		lim[k8sv1.ResourceMemory] = req[k8sv1.ResourceMemory]
	}
	if req[k8sv1.ResourceMemory] != lim[k8sv1.ResourceMemory] {
		return fmt.Errorf("render: DRAM-only class requires requests == limits for memory; user-specified resources conflict")
	}

	// cpu: default when absent; mirror when one-sided; fractional is
	// fine — requests == limits is what makes the pod Guaranteed.
	if _, ok := req[k8sv1.ResourceCPU]; !ok {
		if l, ok := lim[k8sv1.ResourceCPU]; ok {
			req[k8sv1.ResourceCPU] = l
		} else {
			req[k8sv1.ResourceCPU] = resource.MustParse(defaultCPU)
		}
	}
	if _, ok := lim[k8sv1.ResourceCPU]; !ok {
		lim[k8sv1.ResourceCPU] = req[k8sv1.ResourceCPU]
	}
	if req[k8sv1.ResourceCPU] != lim[k8sv1.ResourceCPU] {
		return fmt.Errorf("render: DRAM-only class requires requests == limits for cpu; user-specified resources conflict")
	}
	return nil
}

// Package render applies a resolved memory policy class to a VMI.
//
// Pure logic, unit-tested offline; no admission handler invokes it.
// These existing-field mutations do not implement a complete tier contract:
//
//   - DRAMOnly sets matching VMI CPU/memory requests and limits plus
//     hugepage backing. Final launcher QoS/overhead must be checked
//     separately. Static is a NUMA planner, not a DRAM selector: omitted
//     reservedMemory nodes are not excluded.
//   - Tiered leaves the spec unchanged and annotates the decision.
//     This is intent metadata, not measured residency or a hot-set promise.
//
// Actual tier placement, persistence, shared capacity and lifecycle behavior
// remain unimplemented/unvalidated obligations; see docs/memory-policy-classes.md.
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

// Annotation keys recording the rendering decision, not actual residency.
const (
	RenderedPolicyAnnotation = "memorytiering.shanemcd.com/rendered-policy"
	RenderedSourceAnnotation = "memorytiering.shanemcd.com/rendered-source"
)

// defaultCPU is the VMI request/limit when absent. It is a real CPU resource
// request, not an exclusive CPU assignment. Final pod QoS is a separate check.
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

func renderDRAMOnly(vmi *kubevirtv1.VirtualMachineInstance, r *policy.Resolved) error {
	if err := renderBacking(vmi, r.DefaultedBacking()); err != nil {
		return err
	}
	return renderGuaranteedShape(vmi)
}

// renderBacking sets the candidate backing field.
// An existing user-specified conflicting setting is an error.
func renderBacking(vmi *kubevirtv1.VirtualMachineInstance, b memoryv1alpha1.DRAMBacking) error {
	mem := vmi.Spec.Domain.Memory
	if mem == nil {
		mem = &kubevirtv1.Memory{}
		vmi.Spec.Domain.Memory = mem
	}
	switch b.Type {
	case memoryv1alpha1.HugepagesBacking:
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
	// fine for this VMI shape; final launcher QoS still requires validation.
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

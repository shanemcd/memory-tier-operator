package render

import (
	"strings"
	"testing"

	memoryv1alpha1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	"github.com/shanemcd/memory-tier-operator/internal/policy"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

func resolved(name string, mode memoryv1alpha1.MemoryPolicyMode, backing *memoryv1alpha1.DRAMBacking, source policy.SelectionSource) *policy.Resolved {
	return &policy.Resolved{
		Policy: &memoryv1alpha1.MemoryPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: memoryv1alpha1.MemoryPolicySpec{Mode: mode, Backing: backing},
		},
		Source: source,
	}
}

func baseVMI() *kubevirtv1.VirtualMachineInstance {
	guest := resource.MustParse("1Gi")
	return &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vmi", Namespace: "dev"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Domain: kubevirtv1.DomainSpec{
				Memory: &kubevirtv1.Memory{Guest: &guest},
			},
		},
	}
}

func TestDRAMOnlyHugepagesRendering(t *testing.T) {
	vmi := baseVMI()
	r := resolved("gold-hp", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.HugepagesBacking},
		policy.SourceSelection)
	if err := RenderVMI(vmi, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmi.Spec.Domain.Memory.Hugepages == nil || vmi.Spec.Domain.Memory.Hugepages.PageSize != "2Mi" {
		t.Fatalf("hugepages not rendered: %+v", vmi.Spec.Domain.Memory)
	}
	if vmi.Spec.Domain.Memory.ReservedOverhead != nil {
		t.Fatalf("memLock should not be set for hugepages backing")
	}
	req := vmi.Spec.Domain.Resources.Requests
	lim := vmi.Spec.Domain.Resources.Limits
	if req.Memory().String() != "1Gi" || lim.Memory().String() != "1Gi" {
		t.Fatalf("memory requests/limits not rendered from guest: %v/%v",
			req.Memory(), lim.Memory())
	}
	if req.Cpu().String() != "100m" || lim.Cpu().String() != "100m" {
		t.Fatalf("cpu requests/limits not defaulted: %v/%v",
			req.Cpu(), lim.Cpu())
	}
	if vmi.Annotations[RenderedPolicyAnnotation] != "gold-hp" ||
		vmi.Annotations[RenderedSourceAnnotation] != "selection" {
		t.Fatalf("rendering annotations not recorded: %v", vmi.Annotations)
	}
}

func TestDRAMOnlyMemLockRendering(t *testing.T) {
	vmi := baseVMI()
	r := resolved("gold-lock", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.MemLockBacking},
		policy.SourceClusterDefault)
	if err := RenderVMI(vmi, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ml := vmi.Spec.Domain.Memory.ReservedOverhead.MemLock
	if ml == nil || *ml != kubevirtv1.MemLockRequired {
		t.Fatalf("memLock Required not rendered: %+v", vmi.Spec.Domain.Memory.ReservedOverhead)
	}
	if vmi.Spec.Domain.Memory.Hugepages != nil {
		t.Fatalf("hugepages should not be set for memLock backing")
	}
}

func TestTieredIsExplicitNoOp(t *testing.T) {
	vmi := baseVMI()
	r := resolved("tiered", memoryv1alpha1.Tiered, nil, policy.SourceClusterDefault)
	if err := RenderVMI(vmi, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmi.Spec.Domain.Memory.Hugepages != nil ||
		vmi.Spec.Domain.Memory.ReservedOverhead != nil {
		t.Fatalf("tiered must not render a backing: %+v", vmi.Spec.Domain.Memory)
	}
	if vmi.Spec.Domain.Resources.Requests != nil && len(vmi.Spec.Domain.Resources.Requests) > 0 {
		t.Fatalf("tiered must not render resources: %+v", vmi.Spec.Domain.Resources)
	}
	if vmi.Annotations[RenderedPolicyAnnotation] != "tiered" {
		t.Fatalf("tiered decision not recorded: %v", vmi.Annotations)
	}
}

func TestTieredLeavesUserBackingAlone(t *testing.T) {
	vmi := baseVMI()
	vmi.Spec.Domain.Memory.Hugepages = &kubevirtv1.Hugepages{PageSize: "2Mi"}
	r := resolved("tiered", memoryv1alpha1.Tiered, nil, policy.SourceSelection)
	if err := RenderVMI(vmi, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmi.Spec.Domain.Memory.Hugepages == nil {
		t.Fatalf("user-owned backing on a tiered VM must be left alone")
	}
}

func TestConflictingUserBackingRejected(t *testing.T) {
	// user set memLock; class wants hugepages
	vmi := baseVMI()
	vmi.Spec.Domain.Memory.ReservedOverhead = &kubevirtv1.ReservedOverhead{
		MemLock: &[]kubevirtv1.MemLockRequirement{kubevirtv1.MemLockRequired}[0]}
	r := resolved("gold-hp", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.HugepagesBacking},
		policy.SourceSelection)
	err := RenderVMI(vmi, r)
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("want conflicting-backing error, got %v", err)
	}
	// user set hugepages; class wants memLock
	vmi2 := baseVMI()
	vmi2.Spec.Domain.Memory.Hugepages = &kubevirtv1.Hugepages{PageSize: "2Mi"}
	r2 := resolved("gold-lock", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.MemLockBacking},
		policy.SourceSelection)
	err = RenderVMI(vmi2, r2)
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("want conflicting-backing error, got %v", err)
	}
}

func TestConflictingUserResourcesRejected(t *testing.T) {
	vmi := baseVMI()
	vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{
		k8sv1.ResourceMemory: resource.MustParse("512Mi")}
	vmi.Spec.Domain.Resources.Limits = k8sv1.ResourceList{
		k8sv1.ResourceMemory: resource.MustParse("1Gi")}
	r := resolved("gold-hp", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.HugepagesBacking},
		policy.SourceSelection)
	err := RenderVMI(vmi, r)
	if err == nil || !strings.Contains(err.Error(), "requests == limits") {
		t.Fatalf("want guaranteed-shape conflict error, got %v", err)
	}
}

func TestGuaranteedUserResourcesRespected(t *testing.T) {
	vmi := baseVMI()
	vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{
		k8sv1.ResourceMemory: resource.MustParse("1Gi"),
		k8sv1.ResourceCPU:    resource.MustParse("250m")}
	vmi.Spec.Domain.Resources.Limits = k8sv1.ResourceList{
		k8sv1.ResourceMemory: resource.MustParse("1Gi"),
		k8sv1.ResourceCPU:    resource.MustParse("250m")}
	r := resolved("gold-hp", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.HugepagesBacking},
		policy.SourceSelection)
	if err := RenderVMI(vmi, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmi.Spec.Domain.Resources.Requests.Cpu().String() != "250m" {
		t.Fatalf("user cpu resources should be respected, got %v",
			vmi.Spec.Domain.Resources.Requests.Cpu())
	}
}

func TestMissingGuestMemoryRejected(t *testing.T) {
	vmi := &kubevirtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
		Spec:       kubevirtv1.VirtualMachineInstanceSpec{Domain: kubevirtv1.DomainSpec{}},
	}
	r := resolved("gold-hp", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.HugepagesBacking},
		policy.SourceSelection)
	err := RenderVMI(vmi, r)
	if err == nil || !strings.Contains(err.Error(), "memory size") {
		t.Fatalf("want missing-memory error, got %v", err)
	}
}

func TestExplicitBackingSameValuesNotAConflict(t *testing.T) {
	vmi := baseVMI()
	vmi.Spec.Domain.Memory.Hugepages = &kubevirtv1.Hugepages{PageSize: "2Mi"}
	vmi.Spec.Domain.Resources.Requests = k8sv1.ResourceList{
		k8sv1.ResourceMemory: resource.MustParse("1Gi"),
		k8sv1.ResourceCPU:    resource.MustParse("100m")}
	vmi.Spec.Domain.Resources.Limits = k8sv1.ResourceList{
		k8sv1.ResourceMemory: resource.MustParse("1Gi"),
		k8sv1.ResourceCPU:    resource.MustParse("100m")}
	r := resolved("gold-hp", memoryv1alpha1.DRAMOnly,
		&memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.HugepagesBacking},
		policy.SourceForced)
	if err := RenderVMI(vmi, r); err != nil {
		t.Fatalf("same-value backing is not a conflict: %v", err)
	}
	if vmi.Annotations[RenderedSourceAnnotation] != "forced" {
		t.Fatalf("forced source not recorded: %v", vmi.Annotations)
	}
}

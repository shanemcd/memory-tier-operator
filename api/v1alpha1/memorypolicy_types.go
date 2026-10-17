// Package v1alpha1 — MemoryPolicy types.
//
// MemoryPolicy is the cluster-scoped, administrator-defined memory
// policy class: the workload-facing QoS surface for host-transparent
// memory tiering. A workload selects a class by name (annotation on
// the VMI); the class carries the mechanism and renders into existing
// Kubernetes and KubeVirt fields at admission (Guaranteed resource
// shape, Memory Manager Static placement, and a persistence backing —
// hugepages or memory locking). The Tiered mode is the explicit
// default behavior: kernel demotion with no additional mechanism.
//
// This is the direction replacing the DRA-claims path on main: the
// class is infrastructure-owned (the administrator defines what is
// available and who may use it), and the workload expresses need, not
// mechanism.
package v1alpha1

import (
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MemoryPolicyMode is the behavior class of a policy.
// +kubebuilder:validation:Enum=DRAMOnly;Tiered
type MemoryPolicyMode string

const (
	// DRAMOnly places the workload's guest memory on the fast tier at
	// admission and keeps it there under node pressure (placement via
	// Memory Manager Static + a persistence backing).
	DRAMOnly MemoryPolicyMode = "DRAMOnly"
	// Tiered allows guest memory to span tiers with the kernel moving
	// cold pages to the slow tier — the default system behavior on a
	// tiered node; no mechanism is rendered.
	Tiered MemoryPolicyMode = "Tiered"
)

// BackingType is the persistence wall used by DRAM-only classes.
// +kubebuilder:validation:Enum=Hugepages;MemLock
type BackingType string

const (
	// HugepagesBacking renders spec.domain.memory.hugepages; the pool
	// provides structural placement and scheduler-visible accounting.
	HugepagesBacking BackingType = "Hugepages"
	// MemLockBacking renders spec.domain.memory.memLock: Required
	// (KubeVirt 1.10 field); no node partition beyond Static.
	MemLockBacking BackingType = "MemLock"
)

// DRAMBacking configures the persistence wall of a DRAM-only class.
// The choice between Hugepages and MemLock is an administrator
// tradeoff (scheduler-visible accounting vs no boot-time pool) — the
// prototype implements both; the product decision is open.
type DRAMBacking struct {
	// Type selects the persistence wall.
	// +kubebuilder:validation:Enum=Hugepages;MemLock
	// +kubebuilder:default=Hugepages
	Type BackingType `json:"type"`

	// HugepageSize is the hugepage size for Hugepages backing
	// (default 2Mi). Ignored for MemLock.
	// +optional
	HugepageSize *resource.Quantity `json:"hugepageSize,omitempty"`
}

// NamespaceRule constrains class selection for one namespace ("*"
// matches all). Forced wins over the workload's selection; an explicit
// selection that is forbidden (or not in a non-empty allowed list) is
// rejected at admission — never silently downgraded.
type NamespaceRule struct {
	// Namespace this rule applies to; "*" matches every namespace.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Allowed class names. Empty means all defined classes are
	// permitted (subject to Forbidden).
	// +optional
	Allowed []string `json:"allowed,omitempty"`

	// Forbidden class names — rejected even if listed in Allowed.
	// +optional
	Forbidden []string `json:"forbidden,omitempty"`

	// Forced class — used regardless of the workload's selection.
	// +optional
	Forced *string `json:"forced,omitempty"`

	// Default class for workloads in this namespace that select
	// nothing. Overrides the cluster default.
	// +optional
	Default *string `json:"default,omitempty"`
}

// SelectionRules is the administrator's AAA surface: which classes
// each namespace may use, which are forbidden, which is forced.
type SelectionRules struct {
	// NamespaceRules, most specific match wins (exact namespace over
	// "*"). An absent rule permits all classes.
	// +optional
	NamespaceRules []NamespaceRule `json:"namespaceRules,omitempty"`
}

// MemoryPolicySpec defines the desired class.
type MemoryPolicySpec struct {
	// Mode selects the class behavior.
	Mode MemoryPolicyMode `json:"mode"`

	// Backing is the persistence wall for DRAM-only classes. Required
	// semantically for DRAMOnly (defaulted to Hugepages 2Mi by the
	// resolver when omitted); ignored for Tiered.
	// +optional
	Backing *DRAMBacking `json:"backing,omitempty"`

	// Default marks this class as the cluster default for workloads
	// that select nothing. At most one class may set this.
	// +optional
	Default bool `json:"default,omitempty"`

	// Selection constrains which workloads may use which classes.
	// Rules on this class apply cluster-wide (they are read by the
	// resolver regardless of which class carries them; the
	// conventional home is the default class).
	// +optional
	Selection *SelectionRules `json:"selection,omitempty"`
}

// MemoryPolicyStatus is reserved; the rendering outcome is recorded on
// the workload (VMI status annotation), not on the class.
type MemoryPolicyStatus struct{}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mempol
// +kubebuilder:subresource:status

// MemoryPolicy is an administrator-defined memory policy class.
type MemoryPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MemoryPolicySpec   `json:"spec,omitempty"`
	Status MemoryPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryPolicyList contains a list of MemoryPolicy.
type MemoryPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MemoryPolicy{}, &MemoryPolicyList{})
}

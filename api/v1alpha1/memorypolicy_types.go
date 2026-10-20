// Package v1alpha1 — MemoryPolicy types.
//
// MemoryPolicy is the cluster-scoped, administrator-defined memory
// policy class: the workload-facing QoS surface for host-transparent
// memory tiering. A workload selects a class by name (annotation on
// the VMI). These are proposed types plus unwired resolution/rendering
// functions, not a deployed admission service. Existing fields alone do not
// establish tier placement, persistence or shared physical accounting.
// Tiered permits movement without promising a split or hot-page retention.
//
// Infrastructure owns the proposed implementation; the executable still
// registers the DRA reconciler. Internal DRA and upstream alternatives remain
// open; see docs/memory-policy-classes.md.
package v1alpha1

import (
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MemoryPolicyMode is the behavior class of a policy.
// +kubebuilder:validation:Enum=DRAMOnly;Tiered
type MemoryPolicyMode string

const (
	// DRAMOnly expresses the intended protected scope. The library does not
	// implement a complete placement/persistence/capacity contract.
	DRAMOnly MemoryPolicyMode = "DRAMOnly"
	// Tiered allows guest memory to span tiers with the kernel moving
	// cold pages to the slow tier — the default system behavior on a
	// tiered node; no mechanism is rendered.
	Tiered MemoryPolicyMode = "Tiered"
)

// BackingType selects the candidate rendering.
// +kubebuilder:validation:Enum=Hugepages
type BackingType string

const (
	// HugepagesBacking renders spec.domain.memory.hugepages. DRAM pool
	// locality, actual guest use and shared accounting need validation.
	HugepagesBacking BackingType = "Hugepages"
)

// DRAMBacking configures the backing rendered for DRAMOnly classes.
// Product backing/integration decisions and runtime validation remain open.
type DRAMBacking struct {
	// Type selects the candidate rendering.
	// +kubebuilder:validation:Enum=Hugepages
	// +kubebuilder:default=Hugepages
	Type BackingType `json:"type"`

	// HugepageSize is the hugepage size for Hugepages backing
	// (default 2Mi).
	// +optional
	HugepageSize *resource.Quantity `json:"hugepageSize,omitempty"`
}

// NamespaceRule constrains class selection for one namespace ("*"
// matches all). Forced wins over the workload's selection; an explicit
// selection that is forbidden (or not in a non-empty allowed list) is
// rejected by the resolver. A future admission adapter must enforce the result.
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

	// Backing is the candidate field rendering for DRAM-only classes. Required
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

// MemoryPolicyStatus is reserved. The library records the rendering decision
// in VMI metadata annotations; no residency status integration exists.
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

// Package v1alpha1 contains the API types for the memory-tier operator.
//
// MemoryTiering is the cluster-scoped custom resource that configures
// host-transparent memory tiering: publishing per-node memory tiers
// (DRAM, CXL) as claimable DRA resources and enforcing claimed tiers
// via cpuset memory policies on the claiming pod's cgroup.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MemoryTieringSpec configures the memory tiering deployment.
type MemoryTieringSpec struct {
	// DriverImage overrides the image used for the per-node driver
	// daemon. When unset, the operator deploys its own image with the
	// `driver` entrypoint.
	// +optional
	DriverImage string `json:"driverImage,omitempty"`

	// NodeSelector selects which nodes run the driver daemon. Only
	// matching nodes with CXL-attached memory will publish tiers.
	// Default: none (the operator relies on the driver's own topology
	// detection to publish tiers only where they exist).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// MemoryTieringStatus reports the observed state.
type MemoryTieringStatus struct {
	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status

// MemoryTiering configures host-transparent memory tiering for the cluster.
type MemoryTiering struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MemoryTieringSpec   `json:"spec,omitempty"`
	Status MemoryTieringStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryTieringList contains a list of MemoryTiering.
type MemoryTieringList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryTiering `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MemoryTiering{}, &MemoryTieringList{})
}

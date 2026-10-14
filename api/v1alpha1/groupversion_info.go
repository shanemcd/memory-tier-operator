// Package v1alpha1 contains API Schema versions for the
// memorytier.shanemcd.com API group.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "memorytier.shanemcd.com", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the GroupVersionKind scheme to the runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

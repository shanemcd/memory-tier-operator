// Package webhook provides a VMI mutating admission webhook that
// translates a memory-tier annotation into a DRA ResourceClaimTemplate
// reference, so users don't need to write claims by hand.
//
// The annotation: memorytier.shanemcd.com/tier: DRAM-only
// becomes: spec.resourceClaims[] referencing a claim template named
// <vmi-name>-memory-tier.
package webhook

import (
	"context"
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	virtv1 "kubevirt.io/api/core/v1"
)

// ClaimTemplater creates and maintains the ResourceClaimTemplates that
// the webhook's injected references point to. It runs as part of the
// operator's reconciliation loop.
type ClaimTemplater struct {
	Client client.Client
	Scheme *runtime.Scheme
}

// EnsureVMIClaimTemplate creates or updates the claim template for a
// VMI that has a tier annotation. Called from the operator's reconcile
// when it sees a VMI with the annotation.
func (t *ClaimTemplater) EnsureVMIClaimTemplate(ctx context.Context, vmi *virtv1.VirtualMachineInstance) error {
	logger := log.FromContext(ctx)

	tier := vmi.Annotations[TierAnnotation]
	if tier == "" {
		return nil
	}
	class, ok := tierClasses[tier]
	if !ok {
		return fmt.Errorf("unknown tier %q", tier)
	}

	// Determine guest RAM
	var mem resource.Quantity
	if vmi.Spec.Domain.Memory != nil && vmi.Spec.Domain.Memory.Guest != nil {
		mem = *vmi.Spec.Domain.Memory.Guest
	} else if req, ok := vmi.Spec.Domain.Resources.Requests[corev1_ResourceMemory]; ok {
		mem = req
	} else {
		return fmt.Errorf("tier annotation requires guest memory")
	}

	tmplName := fmt.Sprintf("%s-memory-tier", vmi.Name)
	desired := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tmplName,
			Namespace: vmi.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(vmi, virtv1.VirtualMachineInstanceGroupVersionKind),
			},
		},
		Spec: resourcev1.ResourceClaimTemplateSpec{
			Spec: resourcev1.ResourceClaimSpec{
				Devices: resourcev1.DeviceClaim{
					Requests: []resourcev1.DeviceRequest{{
						Name: "memory",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: class,
							AllocationMode:  resourcev1.DeviceAllocationModeAll,
							Capacity: &resourcev1.CapacityRequirements{
								Requests: map[resourcev1.QualifiedName]resource.Quantity{
									AttrDomain + "/memory": mem,
								},
							},
						},
					}},
				},
			},
		},
	}

	existing := &resourcev1.ResourceClaimTemplate{}
	err := t.Client.Get(ctx, types.NamespacedName{Name: tmplName, Namespace: vmi.Namespace}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get claim template: %w", err)
		}
		if err := t.Client.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create claim template: %w", err)
		}
		logger.Info("created claim template", "name", tmplName, "tier", tier, "capacity", mem.String())
		return nil
	}
	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		existing.Spec = desired.Spec
		if err := t.Client.Update(ctx, existing); err != nil {
			return fmt.Errorf("update claim template: %w", err)
		}
		logger.Info("updated claim template", "name", tmplName, "tier", tier)
	}
	return nil
}

// corev1_ResourceMemory avoids importing corev1 just for one constant.
const corev1_ResourceMemory = "memory"

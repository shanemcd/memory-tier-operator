// Package webhook wires the memory policy classes into VMI admission.
//
// This is the missing wiring described in docs/memory-policy-classes.md:
// the resolver and renderer existed as pure functions, but nothing called
// them at admission time. This handler is that caller. It is the only
// component that turns a MemoryPolicy from vocabulary into behavior.
//
// Semantics (fail closed, never a silent downgrade):
//
//   - The webhook intercepts VMI CREATE in namespaces labeled
//     memorytiering.shanemcd.com/governed="true" (namespaceSelector on
//     the MutatingWebhookConfiguration). A governed namespace opts every
//     VMI in it into class resolution.
//   - Resolution errors (unknown class, forbidden selection, no default)
//     DENY the create with the resolver's message.
//   - Rendering errors (conflicting user-specified backing or resources)
//     DENY the create. The class owns the mechanism; conflicts are
//     surfaced, not overwritten.
//   - MemLock backing is refused unless --allow-memlock-backing is set:
//     spec.domain.memory.reservedOverhead.memLock exists only in
//     KubeVirt v1.10+ behind an alpha gate. On an older cluster the
//     apiserver prunes the field silently, which would turn the class
//     into an unnoticed no-op. Refusing is the honest behavior.
//
// What this handler does NOT do: it does not verify placement, residency
// or capacity. A rendered VMI is intent carried by stock fields, and the
// observability stack (the memory-tier-exporter) measures what actually
// happened. See docs/memory-policy-classes.md for the obligation map.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	memoryv1alpha1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	"github.com/shanemcd/memory-tier-operator/internal/policy"
	"github.com/shanemcd/memory-tier-operator/internal/render"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

// VMIMutation resolves a VMI's memory policy class and renders it into
// the VMI spec at admission time.
type VMIMutation struct {
	Client  client.Client
	Decoder admission.Decoder

	// AllowMemLockBacking permits the MemLock backing type. Requires a
	// cluster actually running KubeVirt v1.10+ with the
	// ReservedOverheadMemlock alpha feature enabled.
	AllowMemLockBacking bool
}

// Handle implements admission.Handler.
func (h *VMIMutation) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Registered for CREATE only; pass through anything else untouched.
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	vmi := &kubevirtv1.VirtualMachineInstance{}
	if err := h.Decoder.Decode(req, vmi); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	namespace := req.Namespace
	if namespace == "" {
		namespace = vmi.Namespace
	}

	policies := &memoryv1alpha1.MemoryPolicyList{}
	if err := h.Client.List(ctx, policies); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	resolved, err := policy.Resolve(policies.Items, namespace, vmi.Annotations[policy.SelectionAnnotation])
	if err != nil {
		// Rejection at admission: unknown class, forbidden selection,
		// missing default. Never a silent downgrade.
		return admission.Denied(err.Error())
	}

	if resolved.Policy.Spec.Mode == memoryv1alpha1.DRAMOnly &&
		resolved.DefaultedBacking().Type == memoryv1alpha1.MemLockBacking &&
		!h.AllowMemLockBacking {
		return admission.Denied(fmt.Sprintf(
			"memory policy class %q requests MemLock backing, which needs KubeVirt v1.10+ with the ReservedOverheadMemlock alpha feature; on this cluster the apiserver would prune the field silently, so admission is refused (fail closed). Start the operator with --allow-memlock-backing on a cluster that supports the field.",
			resolved.Policy.Name))
	}

	rendered := vmi.DeepCopy()
	if err := render.RenderVMI(rendered, resolved); err != nil {
		// Conflicting user-specified mechanism or resource shape.
		return admission.Denied(err.Error())
	}

	raw, err := json.Marshal(rendered)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

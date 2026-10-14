// Package webhook provides a VMI mutating admission webhook that
// translates a memory-tier annotation into a DRA ResourceClaimTemplate
// reference, so users don't need to write claims by hand.
//
// The annotation: memorytier.shanemcd.com/tier: DRAM-only
// becomes: spec.resourceClaims[] referencing a claim template named
// <vmi-name>-memory-tier, created (or expected) in the VMI's namespace.
//
// This replaces the KubeVirt fork approach: instead of adding a
// domain.memory.tier field to the KubeVirt API (which requires a
// fork), the webhook injects the DRA claim into the existing
// spec.resourceClaims field at admission time.
package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	virtv1 "kubevirt.io/api/core/v1"
)

const (
	// TierAnnotation is the annotation that triggers claim injection.
	TierAnnotation = "memorytier.shanemcd.com/tier"

	// ClaimRefName is the name used for the injected resourceClaim reference.
	ClaimRefName = "memory-tier"

	// AttrDomain is the driver's attribute domain.
	AttrDomain = "memorytier.openshift.io"

	// DriverName is the DRA driver identity.
	DriverName = "memorytier.openshift.io"
)

// tierClasses maps the annotation value to a DeviceClass name.
var tierClasses = map[string]string{
	"DRAM-only": "dram-tier",
	"CXL-only":  "cxl-tier",
}

var scheme = runtime.NewScheme()

func init() {
	_ = virtv1.AddToScheme(scheme)
	_ = resourcev1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
}

// HandleMutate processes a VMI admission request.
func HandleMutate(w http.ResponseWriter, r *http.Request) {
	// Read the admission review
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	response := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     review.Request.UID,
			Allowed: true,
		},
	}
	defer func() {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}()

	if review.Request.Operation != admissionv1.Create {
		return // only mutate on create (VMI specs are immutable)
	}

	// Decode the VMI
	codecs := serializer.NewCodecFactory(scheme)
	vmi := &virtv1.VirtualMachineInstance{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(review.Request.Object.Raw, nil, vmi); err != nil {
		response.Response.Result = &metav1.Status{
			Message: fmt.Sprintf("decode VMI: %v", err),
		}
		response.Response.Allowed = false
		return
	}

	tier := vmi.Annotations[TierAnnotation]
	if tier == "" {
		return // no annotation, no mutation
	}

	if _, ok := tierClasses[tier]; !ok {
		response.Response.Result = &metav1.Status{
			Message: fmt.Sprintf("unknown tier %q (supported: DRAM-only, CXL-only)", tier),
		}
		response.Response.Allowed = false
		return
	}

	// Build the claim template name (deterministic per VMI)
	tmplName := fmt.Sprintf("%s-memory-tier", vmi.Name)

	// Check if the VMI already has a memory-tier claim reference
	for _, rc := range vmi.Spec.ResourceClaims {
		if rc.Name == ClaimRefName {
			return // already has one, don't duplicate
		}
	}

	// Inject the resourceClaim reference
	vmi.Spec.ResourceClaims = append(vmi.Spec.ResourceClaims,
		virtv1.VirtualMachineInstanceResourceClaim{
			Name:                      ClaimRefName,
			ResourceClaimTemplateName: &tmplName,
		})

	// Build the JSON patch (replace the spec)
	response.Response.PatchType = func() *admissionv1.PatchType {
		pt := admissionv1.PatchTypeJSONPatch
		return &pt
	}()

	// Create patches for: the resourceClaims addition
	patches := []map[string]interface{}{
		{
			"op":    "add",
			"path":  "/spec/resourceClaims",
			"value": vmi.Spec.ResourceClaims,
		},
	}
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		response.Response.Result = &metav1.Status{
			Message: fmt.Sprintf("marshal patch: %v", err),
		}
		response.Response.Allowed = false
		return
	}
	response.Response.Patch = patchBytes
}

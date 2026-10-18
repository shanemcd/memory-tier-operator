package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	memoryv1alpha1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	"github.com/shanemcd/memory-tier-operator/internal/policy"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		memoryv1alpha1.AddToScheme,
		kubevirtv1.AddToScheme,
		clientgoscheme.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func goldHPPolicy() *memoryv1alpha1.MemoryPolicy {
	return &memoryv1alpha1.MemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gold-hp"},
		Spec: memoryv1alpha1.MemoryPolicySpec{
			Mode:    memoryv1alpha1.DRAMOnly,
			Default: true,
			Backing: &memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.HugepagesBacking, HugepageSize: quantityPtr("2Mi")},
		},
	}
}

func goldLockPolicy() *memoryv1alpha1.MemoryPolicy {
	return &memoryv1alpha1.MemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "gold-lock"},
		Spec: memoryv1alpha1.MemoryPolicySpec{
			Mode:    memoryv1alpha1.DRAMOnly,
			Backing: &memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.MemLockBacking},
		},
	}
}

func tieredPolicy() *memoryv1alpha1.MemoryPolicy {
	return &memoryv1alpha1.MemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "tiered"},
		Spec:       memoryv1alpha1.MemoryPolicySpec{Mode: memoryv1alpha1.Tiered},
	}
}

// vmiRequest builds a CREATE AdmissionRequest for a 1Gi VMI in namespace
// "demo" with the given selection annotation value ("-" = no annotation).
func vmiRequest(t *testing.T, s *runtime.Scheme, annotation string) admission.Request {
	t.Helper()
	vmi := &kubevirtv1.VirtualMachineInstance{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance"},
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "demo"},
		Spec: kubevirtv1.VirtualMachineInstanceSpec{
			Domain: kubevirtv1.DomainSpec{
				Memory: &kubevirtv1.Memory{Guest: quantityPtr("1Gi")},
			},
		},
	}
	if annotation != "-" {
		vmi.Annotations = map[string]string{policy.SelectionAnnotation: annotation}
	}
	raw, err := json.Marshal(vmi)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: "demo",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func newHandler(t *testing.T, policies []memoryv1alpha1.MemoryPolicy, allowMemLock bool) *VMIMutation {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithLists(&memoryv1alpha1.MemoryPolicyList{Items: policies}).Build()
	return &VMIMutation{Client: c, Decoder: admission.NewDecoder(s), AllowMemLockBacking: allowMemLock}
}

func patchPaths(t *testing.T, resp admission.Response) []string {
	t.Helper()
	paths := []string{}
	for _, p := range resp.Patches {
		paths = append(paths, p.Path)
	}
	return paths
}

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func hasPathContaining(paths []string, substr string) bool {
	for _, p := range paths {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// dram-only via hugepages: allowed, patched with backing + Guaranteed
// shape + decision annotations.
func TestDRAMOnlyHugepagesPatched(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldHPPolicy(), *tieredPolicy()}, false)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "gold-hp"))
	if !resp.Allowed {
		t.Fatalf("expected allowed, got deny: %s", resp.Result.Message)
	}
	paths := patchPaths(t, resp)
	for _, want := range []string{
		"/spec/domain/memory/hugepages",
		"/spec/domain/resources/requests",
		"/spec/domain/resources/limits",
	} {
		if !hasPath(paths, want) {
			// requests/limits may also be patched as nested keys when the
			// maps already exist.
			nested := strings.TrimSuffix(want, "/memory") + "/memory"
			if nested != want && !hasPath(paths, nested) {
				t.Errorf("expected patch path %s (or %s), got %v", want, nested, paths)
			}
		}
	}
	if !hasPathContaining(paths, "rendered-policy") {
		t.Errorf("expected a rendered-policy annotation patch, got %v", paths)
	}
}

// tiered: allowed, annotations only — the spec is untouched.
func TestTieredOnlyAnnotates(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldHPPolicy(), *tieredPolicy()}, false)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "tiered"))
	if !resp.Allowed {
		t.Fatalf("expected allowed, got deny: %s", resp.Result.Message)
	}
	for _, p := range resp.Patches {
		if strings.HasPrefix(p.Path, "/spec") {
			t.Errorf("tiered must not touch the spec, got patch %s", p.Path)
		}
	}
}

// unknown class: denied, fail closed.
func TestUnknownClassDenied(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*tieredPolicy()}, false)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "nonexistent"))
	if resp.Allowed {
		t.Fatal("expected deny for unknown class")
	}
	if !strings.Contains(resp.Result.Message, "unknown memory policy class") {
		t.Errorf("unexpected message: %s", resp.Result.Message)
	}
}

// forbidden selection: denied.
func TestForbiddenClassDenied(t *testing.T) {
	s := testScheme(t)
	p := goldHPPolicy()
	p.Spec.Selection = &memoryv1alpha1.SelectionRules{
		NamespaceRules: []memoryv1alpha1.NamespaceRule{
			{Namespace: "demo", Forbidden: []string{"gold-hp"}},
		},
	}
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*p, *tieredPolicy()}, false)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "gold-hp"))
	if resp.Allowed {
		t.Fatal("expected deny for forbidden class")
	}
	if !strings.Contains(resp.Result.Message, "not permitted") {
		t.Errorf("unexpected message: %s", resp.Result.Message)
	}
}

// no selection, cluster default exists: resolved to the default.
func TestNoSelectionUsesClusterDefault(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldHPPolicy()}, false)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "-"))
	if !resp.Allowed {
		t.Fatalf("expected allowed via cluster default, got: %s", resp.Result.Message)
	}
	if !hasPath(patchPaths(t, resp), "/spec/domain/memory/hugepages") {
		t.Error("expected default class to render hugepages")
	}
}

// no selection, no default: denied (governed namespaces must resolve).
func TestNoDefaultDenied(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*tieredPolicy()}, false)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "-"))
	if resp.Allowed {
		t.Fatal("expected deny when nothing resolves")
	}
	if !strings.Contains(resp.Result.Message, "no memory policy class") {
		t.Errorf("unexpected message: %s", resp.Result.Message)
	}
}

// MemLock backing without the opt-in: denied rather than silently pruned.
func TestMemLockRefusedWithoutOptIn(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldLockPolicy()}, false)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "gold-lock"))
	if resp.Allowed {
		t.Fatal("expected deny for MemLock without opt-in")
	}
	if !strings.Contains(resp.Result.Message, "prune the field silently") {
		t.Errorf("unexpected message: %s", resp.Result.Message)
	}
}

// MemLock backing with the opt-in: allowed and rendered.
func TestMemLockAllowedWithOptIn(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldLockPolicy()}, true)
	resp := h.Handle(context.Background(), vmiRequest(t, s, "gold-lock"))
	if !resp.Allowed {
		t.Fatalf("expected allowed with opt-in, got: %s", resp.Result.Message)
	}
	if !hasPath(patchPaths(t, resp), "/spec/domain/memory/reservedOverhead") {
		t.Errorf("expected memLock rendering, got %v", patchPaths(t, resp))
	}
}

// conflicting user-specified backing: denied, not overwritten.
func TestConflictingUserBackingDenied(t *testing.T) {
	s := testScheme(t)
	req := vmiRequest(t, s, "gold-hp")
	vmi := &kubevirtv1.VirtualMachineInstance{}
	if err := admission.NewDecoder(s).Decode(req, vmi); err != nil {
		t.Fatal(err)
	}
	vmi.Spec.Domain.Memory.Hugepages = &kubevirtv1.Hugepages{PageSize: "1Gi"}
	raw, err := json.Marshal(vmi)
	if err != nil {
		t.Fatal(err)
	}
	req.Object.Raw = raw

	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldHPPolicy()}, false)
	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected deny for conflicting user-specified backing")
	}
	if !strings.Contains(resp.Result.Message, "conflicting user-specified backing") {
		t.Errorf("unexpected message: %s", resp.Result.Message)
	}
}

// non-create operations pass through untouched (defensive; the
// MutatingWebhookConfiguration registers CREATE only).
func TestNonCreatePassesThrough(t *testing.T) {
	s := testScheme(t)
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldHPPolicy()}, false)
	req := vmiRequest(t, s, "gold-hp")
	req.Operation = admissionv1.Update
	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatal("expected non-create to be allowed untouched")
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches, got %v", resp.Patches)
	}
}

func TestPolicyLabelsStamped(t *testing.T) {
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldHPPolicy()}, false)
	req := vmiRequest(t, testScheme(t), "gold-hp")
	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denial: %s", resp.Result.Message)
	}
	vmi := patchedVMI(t, req, resp)
	if got := vmi.Labels[policyClassLabel]; got != "gold-hp" {
		t.Errorf("policy-class label = %q, want gold-hp", got)
	}
	if got := vmi.Labels[policyModeLabel]; got != "DRAMOnly" {
		t.Errorf("policy-mode label = %q, want DRAMOnly", got)
	}
}

func TestPolicyModeLabelReflectsClassMode(t *testing.T) {
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*tieredPolicy()}, false)
	req := vmiRequest(t, testScheme(t), "tiered")
	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denial: %s", resp.Result.Message)
	}
	vmi := patchedVMI(t, req, resp)
	if got := vmi.Labels[policyModeLabel]; got != "Tiered" {
		t.Errorf("policy-mode label = %q, want Tiered", got)
	}
}

func TestExistingLabelsPreserved(t *testing.T) {
	h := newHandler(t, []memoryv1alpha1.MemoryPolicy{*goldHPPolicy()}, false)
	req := vmiRequest(t, testScheme(t), "gold-hp")
	vmi := &kubevirtv1.VirtualMachineInstance{}
	if err := json.Unmarshal(req.Object.Raw, vmi); err != nil {
		t.Fatal(err)
	}
	vmi.Labels = map[string]string{"app": "demo", policyClassLabel: "stale-old-value"}
	raw, err := json.Marshal(vmi)
	if err != nil {
		t.Fatal(err)
	}
	req.Object.Raw = raw

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denial: %s", resp.Result.Message)
	}
	final := patchedVMI(t, req, resp)
	if got := final.Labels["app"]; got != "demo" {
		t.Errorf("unrelated label lost: app = %q, want demo", got)
	}
	if got := final.Labels[policyClassLabel]; got != "gold-hp" {
		t.Errorf("policy-class label = %q, want gold-hp (stale value replaced)", got)
	}
	if got := final.Labels[policyModeLabel]; got != "DRAMOnly" {
		t.Errorf("policy-mode label = %q, want DRAMOnly", got)
	}
}

// patchedVMI applies the admission response's patch to the original
// request object and returns the resulting VMI, so tests can assert on
// the effect rather than the patch's wire shape (whole-map add vs
// per-key paths).
func patchedVMI(t *testing.T, req admission.Request, resp admission.Response) *kubevirtv1.VirtualMachineInstance {
	t.Helper()
	if len(resp.Patches) == 0 {
		vmi := &kubevirtv1.VirtualMachineInstance{}
		if err := json.Unmarshal(req.Object.Raw, vmi); err != nil {
			t.Fatal(err)
		}
		return vmi
	}
	raw, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := jsonpatch.DecodePatch(raw)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := patch.Apply(req.Object.Raw)
	if err != nil {
		t.Fatal(err)
	}
	vmi := &kubevirtv1.VirtualMachineInstance{}
	if err := json.Unmarshal(patched, vmi); err != nil {
		t.Fatal(err)
	}
	return vmi
}

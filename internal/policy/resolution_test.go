package policy

import (
	"errors"
	"testing"

	memoryv1alpha1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func class(name string, mode memoryv1alpha1.MemoryPolicyMode, def bool) memoryv1alpha1.MemoryPolicy {
	return memoryv1alpha1.MemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: memoryv1alpha1.MemoryPolicySpec{Mode: mode, Default: def},
	}
}

func withSelection(p memoryv1alpha1.MemoryPolicy, sel *memoryv1alpha1.SelectionRules) memoryv1alpha1.MemoryPolicy {
	p.Spec.Selection = sel
	return p
}

func strp(s string) *string { return &s }

func TestForcedWinsOverSelection(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		withSelection(class("gold", memoryv1alpha1.DRAMOnly, true),
			&memoryv1alpha1.SelectionRules{NamespaceRules: []memoryv1alpha1.NamespaceRule{{
				Namespace: "prod", Forced: strp("silver"),
			}}}),
		class("silver", memoryv1alpha1.Tiered, false),
	}
	r, err := Resolve(policies, "prod", "gold")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Policy.Name != "silver" || r.Source != SourceForced {
		t.Fatalf("forced class not applied: %+v", r)
	}
}

func TestSelectionRespectedWhenAllowed(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		class("gold", memoryv1alpha1.DRAMOnly, true),
		class("silver", memoryv1alpha1.Tiered, false),
	}
	r, err := Resolve(policies, "dev", "silver")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Policy.Name != "silver" || r.Source != SourceSelection {
		t.Fatalf("selection not respected: %+v", r)
	}
}

func TestForbiddenSelectionRejected(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		withSelection(class("gold", memoryv1alpha1.DRAMOnly, true),
			&memoryv1alpha1.SelectionRules{NamespaceRules: []memoryv1alpha1.NamespaceRule{{
				Namespace: "sandbox", Forbidden: []string{"gold"},
			}}}),
	}
	_, err := Resolve(policies, "sandbox", "gold")
	if !errors.Is(err, ErrForbiddenClass) {
		t.Fatalf("want ErrForbiddenClass, got %v", err)
	}
}

func TestNotAllowedListSelectionRejected(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		withSelection(class("gold", memoryv1alpha1.DRAMOnly, true),
			&memoryv1alpha1.SelectionRules{NamespaceRules: []memoryv1alpha1.NamespaceRule{{
				Namespace: "tenants", Allowed: []string{"bronze"},
			}}}),
		class("silver", memoryv1alpha1.Tiered, false),
	}
	_, err := Resolve(policies, "tenants", "silver")
	if !errors.Is(err, ErrForbiddenClass) {
		t.Fatalf("want ErrForbiddenClass for non-allowed selection, got %v", err)
	}
}

func TestUnknownClassRejected(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{class("gold", memoryv1alpha1.DRAMOnly, true)}
	_, err := Resolve(policies, "dev", "platinum")
	if !errors.Is(err, ErrUnknownClass) {
		t.Fatalf("want ErrUnknownClass, got %v", err)
	}
}

func TestNamespaceDefaultOverClusterDefault(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		withSelection(class("gold", memoryv1alpha1.DRAMOnly, true),
			&memoryv1alpha1.SelectionRules{NamespaceRules: []memoryv1alpha1.NamespaceRule{{
				Namespace: "dev", Default: strp("silver"),
			}}}),
		class("silver", memoryv1alpha1.Tiered, false),
	}
	r, err := Resolve(policies, "dev", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Policy.Name != "silver" || r.Source != SourceNamespaceDefault {
		t.Fatalf("namespace default not applied: %+v", r)
	}
	// another namespace falls back to the cluster default
	r, err = Resolve(policies, "prod", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Policy.Name != "gold" || r.Source != SourceClusterDefault {
		t.Fatalf("cluster default not applied: %+v", r)
	}
}

func TestNoDefaultIsError(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{class("gold", memoryv1alpha1.DRAMOnly, false)}
	_, err := Resolve(policies, "dev", "")
	if !errors.Is(err, ErrNoDefault) {
		t.Fatalf("want ErrNoDefault, got %v", err)
	}
}

func TestAmbiguousDefaultIsError(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		class("gold", memoryv1alpha1.DRAMOnly, true),
		class("silver", memoryv1alpha1.Tiered, true),
	}
	_, err := Resolve(policies, "dev", "")
	if !errors.Is(err, ErrAmbiguousDefault) {
		t.Fatalf("want ErrAmbiguousDefault, got %v", err)
	}
}

func TestExactRuleBeatsWildcard(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		withSelection(class("gold", memoryv1alpha1.DRAMOnly, true),
			&memoryv1alpha1.SelectionRules{NamespaceRules: []memoryv1alpha1.NamespaceRule{
				{Namespace: "*", Default: strp("silver")},
				{Namespace: "prod", Default: strp("gold")},
			}}),
		class("silver", memoryv1alpha1.Tiered, false),
	}
	r, err := Resolve(policies, "prod", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Policy.Name != "gold" {
		t.Fatalf("exact rule should beat wildcard, got %+v", r)
	}
	r, err = Resolve(policies, "dev", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Policy.Name != "silver" {
		t.Fatalf("wildcard should apply to other namespaces, got %+v", r)
	}
}

func TestForcedClassMustExist(t *testing.T) {
	policies := []memoryv1alpha1.MemoryPolicy{
		withSelection(class("gold", memoryv1alpha1.DRAMOnly, true),
			&memoryv1alpha1.SelectionRules{NamespaceRules: []memoryv1alpha1.NamespaceRule{{
				Namespace: "prod", Forced: strp("nonexistent"),
			}}}),
	}
	_, err := Resolve(policies, "prod", "")
	if !errors.Is(err, ErrUnknownClass) {
		t.Fatalf("want ErrUnknownClass for missing forced class, got %v", err)
	}
}

func TestDefaultedBacking(t *testing.T) {
	// DRAMOnly with no backing -> Hugepages 2Mi default
	r := &Resolved{Policy: &memoryv1alpha1.MemoryPolicy{Spec: memoryv1alpha1.MemoryPolicySpec{
		Mode: memoryv1alpha1.DRAMOnly}}}
	b := r.DefaultedBacking()
	if b.Type != memoryv1alpha1.HugepagesBacking {
		t.Fatalf("want default Hugepages, got %v", b.Type)
	}
	if b.HugepageSize == nil || b.HugepageSize.String() != "2Mi" {
		t.Fatalf("want default 2Mi, got %v", b.HugepageSize)
	}
	// explicit backing preserved
	q := resource.MustParse("1Gi")
	r2 := &Resolved{Policy: &memoryv1alpha1.MemoryPolicy{Spec: memoryv1alpha1.MemoryPolicySpec{
		Mode: memoryv1alpha1.DRAMOnly,
		Backing: &memoryv1alpha1.DRAMBacking{Type: memoryv1alpha1.MemLockBacking, HugepageSize: &q}}}}
	b2 := r2.DefaultedBacking()
	if b2.Type != memoryv1alpha1.MemLockBacking {
		t.Fatalf("want MemLock preserved, got %v", b2.Type)
	}
}

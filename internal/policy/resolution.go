// Package policy resolves a workload's memory policy class.
//
// Pure logic, unit-tested offline: given the set of MemoryPolicy
// classes, a namespace, and the workload's selection (annotation),
// Resolve returns the effective class and how it was chosen, or a
// typed error for rejection at admission.
//
// Semantics (from the requirements discussions — the AAA model):
//
//  1. The most specific namespace rule wins (exact match over "*").
//  2. A Forced class is used regardless of the workload's selection.
//  3. An explicit selection must name a defined class and must be
//     permitted (not forbidden; within a non-empty allowed list).
//     Violations are errors — never silent downgrades.
//  4. With no selection: the namespace rule's default, else the
//     cluster default (the class with spec.default=true).
//  5. DRAMOnly without a backing type defaults to Hugepages 2Mi;
//     Tiered ignores the backing.
package policy

import (
	"errors"
	"fmt"
	"sort"

	memoryv1alpha1 "github.com/shanemcd/memory-tier-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// SelectionSource records how the effective class was chosen — for the
// rendering status and observability.
type SelectionSource string

const (
	SourceForced           SelectionSource = "forced"
	SourceSelection        SelectionSource = "selection"
	SourceNamespaceDefault SelectionSource = "namespace-default"
	SourceClusterDefault   SelectionSource = "cluster-default"
)

// Workload selection annotation on the VMI.
const SelectionAnnotation = "memorytiering.shanemcd.com/policy-class"

// Typed errors: rejection at admission, never a silent downgrade.
var (
	ErrUnknownClass     = errors.New("unknown memory policy class")
	ErrForbiddenClass   = errors.New("memory policy class not permitted")
	ErrNoDefault        = errors.New("no memory policy class selected and no default defined")
	ErrAmbiguousDefault = errors.New("multiple cluster-default memory policy classes")
)

// Resolved is the effective class for a workload.
type Resolved struct {
	Policy *memoryv1alpha1.MemoryPolicy
	Source SelectionSource
}

// ruleFor returns the most specific namespace rule (exact over "*").
func ruleFor(policies []memoryv1alpha1.MemoryPolicy, namespace string) *memoryv1alpha1.NamespaceRule {
	var wildcard *memoryv1alpha1.NamespaceRule
	for i := range policies {
		sel := policies[i].Spec.Selection
		if sel == nil {
			continue
		}
		for j := range sel.NamespaceRules {
			r := &sel.NamespaceRules[j]
			switch r.Namespace {
			case namespace:
				return r // exact match wins immediately
			case "*":
				if wildcard == nil {
					wildcard = r
				}
			}
		}
	}
	return wildcard
}

// byName indexes the classes by name.
func byName(policies []memoryv1alpha1.MemoryPolicy) map[string]*memoryv1alpha1.MemoryPolicy {
	m := make(map[string]*memoryv1alpha1.MemoryPolicy, len(policies))
	for i := range policies {
		m[policies[i].Name] = &policies[i]
	}
	return m
}

// clusterDefault finds the single class marked spec.default=true.
func clusterDefault(policies []memoryv1alpha1.MemoryPolicy) (*memoryv1alpha1.MemoryPolicy, error) {
	var def *memoryv1alpha1.MemoryPolicy
	names := []string{}
	for i := range policies {
		if policies[i].Spec.Default {
			if def == nil {
				def = &policies[i]
			}
			names = append(names, policies[i].Name)
		}
	}
	if len(names) > 1 {
		sort.Strings(names)
		return nil, fmt.Errorf("%w: %v", ErrAmbiguousDefault, names)
	}
	return def, nil
}

// Resolve returns the effective class for a workload in a namespace
// with the given selection (the annotation value; empty = none).
func Resolve(policies []memoryv1alpha1.MemoryPolicy, namespace, selection string) (*Resolved, error) {
	classes := byName(policies)

	// Forced wins over the workload's selection (documented AAA
	// behavior; the rendering records the source so the override is
	// visible).
	if r := ruleFor(policies, namespace); r != nil && r.Forced != nil {
		p, ok := classes[*r.Forced]
		if !ok {
			return nil, fmt.Errorf("%w: forced class %q is not defined",
				ErrUnknownClass, *r.Forced)
		}
		return &Resolved{Policy: p, Source: SourceForced}, nil
	}

	if selection != "" {
		p, ok := classes[selection]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownClass, selection)
		}
		if r := ruleFor(policies, namespace); r != nil {
			for _, f := range r.Forbidden {
				if f == selection {
					return nil, fmt.Errorf("%w: %q is forbidden in namespace %q",
						ErrForbiddenClass, selection, namespace)
				}
			}
			if len(r.Allowed) > 0 {
				ok := false
				for _, a := range r.Allowed {
					if a == selection {
						ok = true
						break
					}
				}
				if !ok {
					return nil, fmt.Errorf("%w: %q is not in the allowed list for namespace %q",
						ErrForbiddenClass, selection, namespace)
				}
			}
		}
		return &Resolved{Policy: p, Source: SourceSelection}, nil
	}

	// No selection: namespace default, then cluster default.
	if r := ruleFor(policies, namespace); r != nil && r.Default != nil {
		p, ok := classes[*r.Default]
		if !ok {
			return nil, fmt.Errorf("%w: namespace default %q is not defined",
				ErrUnknownClass, *r.Default)
		}
		return &Resolved{Policy: p, Source: SourceNamespaceDefault}, nil
	}
	def, err := clusterDefault(policies)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, ErrNoDefault
	}
	return &Resolved{Policy: def, Source: SourceClusterDefault}, nil
}

// DefaultedBacking returns the backing for a resolved class with the
// documented defaulting applied (Hugepages 2Mi when unset). The
// renderer calls this only for DRAMOnly classes; for Tiered the
// backing is irrelevant and never rendered.
func (r *Resolved) DefaultedBacking() memoryv1alpha1.DRAMBacking {
	b := memoryv1alpha1.DRAMBacking{}
	if r.Policy.Spec.Backing != nil {
		b = *r.Policy.Spec.Backing
	}
	if b.Type == "" {
		b.Type = memoryv1alpha1.HugepagesBacking
	}
	if b.HugepageSize == nil || b.HugepageSize.IsZero() {
		q := resource.MustParse("2Mi")
		b.HugepageSize = &q
	}
	return b
}

// Package capacity publishes one consumable DRAM device per physical budget.
// Policy classes select the SAME device, not independently budgeted aliases.
// This is an opt-in producer prototype, not registered by the existing driver.
// It does not discover outside consumers or atomically admit unclaimed pods.
package capacity

import (
	"fmt"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	Memory           = resourceapi.QualifiedName("memorytier.openshift.io/memory")
	DeviceType       = resourceapi.QualifiedName("memorytier.openshift.io/type")
	MiB        int64 = 1024 * 1024
)

// Hold must refer to a disjoint outside-DRA physical reservation, not an alias
// or a DRA claim already accounted by the scheduler. Discovery/authorization
// of these inputs remains the responsibility of the caller.
type Hold struct {
	ID           string `json:"id"`
	Bytes        int64  `json:"bytes"`
	ManagedByDRA bool   `json:"managedByDRA"`
}

type Snapshot struct {
	Node                    string    `json:"node"`
	NodeUID                 string    `json:"nodeUID"`
	BootID                  string    `json:"bootID"`
	Revision                string    `json:"revision"`
	ObservedAt              time.Time `json:"observedAt"`
	TotalBytes              int64     `json:"totalBytes"`
	Outside                 []Hold    `json:"outside"`
	OutsideCoverageComplete bool      `json:"outsideCoverageComplete"`
}

type Identity struct{ NodeUID, BootID string }

// Available validates freshness/identity and conservatively rounds DOWN to MiB.
// Active claims are NOT subtracted here: native DRA allocation consumes them
// against this same capacity. Including them as Outside would double count.
func Available(s Snapshot, expected Identity, now time.Time, maxAge time.Duration) (int64, error) {
	if s.Node == "" || s.Revision == "" || s.NodeUID == "" || s.BootID == "" || expected.NodeUID == "" || expected.BootID == "" {
		return 0, fmt.Errorf("missing topology identity/revision")
	}
	if s.NodeUID != expected.NodeUID || s.BootID != expected.BootID {
		return 0, fmt.Errorf("topology identity changed")
	}
	if maxAge <= 0 || maxAge > 5*time.Minute || s.ObservedAt.IsZero() || s.ObservedAt.After(now) || now.Sub(s.ObservedAt) > maxAge {
		return 0, fmt.Errorf("missing, future or stale topology")
	}
	if !s.OutsideCoverageComplete {
		return 0, fmt.Errorf("outside-consumer coverage is unknown")
	}
	if s.TotalBytes < 0 {
		return 0, fmt.Errorf("negative physical capacity")
	}
	remaining := s.TotalBytes
	seen := map[string]bool{}
	for _, h := range s.Outside {
		if h.ID == "" || seen[h.ID] || h.Bytes < 0 || h.ManagedByDRA {
			return 0, fmt.Errorf("invalid/duplicate/already-DRA hold %q", h.ID)
		}
		seen[h.ID] = true
		// Subtract sequentially: no overflowing sum can wrap into free capacity.
		if h.Bytes > remaining {
			return 0, fmt.Errorf("outside reservations exceed physical budget")
		}
		remaining -= h.Bytes
	}
	return remaining / MiB * MiB, nil
}

// Slice emits a single device. BindingConditions keeps reservation-only lab
// pods unbound until an external driver explicitly reports Prepared. This
// function does not implement that preparation or verify runtime backing.
func Slice(s Snapshot, expected Identity, now time.Time, maxAge time.Duration, driver, name, namespaceName, namespaceUID string) (*resourceapi.ResourceSlice, error) {
	bytes, err := Available(s, expected, now, maxAge)
	if err != nil {
		return nil, err
	}
	if driver == "" || name == "" || namespaceName == "" || namespaceUID == "" {
		return nil, fmt.Errorf("missing publisher/ownership identity")
	}
	yes, no := true, false
	typ := "cxlsol-physical-dram"
	devices := []resourceapi.Device{}
	if bytes > 0 {
		one := resource.MustParse("1Mi")
		devices = append(devices, resourceapi.Device{
			Name: "physical-dram", AllowMultipleAllocations: &yes,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{DeviceType: {StringValue: &typ}},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{Memory: {
				Value:         *resource.NewQuantity(bytes, resource.BinarySI),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{Default: &one, ValidRange: &resourceapi.CapacityRequestPolicyRange{Min: &one, Step: &one}},
			}},
			BindsToNode: &yes, BindingConditions: []string{"lab.memory-tiering/Prepared"},
			BindingFailureConditions: []string{"lab.memory-tiering/PrepareFailed"},
		})
	}
	return &resourceapi.ResourceSlice{
		TypeMeta: metav1.TypeMeta{APIVersion: "resource.k8s.io/v1", Kind: "ResourceSlice"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
			"lab.memory-tiering/node-uid": s.NodeUID, "lab.memory-tiering/boot-id": s.BootID,
			"lab.memory-tiering/revision": s.Revision, "lab.memory-tiering/observed-at": s.ObservedAt.Format(time.RFC3339Nano),
		}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: namespaceName, UID: types.UID(namespaceUID), BlockOwnerDeletion: &no}}},
		Spec: resourceapi.ResourceSliceSpec{Driver: driver, NodeName: &s.Node,
			Pool: resourceapi.ResourcePool{Name: s.Node, Generation: 1, ResourceSliceCount: 1}, Devices: devices},
	}, nil
}

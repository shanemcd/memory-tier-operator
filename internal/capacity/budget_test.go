package capacity

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func fixture() (Snapshot, Identity, time.Time) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	return Snapshot{Node: "worker-cxl-0", NodeUID: "node-uid", BootID: "boot-id", Revision: "rev-1", ObservedAt: now,
		TotalBytes: 96 * MiB, OutsideCoverageComplete: true, Outside: []Hold{{ID: "system", Bytes: 32 * MiB}}}, Identity{NodeUID: "node-uid", BootID: "boot-id"}, now
}
func TestAvailable(t *testing.T) {
	cases := []struct {
		name     string
		change   func(*Snapshot)
		identity Identity
		age      time.Duration
	}{
		{name: "unknown outside consumers", change: func(s *Snapshot) { s.OutsideCoverageComplete = false }},
		{name: "missing revision", change: func(s *Snapshot) { s.Revision = "" }},
		{name: "missing identity", change: func(s *Snapshot) { s.BootID = "" }},
		{name: "node replaced", identity: Identity{NodeUID: "new", BootID: "boot-id"}},
		{name: "reboot", identity: Identity{NodeUID: "node-uid", BootID: "new"}},
		{name: "stale", age: 2 * time.Minute},
		{name: "future", age: -time.Second},
		{name: "missing timestamp", change: func(s *Snapshot) { s.ObservedAt = time.Time{} }},
		{name: "negative total", change: func(s *Snapshot) { s.TotalBytes = -1 }},
		{name: "negative hold", change: func(s *Snapshot) { s.Outside[0].Bytes = -1 }},
		{name: "missing hold ID", change: func(s *Snapshot) { s.Outside[0].ID = "" }},
		{name: "duplicate backing", change: func(s *Snapshot) { s.Outside = append(s.Outside, s.Outside[0]) }},
		{name: "claim double count", change: func(s *Snapshot) { s.Outside[0].ManagedByDRA = true }},
		{name: "overdraw", change: func(s *Snapshot) { s.Outside[0].Bytes = 97 * MiB }},
		{name: "overflow sum", change: func(s *Snapshot) {
			s.TotalBytes = math.MaxInt64
			s.Outside = []Hold{{ID: "a", Bytes: math.MaxInt64}, {ID: "b", Bytes: 1}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, id, now := fixture()
			if c.change != nil {
				c.change(&s)
			}
			if c.identity.NodeUID != "" {
				id = c.identity
			}
			if _, err := Available(s, id, now.Add(c.age), time.Minute); err == nil {
				t.Fatal("accepted invalid budget")
			}
		})
	}
	s, id, now := fixture()
	got, err := Available(s, id, now, time.Minute)
	if err != nil || got != 64*MiB {
		t.Fatalf("got %d %v", got, err)
	}
	s.TotalBytes++
	got, err = Available(s, id, now, time.Minute)
	if err != nil || got != 64*MiB {
		t.Fatalf("round down: %d %v", got, err)
	}
	for _, age := range []time.Duration{0, 6 * time.Minute} {
		if _, err := Available(s, id, now, age); err == nil {
			t.Fatal("bad max age accepted")
		}
	}
}
func TestSinglePhysicalSlice(t *testing.T) {
	s, id, now := fixture()
	slice, err := Slice(s, id, now, time.Minute, "fixture.memory-tiering.lab", "fixture", "ns", "ns-uid")
	if err != nil {
		t.Fatal(err)
	}
	if len(slice.Spec.Devices) != 1 || slice.Spec.Devices[0].Name != "physical-dram" {
		t.Fatal("duplicated physical budget")
	}
	device := slice.Spec.Devices[0]
	cap := device.Capacity[Memory]
	if cap.Value.Value() != 64*MiB || device.AllowMultipleAllocations == nil || !*device.AllowMultipleAllocations {
		t.Fatal("bad shared capacity")
	}
	if len(device.BindingConditions) != 1 || !*device.BindsToNode {
		t.Fatal("reservation fixture could bind without prepare")
	}
	if cap.RequestPolicy.Default.Value() != MiB || cap.RequestPolicy.ValidRange.Min.Value() != MiB {
		t.Fatal("unbounded implicit default")
	}
	b, err := json.Marshal(slice)
	if err != nil || len(b) == 0 {
		t.Fatal(err)
	}
	if slice.OwnerReferences[0].Name != "ns" || slice.OwnerReferences[0].UID != "ns-uid" {
		t.Fatal("missing owner")
	}
	// Pure producer must not mutate snapshots, and repeated output is deterministic.
	again, err := Slice(s, id, now, time.Minute, "fixture.memory-tiering.lab", "fixture", "ns", "ns-uid")
	bb, _ := json.Marshal(again)
	if err != nil || string(b) != string(bb) {
		t.Fatal("non-idempotent producer")
	}
	s.Outside[0].Bytes = 96 * MiB
	empty, err := Slice(s, id, now, time.Minute, "fixture.memory-tiering.lab", "fixture", "ns", "ns-uid")
	if err != nil || len(empty.Spec.Devices) != 0 {
		t.Fatal("zero budget must withdraw devices")
	}
	if _, err := Slice(s, id, now, time.Minute, "", "fixture", "ns", "ns-uid"); err == nil {
		t.Fatal("missing publisher accepted")
	}
}

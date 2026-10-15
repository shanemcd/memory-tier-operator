package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/shanemcd/memory-tier-operator/internal/discovery"
)

func TestDesiredResourcesDRAMOnly(t *testing.T) {
	inv := &discovery.Inventory{
		DRAMNode:   0,
		DRAMBytes:  resource.MustParse("8Gi"),
		CXLCommitted: false,
	}
	res := desiredResources("n1", inv)
	devices := res.Pools["n1"].Slices[0].Devices
	if len(devices) != 1 {
		t.Fatalf("DRAM-only node should publish exactly dram-0, got %d devices", len(devices))
	}
	if devices[0].Name != "dram-0" {
		t.Errorf("expected dram-0, got %s", devices[0].Name)
	}
	if got := devices[0].Attributes[attrType].StringValue; ptr.Deref(got, "") != "dram" {
		t.Errorf("dram-0 type = %v, want dram", got)
	}
}

func TestDesiredResourcesWithCXL(t *testing.T) {
	inv := &discovery.Inventory{
		DRAMNode:     0,
		DRAMBytes:    resource.MustParse("8125400Ki"),
		CXLNodes:     []int{1},
		CXLBytes:     resource.MustParse("4Gi"),
		CXLCommitted: true,
	}
	res := desiredResources("n1", inv)
	devices := res.Pools["n1"].Slices[0].Devices
	if len(devices) != 3 {
		t.Fatalf("CXL node should publish dram-0, cxl-0, tiered-0; got %d devices", len(devices))
	}
	byName := map[string]int{}
	for i, d := range devices {
		byName[d.Name] = i
	}
	for _, name := range []string{"dram-0", "cxl-0", "tiered-0"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing device %s", name)
		}
	}

	tiered := devices[byName["tiered-0"]]
	if got := ptr.Deref(tiered.Attributes[attrType].StringValue, ""); got != "tiered" {
		t.Errorf("tiered-0 type = %q, want tiered", got)
	}
	if got := ptr.Deref(tiered.Attributes[attrNumaNode].IntValue, -1); got != 0 {
		t.Errorf("tiered-0 numa_node = %d, want 0 (DRAM)", got)
	}
	if got := ptr.Deref(tiered.Attributes[attrDemoteNode].IntValue, -1); got != 1 {
		t.Errorf("tiered-0 demote_node = %d, want 1 (CXL)", got)
	}
	// The tiered capacity is the DRAM hot-set ledger.
	if tiered.Capacity[attrMemory].Value != inv.DRAMBytes {
		t.Errorf("tiered-0 capacity = %v, want the DRAM bytes %v",
			tiered.Capacity[attrMemory].Value, inv.DRAMBytes)
	}
	if !ptr.Deref(tiered.AllowMultipleAllocations, false) {
		t.Error("tiered-0 must allow multiple allocations")
	}
	// The CXL device keeps its own ledger (the CXL-only tier's accounting).
	cxl := devices[byName["cxl-0"]]
	if cxl.Capacity[attrMemory].Value != inv.CXLBytes {
		t.Errorf("cxl-0 capacity = %v, want the CXL bytes %v",
			cxl.Capacity[attrMemory].Value, inv.CXLBytes)
	}
}

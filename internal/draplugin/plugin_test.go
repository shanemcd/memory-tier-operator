package draplugin

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestNodesToMask(t *testing.T) {
	cases := []struct {
		nodes []int
		want  string
	}{
		{[]int{0}, "0"},
		{[]int{1}, "1"},
		{[]int{0, 1}, "0-1"},
		{[]int{1, 0}, "0-1"}, // unordered input
		{[]int{0, 1, 0}, "0-1"},
		{[]int{2, 5}, "2,5"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := nodesToMask(c.nodes); got != c.want {
			t.Errorf("nodesToMask(%v) = %q, want %q", c.nodes, got, c.want)
		}
	}
}

func TestPrepareBuildsUnionMaskFromTieredDevice(t *testing.T) {
	// The enforcement map with a tiered device spanning both nodes.
	p := &Plugin{
		NodeName: "worker-cxl-0",
		DeviceNodes: map[string][]int{
			"dram-0":   {0},
			"cxl-0":    {1},
			"tiered-0": {0, 1},
		},
		Prepared: map[types.UID]*PreparedInfo{},
	}
	// Simulate the node-union logic over an allocation of tiered-0:
	nodes := []int{}
	for _, dev := range []string{"tiered-0"} {
		nodes = append(nodes, p.DeviceNodes[dev]...)
	}
	if got, want := nodesToMask(nodes), "0-1"; got != want {
		t.Fatalf("tiered allocation mask = %q, want %q", got, want)
	}
	// A DRAM-only claim stays single-node.
	nodes = append([]int{}, p.DeviceNodes["dram-0"]...)
	if got, want := nodesToMask(nodes), "0"; got != want {
		t.Fatalf("dram allocation mask = %q, want %q", got, want)
	}
	// fullMask spans every known node.
	if got, want := p.fullMask(), "0-1"; got != want {
		t.Fatalf("fullMask = %q, want %q", got, want)
	}
}

func TestFullMaskFlattensMultiNodeDevices(t *testing.T) {
	p := &Plugin{
		DeviceNodes: map[string][]int{
			"dram-0":   {0},
			"cxl-0":    {3},
			"tiered-0": {0, 3},
		},
	}
	if got, want := p.fullMask(), "0,3"; got != want {
		t.Fatalf("fullMask = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(p.DeviceNodes["tiered-0"], []int{0, 3}) {
		t.Fatalf("tiered nodes = %v, want [0 3]", p.DeviceNodes["tiered-0"])
	}
}

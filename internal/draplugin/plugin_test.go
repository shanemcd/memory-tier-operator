package draplugin

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestPodCgroupPathFindsGuaranteedAndQoSLayers(t *testing.T) {
	uid := "5ec78b20-5d0c-4367-bbb8-09b4c084554d"
	escaped := "5ec78b20_5d0c_4367_bbb8_09b4c084554d"

	// Guaranteed pods live directly under kubepods.slice with no QoS
	// segment; Burstable/BestEffort pods sit in a kubepods-<qos>.slice
	// subdirectory. Both must resolve.
	for name, layout := range map[string]string{
		"guaranteed":  "kubepods.slice/kubepods-pod" + escaped + ".slice",
		"burstable":   "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + escaped + ".slice",
		"besteffort":  "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + escaped + ".slice",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			orig := cgroupRoot
			cgroupRoot = dir
			t.Cleanup(func() { cgroupRoot = orig })

			if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(layout)), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := podCgroupPath(types.UID(uid)); filepath.Clean(got) != filepath.Join(dir, filepath.FromSlash(layout)) {
				t.Errorf("podCgroupPath(%s) = %q, want %q", uid, got, layout)
			}
		})
	}
}

func TestPodCgroupPathAbsent(t *testing.T) {
	dir := t.TempDir()
	orig := cgroupRoot
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = orig })

	if got := podCgroupPath(types.UID("00000000-0000-0000-0000-000000000000")); got != "" {
		t.Errorf("podCgroupPath(missing) = %q, want empty", got)
	}
}

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

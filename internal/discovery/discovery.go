// Package discovery reads the node's NUMA/CXL topology from /sys and turns
// it into the memory-tier pool inventory.
//
// Model (host-transparent tiering): CXL regions are committed and
// onlined as System RAM on CPU-less NUMA nodes; DRAM is the memory of the
// CPU nodes. A node counts as CXL tier iff it has online memory, no CPUs,
// and at least one committed CXL region exists on the host.
package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Inventory is the discovered per-node topology.
type Inventory struct {
	// DRAMNode is the NUMA node backing the DRAM tier (the lab has one).
	DRAMNode int
	// CXLNodes are the CPU-less memory nodes backing the CXL tier.
	CXLNodes []int
	// CXLBytes is the total online memory across CXLNodes.
	CXLBytes resource.Quantity
	// CXLCommitted is true when at least one CXL region is committed.
	CXLCommitted bool
}

// sysfs roots, overridable for tests.
var (
	nodeSysPath = "/sys/devices/system/node"
	cxlSysPath  = "/sys/bus/cxl/devices"
)

// Discover reads the topology.
func Discover() (*Inventory, error) {
	inv := &Inventory{DRAMNode: -1}

	nodes, err := onlineNodes()
	if err != nil {
		return nil, err
	}

	for _, node := range nodes {
		hasCPU, err := nodeHasCPUs(node)
		if err != nil {
			return nil, err
		}
		if hasCPU {
			if inv.DRAMNode == -1 {
				inv.DRAMNode = node
			}
			continue
		}
		// CPU-less node with online memory: CXL candidate.
		kb, err := nodeMemTotalKB(node)
		if err != nil {
			return nil, err
		}
		if kb > 0 {
			inv.CXLNodes = append(inv.CXLNodes, node)
			inv.CXLBytes.Add(resource.MustParse(fmt.Sprintf("%dKi", kb)))
		}
	}

	committed, err := anyRegionCommitted()
	if err != nil {
		return nil, err
	}
	inv.CXLCommitted = committed
	if !committed {
		inv.CXLNodes = nil
		inv.CXLBytes = resource.Quantity{}
	}

	if inv.DRAMNode == -1 {
		return nil, fmt.Errorf("no CPU NUMA node found — cannot identify the DRAM tier")
	}
	return inv, nil
}

func onlineNodes() ([]int, error) {
	raw, err := os.ReadFile(filepath.Join(nodeSysPath, "online"))
	if err != nil {
		return nil, fmt.Errorf("read node online list: %w", err)
	}
	return parseCPUList(strings.TrimSpace(string(raw)))
}

func nodeHasCPUs(node int) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(nodeSysPath, fmt.Sprintf("node%d", node), "cpulist"))
	if err != nil {
		return false, fmt.Errorf("read node %d cpulist: %w", node, err)
	}
	cpus, err := parseCPUList(strings.TrimSpace(string(raw)))
	if err != nil {
		return false, err
	}
	return len(cpus) > 0, nil
}

func nodeMemTotalKB(node int) (int64, error) {
	raw, err := os.ReadFile(filepath.Join(nodeSysPath, fmt.Sprintf("node%d", node), "meminfo"))
	if err != nil {
		return 0, fmt.Errorf("read node %d meminfo: %w", node, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		// "Node 1 MemTotal:  4194304 kB" -> [Node 1 MemTotal: 4194304 kB]
		if fields := strings.Fields(line); len(fields) == 5 && fields[2] == "MemTotal:" {
			return strconv.ParseInt(fields[3], 10, 64)
		}
	}
	return 0, fmt.Errorf("node %d meminfo has no MemTotal", node)
}

func anyRegionCommitted() (bool, error) {
	commits, err := filepath.Glob(filepath.Join(cxlSysPath, "region*", "commit"))
	if err != nil {
		return false, err
	}
	for _, f := range commits {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue // region disappearing underneath us: not fatal
		}
		if strings.TrimSpace(string(raw)) == "1" {
			return true, nil
		}
	}
	return false, nil
}

// parseCPUList parses "0-3,8" / "0" / "" style lists.
func parseCPUList(list string) ([]int, error) {
	if list == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(list, ",") {
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, err := strconv.Atoi(lo)
			if err != nil {
				return nil, fmt.Errorf("parse %q: %w", list, err)
			}
			h, err := strconv.Atoi(hi)
			if err != nil {
				return nil, fmt.Errorf("parse %q: %w", list, err)
			}
			for i := l; i <= h; i++ {
				out = append(out, i)
			}
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", list, err)
		}
		out = append(out, v)
	}
	return out, nil
}

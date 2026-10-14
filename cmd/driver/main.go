// memory-tier-driver: the host-transparent memory-tiering DRA driver.
//
// It discovers the node's DRAM/CXL NUMA topology, publishes tiered pools as
// ResourceSlices (byte-granular capacity, k8s >= 1.36), and serves the
// kubelet DRA plugin interface. Phase A: discovery + publishing +
// registration with claim logging. Phase B adds cpuset.mems enforcement.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/shanemcd/memory-tier-operator/internal/discovery"
	"github.com/shanemcd/memory-tier-operator/internal/draplugin"
)

const (
	attrDomain     = "memorytier.openshift.io"
	attrType       = attrDomain + "/type"
	attrNumaNode   = attrDomain + "/numa_node"
	attrMemory     = attrDomain + "/memory"
	defaultDriver  = "memorytier.openshift.io"
	pluginDataRoot = "/var/lib/kubelet/plugins"
)

func main() {
	driverName := flag.String("driver-name", defaultDriver, "DRA driver name")
	resync := flag.Duration("resync", 30*time.Second, "topology resync interval")
	flag.Parse()

	klog.InitFlags(nil)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := klog.FromContext(ctx)

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		logger.Error(nil, "NODE_NAME env (downward API) is required")
		os.Exit(1)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error(err, "in-cluster config failed")
		os.Exit(1)
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "clientset failed")
		os.Exit(1)
	}

	// The kubeletplugin helper requires its data dir to exist.
	pluginDataDir := pluginDataRoot + "/" + *driverName
	if err := os.MkdirAll(pluginDataDir, 0o755); err != nil {
		logger.Error(err, "cannot create plugin data dir", "dir", pluginDataDir)
		os.Exit(1)
	}

	// Slice publisher: the upstream controller owns pool generations, slice
	// naming, and garbage collection (Node owner => nodeName + GC on node
	// deletion). We just keep the desired resources updated.
	rsController, err := resourceslice.StartController(ctx, resourceslice.Options{
		DriverName: *driverName,
		KubeClient: kubeClient,
		Owner:      &resourceslice.Owner{APIVersion: "v1", Kind: "Node", Name: nodeName},
		Resources:  &resourceslice.DriverResources{Pools: map[string]resourceslice.Pool{}},
	})
	if err != nil {
		logger.Error(err, "resourceslice controller failed to start")
		os.Exit(1)
	}

	// Discovery loop: refresh the desired resources on every tick and on
	// start; the controller reconciles only on change.
	// Shared device topology: device name -> NUMA node, maintained by the
	// discovery loop and consumed by the enforcement path.
	deviceNodes := map[string]int{}
	go func() {
		update := func() {
			inv, err := discovery.Discover()
			if err != nil {
				logger.Error(err, "discovery failed; keeping last published resources")
				return
			}
			res := desiredResources(nodeName, inv)
			rsController.Update(res)
			// Keep the enforcement map in step with what we publish.
			deviceNodes["dram-0"] = inv.DRAMNode
			delete(deviceNodes, "cxl-0")
			if len(inv.CXLNodes) > 0 {
				deviceNodes["cxl-0"] = inv.CXLNodes[0]
			}
			logger.Info("topology published",
				"dramNode", inv.DRAMNode,
				"cxlNodes", inv.CXLNodes,
				"dramBytes", inv.DRAMBytes.String(),
				"cxlBytes", inv.CXLBytes.String(),
				"cxlCommitted", inv.CXLCommitted,
			)
		}
		update()
		ticker := time.NewTicker(*resync)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				update()
			}
		}
	}()

	// Kubelet plugin: registration + NodePrepareResources enforcement.
	plugin := &draplugin.Plugin{
		NodeName:    nodeName,
		Client:      kubeClient,
		DeviceNodes: deviceNodes,
		Prepared:    map[types.UID]*draplugin.PreparedInfo{},
	}
	helper, err := kubeletplugin.Start(ctx, plugin,
		kubeletplugin.DriverName(*driverName),
		kubeletplugin.KubeClient(kubeClient),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.PluginDataDirectoryPath(pluginDataDir),
	)
	if err != nil {
		logger.Error(err, "kubelet plugin failed to start")
		os.Exit(1)
	}
	defer func() {
		helper.Stop()
	}()

	<-ctx.Done()
	logger.Info("shutting down")
}

// desiredResources turns the inventory into the published pools: one pool
// per node, one device per tier. The DRAM device carries the node's full
// DRAM (the stock scheduler handles capacity); the CXL device carries the
// node's full CXL memory (the DRA pool is its only capacity accounting).
func desiredResources(nodeName string, inv *discovery.Inventory) *resourceslice.DriverResources {
	devices := []resourceapi.Device{
		{
			Name: "dram-0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				attrType:     {StringValue: ptr.To("dram")},
				attrNumaNode: {IntValue: ptr.To(int64(inv.DRAMNode))},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				attrMemory: {Value: inv.DRAMBytes},
			},
			AllowMultipleAllocations: ptr.To(true),
		},
	}
	if len(inv.CXLNodes) > 0 {
		// The lab publishes a single CXL device per node; multi-region
		// nodes would publish one device per NUMA node.
		cxlNode := inv.CXLNodes[0]
		devices = append(devices, resourceapi.Device{
			Name: "cxl-0",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				attrType:     {StringValue: ptr.To("cxl")},
				attrNumaNode: {IntValue: ptr.To(int64(cxlNode))},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				attrMemory: {Value: inv.CXLBytes},
			},
			AllowMultipleAllocations: ptr.To(true),
		})
	}
	return &resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {
				Slices: []resourceslice.Slice{{Devices: devices}},
			},
		},
	}
}

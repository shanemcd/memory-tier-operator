// Opt-in reservation-only lab producer. No controller or existing driver change.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/shanemcd/memory-tier-operator/internal/capacity"
	resourceapi "k8s.io/api/resource/v1"
)

type input struct {
	Snapshot     capacity.Snapshot `json:"snapshot"`
	NodeUID      string            `json:"nodeUID"`
	BootID       string            `json:"bootID"`
	Driver       string            `json:"driver"`
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace"`
	NamespaceUID string            `json:"namespaceUID"`
	Mode         string            `json:"mode"`
}

func run() error {
	var in input
	dec := json.NewDecoder(os.Stdin)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return err
	}
	if in.Mode != "shared" && in.Mode != "aliases" {
		return fmt.Errorf("unknown fixture mode")
	}
	s, err := capacity.Slice(in.Snapshot, capacity.Identity{NodeUID: in.NodeUID, BootID: in.BootID}, time.Now(), time.Minute,
		in.Driver, in.Name, in.Namespace, in.NamespaceUID)
	if err != nil {
		return err
	}
	if in.Mode == "aliases" && len(s.Spec.Devices) != 0 {
		// Deliberately reproduce the installed alias defect for the counterexample.
		// Only fixture-specific type values: never match installed broad classes.
		original := s.Spec.Devices[0]
		s.Spec.Devices = nil
		for _, name := range []string{"dram", "tiered"} {
			device := *original.DeepCopy()
			typ := "cxlsol-alias-" + name
			device.Name = name
			device.Attributes[capacity.DeviceType] = resourceapi.DeviceAttribute{StringValue: &typ}
			s.Spec.Devices = append(s.Spec.Devices, device)
		}
	}
	return json.NewEncoder(os.Stdout).Encode(s)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Package cdi writes the static CDI spec for the published functions.
package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	specs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/inerplat/dra-driver-vfio-nic/pkg/inventory"
)

// VFIO backends a device can be exposed with. A container runtime picks
// whichever node it finds, but a VM runtime attaches one device per node it
// is given, so every CDI device carries exactly one backend.
const (
	BackendLegacy  = "legacy"
	BackendIommufd = "iommufd"
)

// specVersion is the oldest CDI version with device node host paths, so that
// older runtimes still accept the spec.
const specVersion = "0.6.0"

// Writer writes one spec file for the whole node.
type Writer struct {
	Dir  string // e.g. /var/run/cdi
	Kind string // vendor/class, e.g. vfio-nic.inerplat.github.io/nic
}

// DeviceID returns the qualified CDI name of a device with a backend.
func (w Writer) DeviceID(device, backend string) string {
	return w.Kind + "=" + device + "-" + backend
}

// Write replaces the spec with one entry per device and backend. The iommufd
// entry is left out for a function without a VFIO cdev.
func (w Writer) Write(devices []inventory.Device) error {
	spec := specs.Spec{Version: specVersion, Kind: w.Kind}
	for _, d := range devices {
		fn := d.Function
		spec.Devices = append(spec.Devices, specs.Device{
			Name: d.Name + "-" + BackendLegacy,
			ContainerEdits: specs.ContainerEdits{DeviceNodes: []*specs.DeviceNode{
				charNode("/dev/vfio/" + fn.IOMMUGroup),
				charNode("/dev/vfio/vfio"),
			}},
		})
		if fn.Cdev != "" {
			spec.Devices = append(spec.Devices, specs.Device{
				Name: d.Name + "-" + BackendIommufd,
				ContainerEdits: specs.ContainerEdits{DeviceNodes: []*specs.DeviceNode{
					charNode("/dev/vfio/devices/" + fn.Cdev),
					charNode("/dev/iommu"),
				}},
			})
		}
	}

	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(w.Dir, strings.ReplaceAll(w.Kind, "/", "_")+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func charNode(path string) *specs.DeviceNode {
	return &specs.DeviceNode{Path: path, HostPath: path, Type: "c"}
}

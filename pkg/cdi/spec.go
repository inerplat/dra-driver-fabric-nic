// Package cdi writes the static CDI spec for the published functions.
package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	specs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/inventory"
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

// GuestRDMADevice names the CDI device with the RDMA verbs nodes as a VM
// guest numbers them. ib_uverbs gives its first 32 devices the fixed char
// major 231 and minors 192-223 in probe order. The guest's own probe order is
// not known here, so the device lists every one of those nodes; verbs
// libraries open only the nodes sysfs names, so unused ones stay idle.
//
// The nodes are meant to be created inside the guest by a runtime that runs
// the container in a VM. On a container sharing the host kernel they would
// name the host's RDMA devices.
const GuestRDMADevice = "guest-rdma"

const (
	uverbsMajor     = 231
	uverbsMinorBase = 192
	uverbsFixed     = 32
)

// Writer writes one spec file for the whole node.
type Writer struct {
	Dir  string // e.g. /var/run/cdi
	Kind string // vendor/class, e.g. fabric-nic.inerplat.github.io/nic
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

	spec.Devices = append(spec.Devices, guestRDMA())

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

func guestRDMA() specs.Device {
	mode := os.FileMode(0o666)
	var nodes []*specs.DeviceNode
	for i := 0; i < uverbsFixed; i++ {
		nodes = append(nodes, &specs.DeviceNode{
			Path:     fmt.Sprintf("/dev/infiniband/uverbs%d", i),
			Type:     "c",
			Major:    uverbsMajor,
			Minor:    uverbsMinorBase + int64(i),
			FileMode: &mode,
		})
	}
	return specs.Device{Name: GuestRDMADevice, ContainerEdits: specs.ContainerEdits{DeviceNodes: nodes}}
}

// GuestRDMAID returns the qualified CDI name of the guest RDMA device.
func (w Writer) GuestRDMAID() string {
	return w.Kind + "=" + GuestRDMADevice
}

func charNode(path string) *specs.DeviceNode {
	return &specs.DeviceNode{Path: path, HostPath: path, Type: "c"}
}

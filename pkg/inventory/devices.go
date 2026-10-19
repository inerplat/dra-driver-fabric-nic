// Package inventory turns the scanned functions into ResourceSlice devices.
package inventory

import (
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/sysfs"
)

// Attribute names. Names without a domain belong to the driver's domain.
const (
	AttrFunction resourceapi.QualifiedName = "function" // "pf" or "vf"
	AttrLinkType resourceapi.QualifiedName = "linkType"
	AttrVendor   resourceapi.QualifiedName = "vendor"
	AttrDevice   resourceapi.QualifiedName = "device"
	AttrParentPF resourceapi.QualifiedName = "parentPF"

	// AttrNUMANode has the name and form the NVIDIA GPU driver publishes, so
	// a claim can match NICs and GPUs on it.
	AttrNUMANode resourceapi.QualifiedName = deviceattribute.StandardDeviceAttributePrefix + "numaNode"
)

// Device is a published function with what preparing it needs.
type Device struct {
	Name     string
	Function sysfs.Function
	Slice    resourceapi.Device
}

// DeviceName derives a device name from a PCI address: 0000:9c:00.0 becomes
// 0000-9c-00-0.
func DeviceName(bdf string) string {
	return strings.NewReplacer(":", "-", ".", "-").Replace(bdf)
}

// Build makes one device per function. extra holds attributes keyed by PCI
// address from the attribute file.
func Build(funcs []sysfs.Function, extra map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, mods ...deviceattribute.MachineModifier) []Device {
	devices := make([]Device, 0, len(funcs))
	for _, fn := range funcs {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			AttrFunction: {StringValue: ptr.To(functionKind(fn))},
			AttrLinkType: {StringValue: ptr.To(fn.LinkType())},
			AttrVendor:   {StringValue: ptr.To(fn.Vendor)},
			AttrDevice:   {StringValue: ptr.To(fn.Device)},
		}
		if fn.VF {
			attrs[AttrParentPF] = resourceapi.DeviceAttribute{StringValue: ptr.To(fn.ParentPF)}
		}
		if fn.NUMANode >= 0 {
			attrs[AttrNUMANode] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(fn.NUMANode))}
		}
		if a, err := deviceattribute.GetPCIBusIDAttribute(fn.BDF); err == nil {
			attrs[a.Name] = a.Value
		}
		if a, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(fn.BDF, mods...); err == nil {
			attrs[a.Name] = a.Value
		}
		for name, value := range extra[fn.BDF] {
			attrs[name] = value
		}
		name := DeviceName(fn.BDF)
		devices = append(devices, Device{
			Name:     name,
			Function: fn,
			Slice:    resourceapi.Device{Name: name, Attributes: attrs},
		})
	}
	return devices
}

// Reserved reports whether an attribute name is set by the driver itself and
// may not come from the attribute file.
func Reserved(driverName string) func(resourceapi.QualifiedName) bool {
	return func(name resourceapi.QualifiedName) bool {
		s := string(name)
		return strings.HasPrefix(s, driverName+"/") || strings.HasPrefix(s, deviceattribute.StandardDeviceAttributePrefix)
	}
}

func functionKind(fn sysfs.Function) string {
	if fn.VF {
		return "vf"
	}
	return "pf"
}

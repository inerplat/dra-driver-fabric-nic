// Package plugin answers the kubelet's prepare and unprepare calls.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/inerplat/dra-driver-vfio-nic/pkg/cdi"
	"github.com/inerplat/dra-driver-vfio-nic/pkg/inventory"
)

// ConfigKind is the kind of the opaque config a class or claim can pass.
const ConfigKind = "NicConfig"

// NicConfig is the opaque per-request configuration.
type NicConfig struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// VfioBackend is "legacy" or "iommufd"; empty keeps the driver default.
	VfioBackend string `json:"vfioBackend,omitempty"`
}

// Plugin prepares claims from the current inventory. Preparing writes
// nothing: the CDI spec already lists every device, so a claim only needs
// the names. Unprepare has nothing to undo.
type Plugin struct {
	DriverName     string
	ConfigVersion  string // apiVersion accepted in NicConfig
	DefaultBackend string
	CDI            cdi.Writer

	mu      sync.RWMutex
	devices map[string]inventory.Device
}

// SetDevices replaces the inventory used by later prepare calls.
func (p *Plugin) SetDevices(devices []inventory.Device) {
	m := make(map[string]inventory.Device, len(devices))
	for _, d := range devices {
		m[d.Name] = d
	}
	p.mu.Lock()
	p.devices = m
	p.mu.Unlock()
}

func (p *Plugin) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	out := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		devices, err := p.prepare(claim)
		if err != nil {
			klog.FromContext(ctx).Error(err, "prepare failed", "claim", klog.KObj(claim))
		}
		out[claim.UID] = kubeletplugin.PrepareResult{Devices: devices, Err: err}
	}
	return out, nil
}

func (p *Plugin) prepare(claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim %s/%s is not allocated", claim.Namespace, claim.Name)
	}
	backends, err := p.backendsByRequest(claim.Status.Allocation.Devices.Config)
	if err != nil {
		return nil, err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	var prepared []kubeletplugin.Device
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver != p.DriverName {
			continue
		}
		d, ok := p.devices[r.Device]
		if !ok {
			return nil, fmt.Errorf("device %s is no longer on this node", r.Device)
		}
		backend := p.DefaultBackend
		if b, ok := backends[r.Request]; ok {
			backend = b
		} else if b, ok := backends[""]; ok {
			backend = b
		}
		if backend == cdi.BackendIommufd && d.Function.Cdev == "" {
			return nil, fmt.Errorf("device %s has no VFIO cdev for the iommufd backend", r.Device)
		}
		prepared = append(prepared, kubeletplugin.Device{
			Requests:     []string{r.Request},
			PoolName:     r.Pool,
			DeviceName:   r.Device,
			CDIDeviceIDs: []string{p.CDI.DeviceID(d.Name, backend)},
			Metadata:     &kubeletplugin.DeviceMetadata{Attributes: metadataAttributes(d)},
		})
	}
	return prepared, nil
}

// backendsByRequest resolves the backend per request name, "" standing for
// configs that name no request. Class configs come first, so a claim config
// overrides them, and a later config overrides an earlier one.
func (p *Plugin) backendsByRequest(configs []resourceapi.DeviceAllocationConfiguration) (map[string]string, error) {
	ordered := make([]resourceapi.DeviceAllocationConfiguration, len(configs))
	copy(ordered, configs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Source == resourceapi.AllocationConfigSourceClass && ordered[j].Source != resourceapi.AllocationConfigSourceClass
	})

	out := map[string]string{}
	for _, c := range ordered {
		if c.Opaque == nil || c.Opaque.Driver != p.DriverName {
			continue
		}
		var cfg NicConfig
		if err := json.Unmarshal(c.Opaque.Parameters.Raw, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s config: %w", ConfigKind, err)
		}
		if cfg.Kind != ConfigKind || cfg.APIVersion != p.ConfigVersion {
			return nil, fmt.Errorf("unsupported config %s %s, want %s %s", cfg.APIVersion, cfg.Kind, p.ConfigVersion, ConfigKind)
		}
		switch cfg.VfioBackend {
		case "":
			continue
		case cdi.BackendLegacy, cdi.BackendIommufd:
		default:
			return nil, fmt.Errorf("unknown vfioBackend %q", cfg.VfioBackend)
		}
		if len(c.Requests) == 0 {
			out[""] = cfg.VfioBackend
			continue
		}
		for _, req := range c.Requests {
			out[req] = cfg.VfioBackend
		}
	}
	return out, nil
}

// metadataAttributes is what a VM runtime reads to find the device on the
// host; KubeVirt assigns the device at this PCI address.
func metadataAttributes(d inventory.Device) map[string]resourceapi.DeviceAttribute {
	attrs := map[string]resourceapi.DeviceAttribute{
		string(deviceattribute.StandardDeviceAttributePCIBusID): {StringValue: ptr.To(d.Function.BDF)},
	}
	if v, ok := d.Slice.Attributes[deviceattribute.StandardDeviceAttributePCIeRoot]; ok {
		attrs[string(deviceattribute.StandardDeviceAttributePCIeRoot)] = v
	}
	return attrs
}

func (p *Plugin) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	out := make(map[types.UID]error, len(claims))
	for _, c := range claims {
		out[c.UID] = nil
	}
	return out, nil
}

func (p *Plugin) HandleError(ctx context.Context, err error, msg string) {
	klog.FromContext(ctx).Error(err, msg)
}

package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	specs "tags.cncf.io/container-device-interface/specs-go"

	"github.com/inerplat/dra-driver-vfio-nic/pkg/cdi"
	"github.com/inerplat/dra-driver-vfio-nic/pkg/inventory"
	"github.com/inerplat/dra-driver-vfio-nic/pkg/sysfs"
)

const driver = "vfio-nic.example.com"

func newPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := &Plugin{
		DriverName:     driver,
		ConfigVersion:  driver + "/v1alpha1",
		DefaultBackend: cdi.BackendLegacy,
		CDI:            cdi.Writer{Dir: t.TempDir(), Kind: driver + "/nic"},
	}
	devices := inventory.Build([]sysfs.Function{
		{BDF: "0000:9c:00.0", Class: 0x020700, NUMANode: 1, IOMMUGroup: "60", Cdev: "vfio3"},
		{BDF: "0000:bc:00.0", Class: 0x020700, NUMANode: 1, IOMMUGroup: "70"},
	}, nil, deviceattribute.WithFSFromRoot(t.TempDir()))
	if err := p.CDI.Write(devices); err != nil {
		t.Fatal(err)
	}
	p.SetDevices(devices)
	return p
}

func opaque(backend string, source resourceapi.AllocationConfigSource, requests ...string) resourceapi.DeviceAllocationConfiguration {
	return opaqueConfig(NicConfig{VfioBackend: backend}, source, requests...)
}

func opaqueConfig(cfg NicConfig, source resourceapi.AllocationConfigSource, requests ...string) resourceapi.DeviceAllocationConfiguration {
	cfg.APIVersion, cfg.Kind = driver+"/v1alpha1", ConfigKind
	raw, _ := json.Marshal(cfg)
	return resourceapi.DeviceAllocationConfiguration{
		Source:   source,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
			Driver:     driver,
			Parameters: runtime.RawExtension{Raw: raw},
		}},
	}
}

func claim(configs ...resourceapi.DeviceAllocationConfiguration) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{}
	c.UID = "uid-1"
	c.Status.Allocation = &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{
		Results: []resourceapi.DeviceRequestAllocationResult{
			{Request: "nic0", Driver: driver, Pool: "node-a", Device: "0000-9c-00-0"},
			{Request: "nic1", Driver: driver, Pool: "node-a", Device: "0000-bc-00-0"},
			{Request: "gpu0", Driver: "gpu.nvidia.com", Pool: "node-a", Device: "gpu-0"},
		},
		Config: configs,
	}}
	return c
}

func prepare(t *testing.T, p *Plugin, c *resourceapi.ResourceClaim) ([]string, error) {
	t.Helper()
	res, err := p.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{c})
	if err != nil {
		t.Fatal(err)
	}
	r := res[c.UID]
	var ids []string
	for _, d := range r.Devices {
		ids = append(ids, d.CDIDeviceIDs...)
		if d.Metadata == nil || d.Metadata.Attributes[string(deviceattribute.StandardDeviceAttributePCIBusID)].StringValue == nil {
			t.Errorf("device %s has no pciBusID metadata", d.DeviceName)
		}
	}
	return ids, r.Err
}

func TestPrepareBackends(t *testing.T) {
	p := newPlugin(t)

	ids, err := prepare(t, p, claim())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{driver + "/nic=0000-9c-00-0-legacy", driver + "/nic=0000-bc-00-0-legacy"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("default: %v, want %v", ids, want)
	}

	// A claim config for one request overrides the class config.
	ids, err = prepare(t, p, claim(
		opaque(cdi.BackendIommufd, resourceapi.AllocationConfigSourceClaim, "nic0"),
		opaque(cdi.BackendLegacy, resourceapi.AllocationConfigSourceClass),
	))
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != driver+"/nic=0000-9c-00-0-iommufd" || ids[1] != driver+"/nic=0000-bc-00-0-legacy" {
		t.Errorf("override: %v", ids)
	}

	// iommufd on a function without a cdev fails the claim.
	if _, err := prepare(t, p, claim(opaque(cdi.BackendIommufd, resourceapi.AllocationConfigSourceClaim))); err == nil {
		t.Errorf("iommufd without cdev accepted")
	}
	if _, err := prepare(t, p, claim(opaque("both", resourceapi.AllocationConfigSourceClaim))); err == nil {
		t.Errorf("unknown backend accepted")
	}
}

func TestPrepareMissingDevice(t *testing.T) {
	p := newPlugin(t)
	p.SetDevices(nil)
	if _, err := prepare(t, p, claim()); err == nil {
		t.Errorf("prepare of a device that left the node succeeded")
	}
}

func TestCDISpecHasOneBackendPerDevice(t *testing.T) {
	p := newPlugin(t)
	b, err := os.ReadFile(filepath.Join(p.CDI.Dir, strings.ReplaceAll(p.CDI.Kind, "/", "_")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	nodes := map[string][]string{}
	for _, d := range spec.Devices {
		if d.Name == cdi.GuestRDMADevice {
			if len(d.ContainerEdits.DeviceNodes) != 32 {
				t.Errorf("guest RDMA device has %d nodes", len(d.ContainerEdits.DeviceNodes))
			}
			last := d.ContainerEdits.DeviceNodes[31]
			if last.Path != "/dev/infiniband/uverbs31" || last.Major != 231 || last.Minor != 223 || last.HostPath != "" {
				t.Errorf("last guest node %+v", last)
			}
			continue
		}
		for _, n := range d.ContainerEdits.DeviceNodes {
			nodes[d.Name] = append(nodes[d.Name], n.Path)
		}
	}
	want := map[string]string{
		"0000-9c-00-0-legacy":  "/dev/vfio/60,/dev/vfio/vfio",
		"0000-9c-00-0-iommufd": "/dev/vfio/devices/vfio3,/dev/iommu",
		"0000-bc-00-0-legacy":  "/dev/vfio/70,/dev/vfio/vfio",
	}
	if len(nodes) != len(want) {
		t.Errorf("spec devices %v", nodes)
	}
	for name, paths := range want {
		if got := strings.Join(nodes[name], ","); got != paths {
			t.Errorf("%s: %s, want %s", name, got, paths)
		}
	}
}

func TestPrepareGuestRDMA(t *testing.T) {
	p := newPlugin(t)
	guest := driver + "/nic=" + cdi.GuestRDMADevice

	ids, err := prepare(t, p, claim(opaqueConfig(NicConfig{GuestRDMA: true}, resourceapi.AllocationConfigSourceClass)))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, id := range ids {
		if id == guest {
			n++
		}
	}
	if n != 1 {
		t.Errorf("guest RDMA device listed %d times: %v", n, ids)
	}

	// Without the class setting the claim gets no guest nodes.
	ids, _ = prepare(t, p, claim())
	for _, id := range ids {
		if id == guest {
			t.Errorf("guest RDMA device without the class setting")
		}
	}

	// A claim cannot turn it on.
	if _, err := prepare(t, p, claim(opaqueConfig(NicConfig{GuestRDMA: true}, resourceapi.AllocationConfigSourceClaim))); err == nil {
		t.Errorf("guestRDMA from a claim accepted")
	}

	// A claim config that changes only the backend keeps the class setting.
	ids, err = prepare(t, p, claim(
		opaqueConfig(NicConfig{GuestRDMA: true}, resourceapi.AllocationConfigSourceClass),
		opaque(cdi.BackendIommufd, resourceapi.AllocationConfigSourceClaim, "nic0"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != driver+"/nic=0000-9c-00-0-iommufd" || ids[1] != guest {
		t.Errorf("backend override with guest RDMA: %v", ids)
	}
}

func TestPrepareGuestRDMASkipsEthernet(t *testing.T) {
	p := newPlugin(t)
	p.SetDevices(inventory.Build([]sysfs.Function{
		{BDF: "0000:9c:00.0", Class: 0x020000, IOMMUGroup: "60"},
		{BDF: "0000:bc:00.0", Class: 0x020000, IOMMUGroup: "70"},
	}, nil, deviceattribute.WithFSFromRoot(t.TempDir())))
	ids, err := prepare(t, p, claim(opaqueConfig(NicConfig{GuestRDMA: true}, resourceapi.AllocationConfigSourceClass)))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == driver+"/nic="+cdi.GuestRDMADevice {
			t.Errorf("guest RDMA device for Ethernet functions: %v", ids)
		}
	}
}

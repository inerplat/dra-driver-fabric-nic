package sysfs

import (
	"testing"

	"github.com/inerplat/dra-driver-fabric-nic/internal/sysfstest"
)

func testTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := sysfstest.BuildTestTree(root, []sysfstest.TestFunc{
		// A whole PF on vfio-pci next to a GPU.
		{BDF: "0000:9c:00.0", Root: "pci0000:97", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", NUMA: 1, Group: "60", Cdev: "vfio3"},
		// A PF still on its host driver, with a VF on vfio-pci.
		{BDF: "0000:9d:00.0", Root: "pci0000:97", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "mlx5_core", NumVFs: 1, NUMA: 1, Group: "61"},
		{BDF: "0000:9d:00.1", Root: "pci0000:97", Class: "0x020700", Vendor: "0x15b3", Device: "0x101c", Driver: "vfio-pci", PhysFn: "0000:9d:00.0", NUMA: 1, Group: "62"},
		// A PF on vfio-pci that still has VFs enabled.
		{BDF: "0000:4e:00.0", Root: "pci0000:4b", Class: "0x020000", Vendor: "0x15b3", Device: "0x1021", Driver: "vfio-pci", NumVFs: 2, NUMA: 0, Group: "20"},
		// A GPU on vfio-pci: not a network function.
		{BDF: "0000:9b:00.0", Root: "pci0000:97", Class: "0x030200", Vendor: "0x10de", Device: "0x2330", Driver: "vfio-pci", NUMA: 1, Group: "59"},
		// An Ethernet VF on the vendor's vfio variant.
		{BDF: "0000:31:00.2", Root: "pci0000:30", Class: "0x020000", Vendor: "0x15b3", Device: "0x101e", Driver: "mlx5_vfio_pci", PhysFn: "0000:31:00.0", NUMA: -1, Group: "33"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestScan(t *testing.T) {
	funcs, skipped, err := Scan(testTree(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Function{}
	for _, f := range funcs {
		got[f.BDF] = f
	}
	if len(got) != 3 {
		t.Fatalf("found %d functions, want 3: %+v", len(got), funcs)
	}

	pf := got["0000:9c:00.0"]
	if pf.VF || pf.LinkType() != LinkInfiniband || pf.NUMANode != 1 || pf.IOMMUGroup != "60" || pf.Cdev != "vfio3" || pf.Vendor != "15b3" || pf.Device != "101b" {
		t.Errorf("whole PF: %+v", pf)
	}
	vf := got["0000:9d:00.1"]
	if !vf.VF || vf.ParentPF != "0000:9d:00.0" || vf.Cdev != "" {
		t.Errorf("VF: %+v", vf)
	}
	eth := got["0000:31:00.2"]
	if eth.LinkType() != LinkEthernet || eth.NUMANode != -1 {
		t.Errorf("ethernet VF: %+v", eth)
	}
	if _, ok := got["0000:4e:00.0"]; ok {
		t.Errorf("PF with VFs enabled was published")
	}
	if len(skipped) != 1 {
		t.Errorf("skipped = %v, want the PF with VFs", skipped)
	}
	if funcs[0].BDF != "0000:31:00.2" {
		t.Errorf("not sorted by BDF: %v", funcs)
	}
}

func TestIsVFIODriver(t *testing.T) {
	for name, want := range map[string]bool{
		"vfio-pci":      true,
		"mlx5_vfio_pci": true,
		"mlx5_core":     false,
		"":              false,
	} {
		if got := IsVFIODriver(name); got != want {
			t.Errorf("IsVFIODriver(%q) = %v", name, got)
		}
	}
}

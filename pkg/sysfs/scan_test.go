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

func TestScanIOMMUGroups(t *testing.T) {
	root := t.TempDir()
	if err := sysfstest.BuildTestTree(root, []sysfstest.TestFunc{
		// Shares group 70 with a function still on its host driver.
		{BDF: "0000:bc:00.0", Root: "pci0000:b7", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", Group: "70"},
		{BDF: "0000:bc:00.1", Root: "pci0000:b7", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "mlx5_core", Group: "70"},
		// Two vfio functions in group 80: both are held back.
		{BDF: "0000:cc:00.0", Root: "pci0000:c7", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", Group: "80"},
		{BDF: "0000:cc:00.1", Root: "pci0000:c7", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", Group: "80"},
		// A bridge in the group does not need a driver.
		{BDF: "0000:da:00.0", Root: "pci0000:d7", Class: "0x060400", Vendor: "0x10b5", Device: "0xc010", Group: "90"},
		{BDF: "0000:dc:00.0", Root: "pci0000:d7", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", Group: "90"},
	}); err != nil {
		t.Fatal(err)
	}
	funcs, skipped, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(funcs) != 1 || funcs[0].BDF != "0000:dc:00.0" {
		t.Errorf("published %+v, skipped %v", funcs, skipped)
	}
	if len(skipped) != 3 {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestGPUs(t *testing.T) {
	root := t.TempDir()
	if err := sysfstest.BuildTestTree(root, []sysfstest.TestFunc{
		{BDF: "0000:9b:00.0", Root: "pci0000:97", Class: "0x030200", Vendor: "0x10de", Device: "0x2330", Driver: "vfio-pci"},
		// A BMC VGA: display class from a non-GPU vendor.
		{BDF: "0000:03:00.0", Root: "pci0000:00", Class: "0x030000", Vendor: "0x1a03", Device: "0x2000", Driver: "ast"},
		// A display-class GPU from an allowed vendor.
		{BDF: "0000:41:00.0", Root: "pci0000:40", Class: "0x030000", Vendor: "0x10de", Device: "0x2236"},
	}); err != nil {
		t.Fatal(err)
	}
	gpus, excluded, err := GPUs(root, []string{"0x10de"})
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 || gpus[0].BDF != "0000:41:00.0" || gpus[1].BDF != "0000:9b:00.0" {
		t.Errorf("gpus %+v", gpus)
	}
	if len(excluded) != 1 {
		t.Errorf("excluded %v", excluded)
	}
}

func TestHostIBPorts(t *testing.T) {
	root := t.TempDir()
	if err := sysfstest.BuildTestTree(root, []sysfstest.TestFunc{
		{BDF: "0000:9c:00.0", Root: "pci0000:97", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "mlx5_core", Group: "60",
			IBDev: "mlx5_4", PortState: "4: ACTIVE", SMLid: "0x1", GID0: "fe80:0000:0000:0000:58a2:e103:0080:35a8", Netdev: "ibp156s0"},
		{BDF: "0000:bc:00.0", Root: "pci0000:b7", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", Group: "70"},
	}); err != nil {
		t.Fatal(err)
	}
	ports, err := HostIBPorts(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 1 {
		t.Fatalf("ports %+v", ports)
	}
	p := ports[0]
	if p.IBDev != "mlx5_4" || p.PortState != "ACTIVE" || p.SMLid != 1 || p.Prefix != "fe80:0000:0000:0000" || len(p.Netdevs) != 1 || p.IOMMUGroup != "60" {
		t.Errorf("port %+v", p)
	}
}

func TestScanRefusesGroupWithAGPU(t *testing.T) {
	root := t.TempDir()
	if err := sysfstest.BuildTestTree(root, []sysfstest.TestFunc{
		{BDF: "0000:9b:00.0", Root: "pci0000:97", Class: "0x030200", Vendor: "0x10de", Device: "0x2330", Driver: "vfio-pci", Group: "50"},
		{BDF: "0000:9c:00.0", Root: "pci0000:97", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", Group: "50"},
	}); err != nil {
		t.Fatal(err)
	}
	if funcs, _, _ := Scan(root); len(funcs) != 0 {
		t.Errorf("NIC sharing a group with a GPU was published: %+v", funcs)
	}
	if all, _ := VFIOIBFunctions(root); len(all) != 1 {
		t.Errorf("unfiltered list %v", all)
	}
}

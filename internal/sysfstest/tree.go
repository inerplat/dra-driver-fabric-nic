// Package sysfstest lays out fake sysfs trees for tests.
package sysfstest

import (
	"fmt"
	"os"
	"path/filepath"
)

// TestFunc describes one PCI function for BuildTestTree.
type TestFunc struct {
	BDF    string
	Root   string // root bus, e.g. "pci0000:97"
	Class  string // e.g. "0x020700"
	Vendor string // e.g. "0x15b3"
	Device string
	Driver string
	PhysFn string // parent PF BDF for a VF
	NumVFs int
	NUMA   int
	Group  string
	Cdev   string

	// IBDev, when set, adds infiniband/<IBDev>/ports/1 with PortState,
	// SMLid and GID0 (the first GID, prefix first).
	IBDev     string
	PortState string // e.g. "4: ACTIVE"
	SMLid     string // e.g. "0x1"
	GID0      string // e.g. "fe80:0000:0000:0000:58a2:e103:0080:35a8"
	Netdev    string // adds net/<Netdev>
}

// BuildTestTree lays out a minimal sysfs under root for tests: the device
// directories under devices/<Root>/ and the bus/pci/devices symlinks.
func BuildTestTree(root string, funcs []TestFunc) error {
	bus := filepath.Join(root, "bus", "pci", "devices")
	if err := os.MkdirAll(bus, 0o755); err != nil {
		return err
	}
	for _, f := range funcs {
		dev := filepath.Join(root, "devices", f.Root, f.BDF)
		if err := os.MkdirAll(dev, 0o755); err != nil {
			return err
		}
		files := map[string]string{
			"class":     f.Class,
			"vendor":    f.Vendor,
			"device":    f.Device,
			"numa_node": fmt.Sprint(f.NUMA),
		}
		if f.PhysFn == "" {
			files["sriov_numvfs"] = fmt.Sprint(f.NumVFs)
		}
		for name, v := range files {
			if err := os.WriteFile(filepath.Join(dev, name), []byte(v+"\n"), 0o644); err != nil {
				return err
			}
		}
		links := map[string]string{}
		if f.Driver != "" {
			links["driver"] = "../../../bus/pci/drivers/" + f.Driver
		}
		if f.PhysFn != "" {
			links["physfn"] = "../" + f.PhysFn
		}
		if f.Group != "" {
			links["iommu_group"] = "../../../kernel/iommu_groups/" + f.Group
		}
		for name, target := range links {
			if err := os.Symlink(target, filepath.Join(dev, name)); err != nil {
				return err
			}
		}
		if f.Cdev != "" {
			if err := os.MkdirAll(filepath.Join(dev, "vfio-dev", f.Cdev), 0o755); err != nil {
				return err
			}
		}
		if err := os.Symlink(filepath.Join("..", "..", "..", "devices", f.Root, f.BDF), filepath.Join(bus, f.BDF)); err != nil {
			return err
		}
		if f.Group != "" {
			g := filepath.Join(root, "kernel", "iommu_groups", f.Group, "devices")
			if err := os.MkdirAll(g, 0o755); err != nil {
				return err
			}
			if err := os.Symlink(filepath.Join("..", "..", "..", "..", "devices", f.Root, f.BDF), filepath.Join(g, f.BDF)); err != nil {
				return err
			}
		}
		if f.IBDev != "" {
			port := filepath.Join(dev, "infiniband", f.IBDev, "ports", "1")
			if err := os.MkdirAll(filepath.Join(port, "gids"), 0o755); err != nil {
				return err
			}
			for name, v := range map[string]string{"state": f.PortState, "sm_lid": f.SMLid, "gids/0": f.GID0} {
				if err := os.WriteFile(filepath.Join(port, name), []byte(v+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
		if f.Netdev != "" {
			if err := os.MkdirAll(filepath.Join(dev, "net", f.Netdev), 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

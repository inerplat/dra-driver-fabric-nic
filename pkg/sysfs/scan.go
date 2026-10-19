// Package sysfs finds the network functions a node has handed to vfio-pci.
package sysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Link types derived from the PCI subclass of a network controller.
const (
	LinkEthernet   = "ethernet"
	LinkInfiniband = "infiniband"
	LinkOther      = "other"
)

// Function is one PCI network function bound to a VFIO driver.
type Function struct {
	BDF        string
	Vendor     string // four hex digits, e.g. "15b3"
	Device     string // four hex digits
	Class      uint32 // 24-bit PCI class code, e.g. 0x020700
	VF         bool
	ParentPF   string // BDF of the physical function; empty for a PF
	NUMANode   int    // -1 when the platform reports none
	IOMMUGroup string // e.g. "57"
	Cdev       string // e.g. "vfio3"; empty without iommufd support
}

// LinkType maps the PCI subclass to a link type.
func (f Function) LinkType() string {
	switch (f.Class >> 8) & 0xff {
	case 0x00:
		return LinkEthernet
	case 0x07:
		return LinkInfiniband
	default:
		return LinkOther
	}
}

// IsVFIODriver reports whether a kernel driver name is vfio-pci or one of its
// vendor variants, such as mlx5_vfio_pci.
func IsVFIODriver(name string) bool {
	return name == "vfio-pci" || strings.HasSuffix(name, "_vfio_pci") || strings.HasSuffix(name, "-vfio-pci")
}

// Scan returns the network functions under root (normally "/sys") that are
// bound to a VFIO driver, sorted by BDF. A physical function that still has
// VFs enabled is skipped, because handing it out would take the VFs with it.
func Scan(root string) ([]Function, []string, error) {
	devDir := filepath.Join(root, "bus", "pci", "devices")
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", devDir, err)
	}

	var found []Function
	var skipped []string
	for _, e := range entries {
		bdf := e.Name()
		dir := filepath.Join(devDir, bdf)

		class, err := readHex(filepath.Join(dir, "class"))
		if err != nil || class>>16 != 0x02 {
			continue
		}
		driver := linkBase(filepath.Join(dir, "driver"))
		if !IsVFIODriver(driver) {
			continue
		}

		fn := Function{BDF: bdf, Class: uint32(class), NUMANode: -1}
		if fn.Vendor, err = readID(filepath.Join(dir, "vendor")); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", bdf, err))
			continue
		}
		if fn.Device, err = readID(filepath.Join(dir, "device")); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", bdf, err))
			continue
		}
		if parent := linkBase(filepath.Join(dir, "physfn")); parent != "" {
			fn.VF = true
			fn.ParentPF = parent
		} else if n, err := readInt(filepath.Join(dir, "sriov_numvfs")); err == nil && n > 0 {
			skipped = append(skipped, fmt.Sprintf("%s: physical function has %d VFs enabled", bdf, n))
			continue
		}
		if n, err := readInt(filepath.Join(dir, "numa_node")); err == nil {
			fn.NUMANode = n
		}
		fn.IOMMUGroup = linkBase(filepath.Join(dir, "iommu_group"))
		if fn.IOMMUGroup == "" {
			skipped = append(skipped, fmt.Sprintf("%s: no IOMMU group", bdf))
			continue
		}
		if cdevs, err := os.ReadDir(filepath.Join(dir, "vfio-dev")); err == nil && len(cdevs) == 1 {
			fn.Cdev = cdevs[0].Name()
		}
		found = append(found, fn)
	}

	sort.Slice(found, func(i, j int) bool { return found[i].BDF < found[j].BDF })
	return found, skipped, nil
}

func linkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func readHex(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 32)
}

func readID(path string) (string, error) {
	v, err := readHex(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04x", v), nil
}

func readInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

package prep

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/sysfs"
)

// Binder moves a PCI function to vfio-pci.
type Binder interface {
	BindVFIO(bdf string) error
}

// SysfsBinder binds through driver_override, which survives the unbind, then
// lets the kernel probe the function again.
type SysfsBinder struct {
	SysRoot string
}

func (b SysfsBinder) BindVFIO(bdf string) error {
	if _, err := os.Stat(filepath.Join(b.SysRoot, "bus", "pci", "drivers", "vfio-pci")); err != nil {
		return fmt.Errorf("vfio-pci driver is not loaded: %w", err)
	}
	dev := filepath.Join(b.SysRoot, "bus", "pci", "devices", bdf)
	if err := os.WriteFile(filepath.Join(dev, "driver_override"), []byte("vfio-pci"), 0o200); err != nil {
		return fmt.Errorf("set driver_override: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(dev, "driver")); err == nil {
		if err := os.WriteFile(filepath.Join(dev, "driver", "unbind"), []byte(bdf), 0o200); err != nil {
			return fmt.Errorf("unbind: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(b.SysRoot, "bus", "pci", "drivers_probe"), []byte(bdf), 0o200); err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	target, err := os.Readlink(filepath.Join(dev, "driver"))
	if err != nil || filepath.Base(target) != "vfio-pci" {
		return fmt.Errorf("%s is not on vfio-pci after probing", bdf)
	}
	return nil
}

// groupPlan checks that the IOMMU group of a function holds nothing but the
// function and bridges, the same rule the plugin publishes by, so prep never
// detaches a port the plugin would refuse to publish.
func groupPlan(sysRoot, group, bdf string) error {
	members, err := sysfs.GroupMembers(sysRoot, group)
	if err != nil {
		return err
	}
	if other := sysfs.OtherMembers(members, bdf); len(other) > 0 {
		return fmt.Errorf("iommu group %s also holds %v", group, other)
	}
	return nil
}

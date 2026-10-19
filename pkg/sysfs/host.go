package sysfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Member is one PCI function of an IOMMU group.
type Member struct {
	BDF    string
	Class  uint32
	Driver string
}

// IsBridge reports whether the member is a PCI bridge, which never needs a
// driver for its group to be usable by VFIO.
func (m Member) IsBridge() bool { return m.Class>>8 == 0x0604 }

// GroupMembers lists the functions of an IOMMU group.
func GroupMembers(root, group string) ([]Member, error) {
	dir := filepath.Join(root, "kernel", "iommu_groups", group, "devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Member
	for _, e := range entries {
		dev := filepath.Join(root, "bus", "pci", "devices", e.Name())
		class, _ := readHex(filepath.Join(dev, "class"))
		out = append(out, Member{BDF: e.Name(), Class: uint32(class), Driver: linkBase(filepath.Join(dev, "driver"))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BDF < out[j].BDF })
	return out, nil
}

// OtherMembers lists the members of a group, other than bdf, that are not
// bridges.
func OtherMembers(members []Member, bdf string) []string {
	var out []string
	for _, m := range members {
		if m.BDF != bdf && !m.IsBridge() {
			out = append(out, m.BDF)
		}
	}
	return out
}

// NetworkPF is a network-class physical function, on any driver or none.
type NetworkPF struct {
	BDF        string
	Vendor     string
	Class      uint32
	Driver     string
	IOMMUGroup string
	NumVFs     int
	IBDev      string
	Netdevs    []string
}

// NetworkPFs lists every network-class PF under root, of any vendor and
// whatever it is bound to, so that nothing prep cannot probe goes unseen. A
// device it cannot read is an error, not a gap.
func NetworkPFs(root string) ([]NetworkPF, error) {
	devDir := filepath.Join(root, "bus", "pci", "devices")
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return nil, err
	}
	var out []NetworkPF
	for _, e := range entries {
		dir := filepath.Join(devDir, e.Name())
		class, err := readHex(filepath.Join(dir, "class"))
		if err != nil {
			return nil, fmt.Errorf("%s: read class: %w", e.Name(), err)
		}
		if class>>16 != 0x02 || linkBase(filepath.Join(dir, "physfn")) != "" {
			continue
		}
		vendor, err := readID(filepath.Join(dir, "vendor"))
		if err != nil {
			return nil, fmt.Errorf("%s: read vendor: %w", e.Name(), err)
		}
		p := NetworkPF{BDF: e.Name(), Vendor: vendor, Class: uint32(class), Driver: linkBase(filepath.Join(dir, "driver")), IOMMUGroup: linkBase(filepath.Join(dir, "iommu_group"))}
		switch n, err := readInt(filepath.Join(dir, "sriov_numvfs")); {
		case err == nil:
			p.NumVFs = n
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("%s: read sriov_numvfs: %w", e.Name(), err)
		}
		if ibdevs, err := os.ReadDir(filepath.Join(dir, "infiniband")); err == nil && len(ibdevs) > 0 {
			p.IBDev = ibdevs[0].Name()
		}
		if nets, err := os.ReadDir(filepath.Join(dir, "net")); err == nil {
			for _, n := range nets {
				p.Netdevs = append(p.Netdevs, n.Name())
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BDF < out[j].BDF })
	return out, nil
}

// GPU is a PCI display-class device counted as a GPU.
type GPU struct {
	BDF    string
	Class  uint32
	Vendor string
}

// GPUs returns the GPUs under root whatever driver they are bound to: every
// 3D controller (class 0x0302), and other display-class devices only from
// the given vendors. The rest of the display class, such as a BMC's VGA, is
// returned as excluded with the reason.
func GPUs(root string, vendors []string) (gpus []GPU, excluded []string, err error) {
	devDir := filepath.Join(root, "bus", "pci", "devices")
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return nil, nil, err
	}
	allowed := map[string]bool{}
	for _, v := range vendors {
		allowed[strings.ToLower(strings.TrimPrefix(v, "0x"))] = true
	}
	for _, e := range entries {
		dir := filepath.Join(devDir, e.Name())
		class, err := readHex(filepath.Join(dir, "class"))
		if err != nil || class>>16 != 0x03 {
			continue
		}
		vendor, _ := readID(filepath.Join(dir, "vendor"))
		if class>>8 == 0x0302 || allowed[vendor] {
			gpus = append(gpus, GPU{BDF: e.Name(), Class: uint32(class), Vendor: vendor})
			continue
		}
		excluded = append(excluded, e.Name()+": display class 0x"+strconv.FormatUint(class, 16)+" from vendor "+vendor+" is not a GPU vendor")
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].BDF < gpus[j].BDF })
	return gpus, excluded, nil
}

// HostPort is an InfiniBand function on its host driver, with its first port
// as sysfs shows it.
type HostPort struct {
	BDF        string
	Driver     string
	VF         bool
	NumVFs     int
	IOMMUGroup string
	IBDev      string
	// Ports counts the ports of every ibdev of the function. Only a function
	// with exactly one port is described by the fields below.
	Ports     int
	PortState string // e.g. "ACTIVE"
	SMLid     uint64
	Prefix    string // GID subnet prefix, e.g. "fe80:0000:0000:0000"
	Netdevs   []string
}

// HostIBPorts returns the InfiniBand functions under root that are bound to a
// non-VFIO driver and expose an ibdev.
func HostIBPorts(root string) ([]HostPort, error) {
	devDir := filepath.Join(root, "bus", "pci", "devices")
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return nil, err
	}
	var out []HostPort
	for _, e := range entries {
		dir := filepath.Join(devDir, e.Name())
		class, err := readHex(filepath.Join(dir, "class"))
		if err != nil || class>>8 != 0x0207 {
			continue
		}
		driver := linkBase(filepath.Join(dir, "driver"))
		if driver == "" || IsVFIODriver(driver) {
			continue
		}
		p := HostPort{BDF: e.Name(), Driver: driver, IOMMUGroup: linkBase(filepath.Join(dir, "iommu_group"))}
		p.VF = linkBase(filepath.Join(dir, "physfn")) != ""
		p.NumVFs, _ = readInt(filepath.Join(dir, "sriov_numvfs"))
		if ibdevs, err := os.ReadDir(filepath.Join(dir, "infiniband")); err == nil && len(ibdevs) > 0 {
			for _, d := range ibdevs {
				ports, _ := os.ReadDir(filepath.Join(dir, "infiniband", d.Name(), "ports"))
				p.Ports += len(ports)
			}
			p.IBDev = ibdevs[0].Name()
			port := filepath.Join(dir, "infiniband", p.IBDev, "ports", "1")
			if b, err := os.ReadFile(filepath.Join(port, "state")); err == nil {
				s := strings.TrimSpace(string(b))
				if i := strings.Index(s, ": "); i >= 0 {
					s = s[i+2:]
				}
				p.PortState = s
			}
			if v, err := readHex(filepath.Join(port, "sm_lid")); err == nil {
				p.SMLid = v
			}
			if b, err := os.ReadFile(filepath.Join(port, "gids", "0")); err == nil {
				if parts := strings.Split(strings.TrimSpace(string(b)), ":"); len(parts) == 8 {
					p.Prefix = strings.Join(parts[:4], ":")
				}
			}
		}
		if nets, err := os.ReadDir(filepath.Join(dir, "net")); err == nil {
			for _, n := range nets {
				p.Netdevs = append(p.Netdevs, n.Name())
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BDF < out[j].BDF })
	return out, nil
}

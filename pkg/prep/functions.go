package prep

import (
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/sysfs"
)

// MellanoxVendor is the PCI vendor whose network functions prep binds.
const MellanoxVendor = "15b3"

// Function is one network PF of the node as prep sees it at start.
type Function struct {
	BDF        string `json:"bdf"`
	Vendor     string `json:"vendor"`
	ID         string `json:"id,omitempty"` // <VPD serial>-f<PCI function>
	IDError    string `json:"idError,omitempty"`
	InfiniBand bool   `json:"infiniband"`
	Driver     string `json:"driver"`
	Root       string `json:"pcieRoot"`
	GPU        string `json:"gpu,omitempty"` // a GPU under the same PCIe root
	// NVLinkManagement marks a ConnectX bridge that manages NVLink switches.
	NVLinkManagement bool     `json:"nvlinkManagement,omitempty"`
	IOMMUGroup       string   `json:"iommuGroup"`
	NumVFs           int      `json:"numVFs,omitempty"`
	IBDev            string   `json:"ibdev,omitempty"`
	Netdevs          []string `json:"netdevs,omitempty"`
}

var serialRe = regexp.MustCompile(`^[A-Za-z0-9]{1,48}$`)

// enumerate lists every network PF, of any vendor, on any driver or none,
// with its stable ID. A function whose VPD cannot be read, has no usable
// serial, or shares its ID with another gets no ID.
func enumerate(sysRoot string, gpus []GPUInfo, vpdTimeout time.Duration) ([]Function, error) {
	pfs, err := sysfs.NetworkPFs(sysRoot)
	if err != nil {
		return nil, err
	}
	gpuOnRoot := map[string]string{}
	for _, g := range gpus {
		if _, ok := gpuOnRoot[g.Root]; !ok && g.Root != "" {
			gpuOnRoot[g.Root] = g.BDF
		}
	}
	out := make([]Function, 0, len(pfs))
	seen := map[string][]int{}
	for _, p := range pfs {
		f := Function{
			BDF: p.BDF, Vendor: p.Vendor, InfiniBand: p.Class>>8 == 0x0207, Driver: p.Driver, Root: pcieRoot(sysRoot, p.BDF),
			IOMMUGroup: p.IOMMUGroup, NumVFs: p.NumVFs, IBDev: p.IBDev, Netdevs: p.Netdevs,
		}
		if f.Root != "" {
			f.GPU = gpuOnRoot[f.Root]
		}
		vpd, err := sysfs.ReadVPD(filepath.Join(sysRoot, "bus", "pci", "devices", p.BDF), vpdTimeout)
		switch {
		case err != nil:
			f.IDError = fmt.Sprintf("cannot read VPD: %v", err)
		case !serialRe.MatchString(vpd.Serial):
			f.IDError = fmt.Sprintf("VPD serial %q is not usable as an ID", vpd.Serial)
		default:
			f.ID = vpd.Serial + "-f" + p.BDF[len(p.BDF)-1:]
		}
		f.NVLinkManagement = vpd.NVLinkManagement
		if f.ID != "" {
			seen[f.ID] = append(seen[f.ID], len(out))
		}
		out = append(out, f)
	}
	for id, idx := range seen {
		if len(idx) > 1 {
			for _, i := range idx {
				out[i].ID, out[i].IDError = "", fmt.Sprintf("ID %s is shared by %d functions", id, len(idx))
			}
		}
	}
	return out, nil
}

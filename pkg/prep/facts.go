package prep

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/dynamic-resource-allocation/deviceattribute"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/sysfs"
)

// Prober asks the fabric about a port. Its answers need the port on its host
// driver, through the kernel's management datagram interface.
type Prober interface {
	// SMGUID returns the GUID of the master subnet manager the port sees.
	SMGUID(ctx context.Context, ibdev string, port int) (string, error)
	// Neighbor describes the switch the port is cabled to.
	Neighbor(ctx context.Context, ibdev string, port int) (string, error)
}

// DiagsProber uses infiniband-diags: sminfo asks the master subnet manager
// for its SMInfo, and smpquery reads the neighbor over a directed route.
type DiagsProber struct {
	Timeout time.Duration
}

func (d DiagsProber) SMGUID(ctx context.Context, ibdev string, port int) (string, error) {
	out, err := d.run(ctx, "sminfo", "-C", ibdev, "-P", fmt.Sprint(port))
	if err != nil {
		return "", err
	}
	return parseSMInfo(out)
}

// parseSMInfo reads the GUID of a master subnet manager from sminfo's
// one-line answer, for example
//
//	sminfo: sm lid 1 sm guid 0xa088c20300eeae30, activity count 52 priority 15 state 3 SMINFO_MASTER
//
// An answer from a subnet manager that is not the master is refused: its
// GUID does not identify the fabric the port is managed by.
func parseSMInfo(out string) (string, error) {
	fields := strings.Fields(strings.ReplaceAll(out, ",", " "))
	guid, state := "", ""
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case "guid":
			guid = strings.ToLower(fields[i+1])
		case "state":
			state = fields[i+1]
		}
	}
	if !strings.HasPrefix(guid, "0x") || len(guid) < 3 {
		return "", fmt.Errorf("sminfo: no GUID in %q", strings.TrimSpace(out))
	}
	if _, err := strconv.ParseUint(guid[2:], 16, 64); err != nil {
		return "", fmt.Errorf("sminfo: GUID %q is not hexadecimal", guid)
	}
	if state != "3" {
		return "", fmt.Errorf("sminfo: subnet manager %s is in state %q, not master", guid, state)
	}
	return guid, nil
}

func (d DiagsProber) Neighbor(ctx context.Context, ibdev string, port int) (string, error) {
	out, err := d.run(ctx, "smpquery", "-C", ibdev, "-P", fmt.Sprint(port), "-D", "nodedesc", "0,1")
	if err != nil {
		return "", err
	}
	desc := strings.TrimSpace(out)
	if i := strings.Index(desc, "Node Description:"); i >= 0 {
		desc = strings.Trim(strings.TrimSpace(desc[i+len("Node Description:"):]), ".")
	}
	return desc, nil
}

func (d DiagsProber) run(ctx context.Context, name string, args ...string) (string, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// readFacts reads one pass of facts for every InfiniBand function on its host
// driver, except those in skip. VFs inherit their PF's fabric and are not
// probed.
func readFacts(ctx context.Context, sysRoot string, prober Prober, skip map[string]bool) ([]PortFacts, error) {
	ports, err := sysfs.HostIBPorts(sysRoot)
	if err != nil {
		return nil, err
	}
	var out []PortFacts
	for _, p := range ports {
		if p.VF || skip[p.BDF] {
			continue
		}
		f := PortFacts{
			BDF: p.BDF, Root: pcieRoot(sysRoot, p.BDF), Driver: p.Driver, IBDev: p.IBDev,
			NumVFs: p.NumVFs, IOMMUGroup: p.IOMMUGroup, Netdevs: p.Netdevs,
			State: p.PortState, Prefix: p.Prefix, SMLid: p.SMLid,
		}
		switch {
		case p.IBDev == "":
			f.Status, f.StatusErr = FactRefused, "no ibdev"
		case p.Ports != 1:
			f.Status, f.StatusErr = FactUnsupported, fmt.Sprintf("function has %d InfiniBand ports; only single-port functions are classified", p.Ports)
		case p.PortState != "ACTIVE":
			f.Status = FactPortNotActive
		case p.Prefix == "" || p.SMLid == 0:
			f.Status, f.StatusErr = FactRefused, "no subnet prefix or SM LID in sysfs"
		default:
			guid, err := prober.SMGUID(ctx, p.IBDev, 1)
			if err != nil {
				f.Status, f.StatusErr = FactRefused, err.Error()
			} else {
				f.Status, f.SMGUID = FactOK, guid
				if n, err := prober.Neighbor(ctx, p.IBDev, 1); err == nil {
					f.Neighbor = n
				}
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func pcieRoot(sysRoot, bdf string) string {
	a, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(bdf, deviceattribute.WithFSFromRoot(sysRoot))
	if err != nil || a.Value.StringValue == nil {
		return ""
	}
	return *a.Value.StringValue
}

// gpuInfo returns the GPUs with their PCIe roots.
func gpuInfo(sysRoot string, vendors []string) ([]GPUInfo, []string, error) {
	gpus, excluded, err := sysfs.GPUs(sysRoot, vendors)
	if err != nil {
		return nil, nil, err
	}
	out := make([]GPUInfo, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, GPUInfo{BDF: g.BDF, Root: pcieRoot(sysRoot, g.BDF)})
	}
	return out, excluded, nil
}

// Package prep reads fabric facts from InfiniBand ports that are still on
// their host driver, infers which ports belong to the compute fabric, and
// optionally hands those ports to vfio-pci.
package prep

import (
	"fmt"
	"sort"
	"strings"
)

// Outcomes of a classification pass.
const (
	Matched    = "matched"
	Incomplete = "incomplete" // a fact is missing or changed; retried
	Unmatched  = "unmatched"  // the topology is not one the detector knows
)

// Fact statuses.
const (
	FactOK            = "ok"
	FactRefused       = "refused"
	FactPortNotActive = "port-not-active"
	FactUnsupported   = "unsupported" // e.g. a function with several ports
)

// GPUInfo is a GPU and its PCIe root.
type GPUInfo struct {
	BDF  string `json:"bdf"`
	Root string `json:"pcieRoot"`
}

// PortFacts is what a pass read about one InfiniBand function.
type PortFacts struct {
	BDF        string   `json:"bdf"`
	Root       string   `json:"pcieRoot"`
	Driver     string   `json:"driver"`
	IBDev      string   `json:"ibdev,omitempty"`
	VF         bool     `json:"vf,omitempty"`
	NumVFs     int      `json:"numVFs,omitempty"`
	IOMMUGroup string   `json:"iommuGroup"`
	Netdevs    []string `json:"netdevs,omitempty"`
	State      string   `json:"portState,omitempty"`
	Prefix     string   `json:"subnetPrefix,omitempty"`
	SMLid      uint64   `json:"smLID,omitempty"`
	SMGUID     string   `json:"smGUID,omitempty"`
	Status     string   `json:"status"`
	StatusErr  string   `json:"statusError,omitempty"`
	Neighbor   string   `json:"neighbor,omitempty"` // leaf switch, for the report only
}

func (p PortFacts) group() string { return p.Prefix + "/" + p.SMGUID }

// Outcome is the result of classifyHGX.
type Outcome struct {
	Result  string   `json:"result"`
	Reason  string   `json:"reason,omitempty"`
	Compute []string `json:"compute,omitempty"` // BDFs of the compute ports
}

// classifyHGX recognises one topology: a single InfiniBand fabric that gives
// every GPU exactly one port under the GPU's own PCIe root, and has no ports
// anywhere else. Those ports are the compute fabric. Any other shape is left
// unclassified, with the clause that failed.
//
// unprobed lists InfiniBand functions that are not on a host driver and were
// not bound by prep, whose facts cannot be read; any of them makes the pass
// unmatched.
func classifyHGX(gpus []GPUInfo, ports []PortFacts, unprobed []string) Outcome {
	if len(gpus) == 0 {
		return Outcome{Result: Unmatched, Reason: "no GPUs on this node"}
	}
	gpusPerRoot := map[string][]string{}
	for _, g := range gpus {
		if g.Root == "" {
			return Outcome{Result: Incomplete, Reason: "cannot find the PCIe root of GPU " + g.BDF}
		}
		gpusPerRoot[g.Root] = append(gpusPerRoot[g.Root], g.BDF)
	}
	if len(unprobed) > 0 {
		return Outcome{Result: Unmatched, Reason: fmt.Sprintf("InfiniBand functions that prep did not bind are off their host driver and cannot be probed: %s", strings.Join(unprobed, ", "))}
	}
	for _, p := range ports {
		if p.Root == "" {
			return Outcome{Result: Incomplete, Reason: "cannot find the PCIe root of port " + p.BDF}
		}
	}

	// (a) every port has all its facts. A port anywhere with unknown facts
	// might belong to the GPU fabric, so the shape cannot be confirmed.
	for _, p := range ports {
		switch p.Status {
		case FactOK:
		case FactUnsupported:
			return Outcome{Result: Unmatched, Reason: fmt.Sprintf("port %s: %s", p.BDF, p.StatusErr)}
		default:
			return Outcome{Result: Incomplete, Reason: fmt.Sprintf("port %s on %s: %s %s", p.BDF, p.Root, p.Status, p.StatusErr)}
		}
	}

	// Ports per fabric group, and per group per root.
	groups := map[string][]PortFacts{}
	for _, p := range ports {
		if p.Status == FactOK {
			groups[p.group()] = append(groups[p.group()], p)
		}
	}
	rootsWithPort := map[string]bool{}
	for _, p := range ports {
		rootsWithPort[p.Root] = true
	}
	for _, g := range sortedKeys(gpusPerRoot) {
		if !rootsWithPort[g] {
			return Outcome{Result: Unmatched, Reason: fmt.Sprintf("GPU %s has no InfiniBand port under its root %s", gpusPerRoot[g][0], g)}
		}
	}

	var covering []string
	singleRoot := 0
	for _, key := range sortedKeys(groups) {
		perRoot := map[string]int{}
		for _, p := range groups[key] {
			perRoot[p.Root]++
		}
		covered := 0
		for root := range gpusPerRoot {
			if perRoot[root] == 1 {
				covered++
			}
		}
		if covered == 1 {
			singleRoot++
		}
		if covered == len(gpusPerRoot) {
			covering = append(covering, key)
		}
	}
	switch {
	case len(covering) == 0 && singleRoot == len(gpusPerRoot) && len(gpusPerRoot) > 1:
		return Outcome{Result: Unmatched, Reason: fmt.Sprintf("%d fabrics, each covering exactly one GPU root (one subnet per rail)", singleRoot)}
	case len(covering) == 0:
		return Outcome{Result: Unmatched, Reason: "no fabric gives every GPU root exactly one port"}
	case len(covering) > 1:
		return Outcome{Result: Unmatched, Reason: fmt.Sprintf("%d fabrics each give every GPU root exactly one port", len(covering))}
	}

	compute := groups[covering[0]]
	// (c) no port of the compute fabric outside a GPU root.
	for _, p := range compute {
		if _, onGPURoot := gpusPerRoot[p.Root]; !onGPURoot {
			return Outcome{Result: Unmatched, Reason: fmt.Sprintf("port %s of the GPU fabric lies outside every GPU root", p.BDF)}
		}
	}
	// One GPU per root: two GPUs sharing a root would share its port.
	for _, root := range sortedKeys(gpusPerRoot) {
		if n := len(gpusPerRoot[root]); n != 1 {
			return Outcome{Result: Unmatched, Reason: fmt.Sprintf("root %s has %d GPUs and 1 compute-fabric port", root, n)}
		}
	}

	out := Outcome{Result: Matched}
	for _, p := range compute {
		out.Compute = append(out.Compute, p.BDF)
	}
	sort.Strings(out.Compute)
	return out
}

// sameFabricFacts reports whether two passes over the same ports read the
// same fabric identity for each, so a failover during the pass is caught.
func sameFabricFacts(a, b []PortFacts) (bool, string) {
	byBDF := map[string]PortFacts{}
	for _, p := range a {
		byBDF[p.BDF] = p
	}
	for _, q := range b {
		p, ok := byBDF[q.BDF]
		if !ok {
			return false, fmt.Sprintf("port %s appeared during the pass", q.BDF)
		}
		if p.Root != q.Root || p.Prefix != q.Prefix || p.SMLid != q.SMLid || p.SMGUID != q.SMGUID || p.Status != q.Status {
			return false, fmt.Sprintf("fabric changed during the pass on %s", q.BDF)
		}
	}
	if len(a) != len(b) {
		return false, "a port disappeared during the pass"
	}
	return true, ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

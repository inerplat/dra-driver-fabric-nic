package prep

import (
	"strings"
	"testing"
)

func port(bdf, root, sm string) PortFacts {
	return PortFacts{BDF: bdf, Root: root, Status: FactOK, Prefix: "fe80:0000:0000:0000", SMGUID: sm}
}

func gpu(bdf, root string) GPUInfo { return GPUInfo{BDF: bdf, Root: root} }

func TestClassifyHGX(t *testing.T) {
	// Eight GPUs, each with its own root; a compute port on every root and a
	// storage port sharing one root (the H100 layout).
	var gpus []GPUInfo
	var ports []PortFacts
	for i, r := range []string{"15", "37", "48", "59", "97", "b7", "c7", "d7"} {
		gpus = append(gpus, gpu("g"+r, "pci0000:"+r))
		ports = append(ports, port("c"+r, "pci0000:"+r, "0xcompute"))
		_ = i
	}
	h100 := append(append([]PortFacts(nil), ports...), port("s97", "pci0000:97", "0xstorage"))
	b200 := append(append([]PortFacts(nil), ports...), port("s4b", "pci0000:4b", "0xstorage"), port("sc8", "pci0000:c8", "0xstorage"))

	for name, tc := range map[string]struct {
		gpus     []GPUInfo
		ports    []PortFacts
		unprobed []string
		result   string
		reason   string
	}{
		"h100 storage on a GPU root":     {gpus: gpus, ports: h100, result: Matched},
		"b200 storage on its own roots":  {gpus: gpus, ports: b200, result: Matched},
		"port down under a GPU root":     {gpus: gpus, ports: append(append([]PortFacts(nil), ports[1:]...), PortFacts{BDF: "c15", Root: "pci0000:15", Status: FactPortNotActive}), result: Incomplete},
		"SM refused under a GPU root":    {gpus: gpus, ports: append(append([]PortFacts(nil), ports[1:]...), PortFacts{BDF: "c15", Root: "pci0000:15", Status: FactRefused}), result: Incomplete},
		"GPU without a port":             {gpus: gpus, ports: ports[1:], result: Unmatched, reason: "has no InfiniBand port"},
		"one subnet per rail":            {gpus: gpus[:3], ports: []PortFacts{port("c15", "pci0000:15", "0xa"), port("c37", "pci0000:37", "0xb"), port("c48", "pci0000:48", "0xc")}, result: Unmatched, reason: "one subnet per rail"},
		"two GPUs on one root":           {gpus: []GPUInfo{gpu("g1", "pci0000:15"), gpu("g2", "pci0000:15"), gpu("g3", "pci0000:37")}, ports: []PortFacts{port("c15", "pci0000:15", "0xa"), port("c37", "pci0000:37", "0xa")}, result: Unmatched, reason: "has 2 GPUs"},
		"compute port off the GPU roots": {gpus: gpus[:2], ports: []PortFacts{port("c15", "pci0000:15", "0xa"), port("c37", "pci0000:37", "0xa"), port("x", "pci0000:99", "0xa")}, result: Unmatched, reason: "outside every GPU root"},
		"already on vfio":                {gpus: gpus, ports: h100, unprobed: []string{"c15"}, result: Unmatched, reason: "cannot be probed"},
		"no GPUs":                        {ports: h100, result: Unmatched},
	} {
		t.Run(name, func(t *testing.T) {
			got := classifyHGX(tc.gpus, tc.ports, tc.unprobed)
			if got.Result != tc.result || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("got %+v, want %s containing %q", got, tc.result, tc.reason)
			}
			if got.Result == Matched {
				if len(got.Compute) != 8 {
					t.Errorf("compute %v", got.Compute)
				}
				for _, b := range got.Compute {
					if strings.HasPrefix(b, "s") {
						t.Errorf("storage port %s classified compute", b)
					}
				}
			}
		})
	}
}

func TestSameFabricFacts(t *testing.T) {
	a := []PortFacts{port("c15", "pci0000:15", "0xa")}
	if ok, _ := sameFabricFacts(a, a); !ok {
		t.Errorf("identical passes differ")
	}
	b := []PortFacts{port("c15", "pci0000:15", "0xb")}
	if ok, _ := sameFabricFacts(a, b); ok {
		t.Errorf("SM failover not detected")
	}
}

func TestParseSMInfo(t *testing.T) {
	guid, err := parseSMInfo("sminfo: sm lid 1 sm guid 0xA088C20300EEAE30, activity count 52 priority 15 state 3 SMINFO_MASTER\n")
	if err != nil || guid != "0xa088c20300eeae30" {
		t.Errorf("master: %q %v", guid, err)
	}
	for name, out := range map[string]string{
		"standby":  "sminfo: sm lid 2 sm guid 0x1, activity count 1 priority 14 state 2 SMINFO_STANDBY",
		"no guid":  "sminfo: query failed",
		"not hex":  "sminfo: sm lid 1 sm guid 0xzz, activity count 1 priority 15 state 3 SMINFO_MASTER",
		"no state": "sminfo: sm lid 1 sm guid 0x1",
	} {
		if _, err := parseSMInfo(out); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

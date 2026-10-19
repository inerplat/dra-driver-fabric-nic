package inventory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"

	"github.com/inerplat/dra-driver-fabric-nic/internal/sysfstest"
	"github.com/inerplat/dra-driver-fabric-nic/pkg/sysfs"
)

const driver = "fabric-nic.example.com"

func TestBuild(t *testing.T) {
	root := t.TempDir()
	if err := sysfstest.BuildTestTree(root, []sysfstest.TestFunc{
		{BDF: "0000:9c:00.0", Root: "pci0000:97", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "vfio-pci", NUMA: 1, Group: "60"},
		{BDF: "0000:9d:00.1", Root: "pci0000:97", Class: "0x020700", Vendor: "0x15b3", Device: "0x101c", Driver: "vfio-pci", PhysFn: "0000:9d:00.0", NUMA: -1, Group: "62"},
	}); err != nil {
		t.Fatal(err)
	}
	funcs, _, err := sysfs.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	role := "compute"
	extra := map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"0000:9c:00.0": {"example.com/fabric": {StringValue: &role}},
	}
	devices := Build(funcs, Merged{Attrs: extra, Source: map[string]string{"0000:9c:00.0": SourceFile}}, deviceattribute.WithFSFromRoot(root))
	if len(devices) != 2 {
		t.Fatalf("got %d devices", len(devices))
	}

	pf := devices[0].Slice
	if pf.Name != "0000-9c-00-0" {
		t.Errorf("name %q", pf.Name)
	}
	want := map[resourceapi.QualifiedName]string{
		AttrFunction: "pf",
		AttrLinkType: "infiniband",
		AttrVendor:   "15b3",
		deviceattribute.StandardDeviceAttributePCIBusID: "0000:9c:00.0",
		deviceattribute.StandardDeviceAttributePCIeRoot: "pci0000:97",
		"example.com/fabric":                            "compute",
	}
	for name, v := range want {
		if a, ok := pf.Attributes[name]; !ok || a.StringValue == nil || *a.StringValue != v {
			t.Errorf("attribute %s = %+v, want %q", name, a, v)
		}
	}
	if a := pf.Attributes[AttrNUMANode]; a.IntValue == nil || *a.IntValue != 1 {
		t.Errorf("numaNode = %+v", a)
	}
	if _, ok := pf.Attributes[AttrParentPF]; ok {
		t.Errorf("PF has parentPF")
	}

	vf := devices[1].Slice
	if a := vf.Attributes[AttrParentPF]; a.StringValue == nil || *a.StringValue != "0000:9d:00.0" {
		t.Errorf("parentPF = %+v", a)
	}
	if _, ok := vf.Attributes[AttrNUMANode]; ok {
		t.Errorf("numaNode published for a function without NUMA affinity")
	}
	if _, ok := vf.Attributes["example.com/fabric"]; ok {
		t.Errorf("attribute of another device leaked")
	}
	if a := vf.Attributes[AttrRoleSource]; a.StringValue == nil || *a.StringValue != SourceNone {
		t.Errorf("roleSource of a device no file names = %+v", a)
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "attrs.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExtra(t *testing.T) {
	reserved := Reserved(driver)

	got, err := LoadExtra(writeFile(t, `{"version":"v1","bootID":"b1","devices":{"0000:9c:00.0":{"example.com/fabric":{"string":"compute"}}}}`), "b1", reserved)
	if err != nil {
		t.Fatal(err)
	}
	if v := got["0000:9c:00.0"]["example.com/fabric"].StringValue; v == nil || *v != "compute" {
		t.Errorf("got %+v", got)
	}

	if got, err := LoadExtra(filepath.Join(t.TempDir(), "missing.json"), "b1", reserved); err != nil || got != nil {
		t.Errorf("missing file: %v %v", got, err)
	}

	if _, err := LoadExtra(writeFile(t, `{"version":"v1","bootID":"old","devices":{}}`), "b1", reserved); !errors.Is(err, ErrStale) {
		t.Errorf("stale file: err = %v", err)
	}

	for name, body := range map[string]string{
		"version":         `{"version":"v2","devices":{}}`,
		"no domain":       `{"version":"v1","devices":{"0000:9c:00.0":{"fabric":{"string":"x"}}}}`,
		"driver domain":   `{"version":"v1","devices":{"0000:9c:00.0":{"fabric-nic.example.com/function":{"string":"x"}}}}`,
		"standard":        `{"version":"v1","devices":{"0000:9c:00.0":{"resource.kubernetes.io/pcieRoot":{"string":"x"}}}}`,
		"two values":      `{"version":"v1","devices":{"0000:9c:00.0":{"example.com/a":{"string":"x","int":1}}}}`,
		"no value":        `{"version":"v1","devices":{"0000:9c:00.0":{"example.com/a":{}}}}`,
		"bad name":        `{"version":"v1","devices":{"0000:9c:00.0":{"example.com/has-dash":{"string":"x"}}}}`,
		"bad domain":      `{"version":"v1","devices":{"0000:9c:00.0":{"Example_Com/a":{"string":"x"}}}}`,
		"long string":     `{"version":"v1","devices":{"0000:9c:00.0":{"example.com/a":{"string":"` + strings.Repeat("x", 65) + `"}}}}`,
		"bad version":     `{"version":"v1","devices":{"0000:9c:00.0":{"example.com/a":{"version":"1.2"}}}}`,
		"zero prerelease": `{"version":"v1","devices":{"0000:9c:00.0":{"example.com/a":{"version":"1.2.3-01"}}}}`,
	} {
		if _, err := LoadExtra(writeFile(t, body), "b1", reserved); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadAll(t *testing.T) {
	reserved := Reserved(driver)
	admin := writeFile(t, `{"version":"v1","devices":{"0000:9c:00.0":{"example.com/fabric":{"string":"host"}}}}`)
	generated := writeFile(t, `{"version":"v1","bootID":"b1","devices":{
		"0000:9c:00.0":{"example.com/fabric":{"string":"compute"},"example.com/extra":{"string":"x"}},
		"0000:bc:00.0":{"example.com/fabric":{"string":"compute"}}}}`)

	m := LoadAll([]string{admin, generated}, "b1", reserved)
	if v := m.Attrs["0000:9c:00.0"]["example.com/fabric"].StringValue; v == nil || *v != "host" || m.Source["0000:9c:00.0"] != SourceFile {
		t.Errorf("admin entry did not win: %+v %v", m.Attrs["0000:9c:00.0"], m.Source)
	}
	if _, ok := m.Attrs["0000:9c:00.0"]["example.com/extra"]; ok {
		t.Errorf("generated attribute merged into a device the admin file names")
	}
	if m.Source["0000:bc:00.0"] != SourceAuto {
		t.Errorf("generated entry source = %q", m.Source["0000:bc:00.0"])
	}

	noBoot := writeFile(t, `{"version":"v1","devices":{"0000:bc:00.0":{"example.com/fabric":{"string":"compute"}}}}`)
	m = LoadAll([]string{filepath.Join(t.TempDir(), "absent.json"), noBoot}, "b1", reserved)
	if m.Files[0].State != "missing" || m.Files[1].State != "invalid" || len(m.Attrs) != 0 {
		t.Errorf("states %+v attrs %v", m.Files, m.Attrs)
	}

	stale := writeFile(t, `{"version":"v1","bootID":"old","devices":{"0000:bc:00.0":{"example.com/fabric":{"string":"compute"}}}}`)
	m = LoadAll([]string{admin, stale}, "b1", reserved)
	if m.Files[1].State != "stale" || m.Source["0000:bc:00.0"] != "" {
		t.Errorf("stale generated file used: %+v", m)
	}
}

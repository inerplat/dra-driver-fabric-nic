package prep

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inerplat/dra-driver-fabric-nic/internal/sysfstest"
	"github.com/inerplat/dra-driver-fabric-nic/pkg/inventory"
)

const (
	boot   = "b1"
	prefix = "fabric-nic.example.com"
)

type fakeProber map[string]string // ibdev -> SM GUID; missing means the query fails

func (f fakeProber) SMGUID(_ context.Context, ibdev string, _ int) (string, error) {
	if g, ok := f[ibdev]; ok {
		return g, nil
	}
	return "", errors.New("sminfo: failed: query")
}
func (f fakeProber) Neighbor(context.Context, string, int) (string, error) { return "leaf", nil }

type fakeHostUse map[string]string // BDF -> reason

func (f fakeHostUse) InUse(_ context.Context, p PortFacts) (bool, string) {
	why, ok := f[p.BDF]
	return ok, why
}

// treeBinder moves a function of the fake tree to vfio-pci.
type treeBinder struct {
	root  string
	bound []string
}

func (b *treeBinder) BindVFIO(bdf string, verify func() error) error {
	if err := verify(); err != nil {
		return err
	}
	real, _ := filepath.EvalSymlinks(filepath.Join(b.root, "bus", "pci", "devices", bdf))
	os.Remove(filepath.Join(real, "driver"))
	os.RemoveAll(filepath.Join(real, "infiniband"))
	b.bound = append(b.bound, bdf)
	return os.Symlink("../../../bus/pci/drivers/vfio-pci", filepath.Join(real, "driver"))
}

func ib(bdf, root, ibdev, gidLow, serial, vendor string) sysfstest.TestFunc {
	return sysfstest.TestFunc{BDF: bdf, Root: root, Class: "0x020700", Vendor: "0x15b3", Device: "0x1021", Driver: "mlx5_core", Group: "n" + bdf,
		IBDev: ibdev, PortState: "4: ACTIVE", SMLid: "0x1", GID0: "fe80:0000:0000:0000:0000:0000:0000:" + gidLow, VPD: sysfstest.VPD(serial, vendor)}
}

// node lays out two GPUs, each with a compute port on its root, a storage
// port with VFs on a GPU-less root, and a two-function NVLink management
// bridge whose second function does not answer the subnet manager.
func node(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	funcs := []sysfstest.TestFunc{
		{BDF: "0000:15:00.0", Root: "pci0000:15", Class: "0x030200", Vendor: "0x10de", Device: "0x2901", Driver: "vfio-pci", Group: "g15"},
		{BDF: "0000:37:00.0", Root: "pci0000:37", Class: "0x030200", Vendor: "0x10de", Device: "0x2901", Driver: "vfio-pci", Group: "g37"},
		ib("0000:15:01.0", "pci0000:15", "mlx5_0", "0001", "CMP0", ""),
		ib("0000:37:01.0", "pci0000:37", "mlx5_1", "0002", "CMP1", ""),
		ib("0000:4e:00.0", "pci0000:4b", "mlx5_2", "0003", "STOR", ""),
		ib("0000:ab:00.0", "pci0000:a6", "mlx5_6", "0006", "NVL", "SMDL=SW_MNG"),
		ib("0000:ab:00.1", "pci0000:a6", "mlx5_7", "0007", "NVL", "SMDL=SW_MNG"),
		{BDF: "0000:03:00.0", Root: "pci0000:00", Class: "0x030000", Vendor: "0x1a03", Device: "0x2000", Driver: "ast"},
	}
	funcs[4].NumVFs = 2
	if err := sysfstest.BuildTestTree(root, funcs); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bus", "pci", "drivers", "vfio-pci"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func newPrep(t *testing.T, root string, hostUse fakeHostUse) (*Prep, *treeBinder) {
	t.Helper()
	b := &treeBinder{root: root}
	return &Prep{
		Config: Config{
			DriverName: prefix, SysRoot: root, StateDir: t.TempDir(), GPUVendors: []string{"10de"},
			Uptime: time.Hour, Deadline: 10 * time.Minute, VPDTimeout: time.Second,
			Now: func() time.Time { return time.Unix(0, 0) },
		},
		Prober:  fakeProber{"mlx5_0": "0xcompute", "mlx5_1": "0xcompute", "mlx5_2": "0xstorage", "mlx5_6": "0xnvlsm"},
		HostUse: hostUse,
		Binder:  b,
	}, b
}

func intent(mode string, annotations map[string]string) Intent {
	in := ParseIntent(prefix, map[string]string{prefix + "/mode": mode}, annotations, ModeReport)
	in.Node, in.BootID = "n1", boot
	return in
}

func portOf(rep Report, bdf string) PortResult {
	for _, p := range rep.Ports {
		if p.BDF == bdf {
			return p
		}
	}
	return PortResult{}
}

// published loads the plugin's attribute files the way the plugin does.
func published(t *testing.T, p *Prep) inventory.Merged {
	t.Helper()
	return inventory.LoadAll([]string{filepath.Join(p.StateDir, DeclaredFile), filepath.Join(p.StateDir, GeneratedFile)}, boot, inventory.Reserved(prefix))
}

func TestReportBindsNothing(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	rep, code := p.Apply(context.Background(), intent(ModeReport, nil))
	if code != 0 || len(b.bound) != 0 {
		t.Fatalf("code %d, bound %v", code, b.bound)
	}
	if rep.Auto == nil || rep.Auto.Result != Matched || strings.Join(rep.Auto.Compute, ",") != "CMP0-f0,CMP1-f0" {
		t.Fatalf("detection %+v", rep.Auto)
	}
	if r := portOf(rep, "0000:15:01.0"); r.Result != ResultPreview || r.Role != "compute" {
		t.Errorf("compute port %+v", r)
	}
	if m := published(t, p); len(m.Attrs) != 0 {
		t.Errorf("report mode published roles %v", m.Attrs)
	}
}

func TestAutoBindsComputeAndSkipsNVLinkBridge(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, nil)
	rep, code := p.Apply(context.Background(), intent(ModeAuto, nil))
	if code != 0 || strings.Join(b.bound, ",") != "0000:15:01.0,0000:37:01.0" {
		t.Fatalf("code %d, bound %v, fatal %q, auto %+v", code, b.bound, rep.Fatal, rep.Auto)
	}
	if r := portOf(rep, "0000:ab:00.1"); r.Result != ResultUndeclared || !r.NVLinkManagement {
		t.Errorf("NVLink bridge %+v", r)
	}
	m := published(t, p)
	if m.Source["0000:15:01.0"] != inventory.SourceAuto || len(m.Attrs) != 2 {
		t.Errorf("published %v %v", m.Attrs, m.Source)
	}

	// The pod restarts in the same boot: the ports are on vfio-pci now and
	// cannot be probed, so the match is reused.
	b.bound = nil
	rep, code = p.Apply(context.Background(), intent(ModeAuto, nil))
	if code != 0 || len(b.bound) != 0 || portOf(rep, "0000:15:01.0").Result != ResultOnVFIO {
		t.Fatalf("restart: code %d, bound %v, %+v", code, b.bound, portOf(rep, "0000:15:01.0"))
	}
	if m := published(t, p); len(m.Attrs) != 2 {
		t.Errorf("restart published %v", m.Attrs)
	}

	// A new boot detects again; the vfio-pci ports prep did not bind in this
	// boot cannot be probed, so nothing is decided.
	in := intent(ModeAuto, nil)
	in.BootID = "b2"
	rep, _ = p.Apply(context.Background(), in)
	if rep.Auto.Result != Unmatched {
		t.Errorf("new boot %+v", rep.Auto)
	}
}

func TestAutoIncompleteRetriesOnlyNearBoot(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	delete(p.Prober.(fakeProber), "mlx5_1")
	p.Uptime = time.Minute
	rep, code := p.Apply(context.Background(), intent(ModeAuto, nil))
	if code != 1 || rep.Auto.Result != Incomplete || len(b.bound) != 0 {
		t.Fatalf("near boot: code %d, %+v, bound %v", code, rep.Auto, b.bound)
	}
	p.Uptime = time.Hour
	if _, code := p.Apply(context.Background(), intent(ModeAuto, nil)); code != 0 {
		t.Errorf("after the deadline: code %d", code)
	}
}

func TestExplicit(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	rep, code := p.Apply(context.Background(), intent(ModeExplicit, map[string]string{
		prefix + "/port.CMP0-f0": "compute/vfio-pci",
		prefix + "/port.STOR-f0": "storage/vfio-pci",
		prefix + "/port.NVL-f0":  "other/vfio-pci",
		prefix + "/port.GONE-f0": "compute/vfio-pci",
	}))
	if code != 0 || rep.Auto != nil {
		t.Fatalf("code %d, auto %+v", code, rep.Auto)
	}
	if strings.Join(b.bound, ",") != "0000:15:01.0" {
		t.Errorf("bound %v", b.bound)
	}
	if r := portOf(rep, "0000:4e:00.0"); r.Result != ResultRefused || !strings.Contains(r.Reason, "VFs") {
		t.Errorf("storage port with VFs %+v", r)
	}
	if r := portOf(rep, "0000:ab:00.0"); r.Result != ResultRefused || !strings.Contains(r.Reason, "NVLink") {
		t.Errorf("NVLink bridge %+v", r)
	}
	if r := portOf(rep, "0000:37:01.0"); r.Result != ResultUndeclared {
		t.Errorf("undeclared port %+v", r)
	}
	if strings.Join(rep.Stale, ",") != "GONE-f0" {
		t.Errorf("stale %v", rep.Stale)
	}
	// Declared roles are published even when the port could not be bound;
	// the bridge may only be other.
	m := published(t, p)
	if m.Source["0000:15:01.0"] != inventory.SourceFile || m.Source["0000:4e:00.0"] != inventory.SourceFile || len(m.Attrs) != 3 {
		t.Errorf("published %v %v", m.Attrs, m.Source)
	}
}

func TestExplicitComputeNeedsAGPURoot(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	rep, _ := p.Apply(context.Background(), intent(ModeExplicit, map[string]string{prefix + "/port.NVL-f1": "compute/vfio-pci"}))
	if len(b.bound) != 0 || portOf(rep, "0000:ab:00.1").Result != ResultRefused {
		t.Errorf("bound %v, %+v", b.bound, portOf(rep, "0000:ab:00.1"))
	}
	if m := published(t, p); len(m.Attrs) != 0 {
		t.Errorf("a role the hardware contradicts was published: %v", m.Attrs)
	}
}

func TestBindRecordNeedsTheSameCard(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, nil)
	// A record from this boot for the slot, but of another card.
	if err := saveBinds(p.StateDir, boot, []BindRecord{{BDF: "0000:15:01.0", ID: "OLD-f0"}}); err != nil {
		t.Fatal(err)
	}
	rep, _ := p.Apply(context.Background(), intent(ModeExplicit, map[string]string{prefix + "/port.CMP0-f0": "compute/vfio-pci"}))
	if strings.Join(b.bound, ",") != "0000:15:01.0" {
		t.Errorf("bound %v, %+v", b.bound, portOf(rep, "0000:15:01.0"))
	}
}

func TestPortOnAnotherDriverStopsDetection(t *testing.T) {
	root := node(t)
	real, _ := filepath.EvalSymlinks(filepath.Join(root, "bus", "pci", "devices", "0000:37:01.0"))
	os.Remove(filepath.Join(real, "driver"))
	os.RemoveAll(filepath.Join(real, "infiniband"))
	os.Symlink("../../../bus/pci/drivers/pci-stub", filepath.Join(real, "driver"))
	p, b := newPrep(t, root, nil)
	rep, _ := p.Apply(context.Background(), intent(ModeAuto, nil))
	if rep.Auto.Result == Matched || len(b.bound) != 0 {
		t.Errorf("%+v, bound %v", rep.Auto, b.bound)
	}
}

func TestInvalidDeclarationsFailClosed(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	rep, code := p.Apply(context.Background(), intent(ModeAuto, map[string]string{
		prefix + "/port.CMP0-f0": "compute",
	}))
	if code != 0 || len(b.bound) != 0 || rep.Fatal == "" {
		t.Fatalf("code %d, bound %v, fatal %q", code, b.bound, rep.Fatal)
	}
	if m := published(t, p); len(m.Attrs) != 0 {
		t.Errorf("published %v", m.Attrs)
	}
}

func TestBadModeFailsClosed(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	rep, _ := p.Apply(context.Background(), intent("bind", nil))
	if len(b.bound) != 0 || !strings.Contains(rep.Fatal, "mode") {
		t.Errorf("bound %v, fatal %q", b.bound, rep.Fatal)
	}
}

func TestHostUseAndReturnedPorts(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, fakeHostUse{"0000:37:01.0": "has address 10.0.0.1/24"})
	rep, _ := p.Apply(context.Background(), intent(ModeAuto, nil))
	if strings.Join(b.bound, ",") != "0000:15:01.0" || !strings.Contains(portOf(rep, "0000:37:01.0").Reason, "10.0.0.1") {
		t.Fatalf("bound %v, %+v", b.bound, portOf(rep, "0000:37:01.0"))
	}
	// An operator moves the bound port back to its host driver.
	real, _ := filepath.EvalSymlinks(filepath.Join(root, "bus", "pci", "devices", "0000:15:01.0"))
	os.Remove(filepath.Join(real, "driver"))
	os.Symlink("../../../bus/pci/drivers/mlx5_core", filepath.Join(real, "driver"))
	b.bound = nil
	rep, _ = p.Apply(context.Background(), intent(ModeAuto, nil))
	if len(b.bound) != 0 || portOf(rep, "0000:15:01.0").Result != ResultReturned {
		t.Errorf("bound %v, %+v", b.bound, portOf(rep, "0000:15:01.0"))
	}
}

func TestUnprobedPortStopsDetection(t *testing.T) {
	root := node(t)
	real, _ := filepath.EvalSymlinks(filepath.Join(root, "bus", "pci", "devices", "0000:37:01.0"))
	os.Remove(filepath.Join(real, "driver"))
	os.Symlink("../../../bus/pci/drivers/vfio-pci", filepath.Join(real, "driver"))
	p, b := newPrep(t, root, nil)
	rep, _ := p.Apply(context.Background(), intent(ModeAuto, nil))
	if rep.Auto.Result != Unmatched || len(b.bound) != 0 {
		t.Errorf("%+v, bound %v", rep.Auto, b.bound)
	}
}

func TestOffWritesEmptyFiles(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	p.Apply(context.Background(), intent(ModeAuto, nil))
	b.bound = nil
	p.Apply(context.Background(), intent(ModeOff, nil))
	if m := published(t, p); len(m.Attrs) != 0 || len(b.bound) != 0 {
		t.Errorf("published %v, bound %v", m.Attrs, b.bound)
	}
}

func TestParseIntent(t *testing.T) {
	in := ParseIntent(prefix, map[string]string{prefix + "/enabled": "true", prefix + "/prep": "bind"}, map[string]string{
		prefix + "/port.MT2530J01EYY-f0": "compute/vfio-pci",
		prefix + "/port.bad key":         "compute/vfio-pci",
		prefix + "/redetect":             "1",
		"other.example.com/x":            "y",
	}, ModeReport)
	if in.Mode != ModeReport || in.ModeErr != "" {
		t.Errorf("mode %q %q", in.Mode, in.ModeErr)
	}
	if d := in.Ports["MT2530J01EYY-f0"]; d.Role != "compute" || d.Bind != BindVFIO {
		t.Errorf("ports %v", in.Ports)
	}
	if len(in.Invalid) != 1 || len(in.Unknown) != 2 {
		t.Errorf("invalid %v unknown %v", in.Invalid, in.Unknown)
	}
}

func TestDeclaredBindHidesNoFabric(t *testing.T) {
	p, b := newPrep(t, node(t), nil)
	p.Apply(context.Background(), intent(ModeExplicit, map[string]string{prefix + "/port.CMP0-f0": "compute/vfio-pci"}))
	// The same boot switches to auto: the port bound for the declaration
	// cannot be probed, so its fabric is unknown.
	b.bound = nil
	rep, _ := p.Apply(context.Background(), intent(ModeAuto, nil))
	if rep.Auto.Result != Unmatched || len(b.bound) != 0 {
		t.Errorf("%+v, bound %v", rep.Auto, b.bound)
	}
}

func TestCachedMatchNeedsTheSameLayout(t *testing.T) {
	root := node(t)
	p, _ := newPrep(t, root, nil)
	p.Apply(context.Background(), intent(ModeReport, nil))
	// A card is replaced in the same boot.
	real, _ := filepath.EvalSymlinks(filepath.Join(root, "bus", "pci", "devices", "0000:37:01.0"))
	os.WriteFile(filepath.Join(real, "vpd"), sysfstest.VPD("NEW1", ""), 0o644)
	rep, _ := p.Apply(context.Background(), intent(ModeAuto, nil))
	if rep.Auto.Reason == "classified earlier in this boot" || strings.Contains(strings.Join(rep.Auto.Compute, ","), "CMP1-f0") {
		t.Errorf("stale match reused: %+v", rep.Auto)
	}
}

// swapHostUse replaces the card in a slot during the last host-use check.
type swapHostUse struct {
	root, bdf string
	calls     *int
}

func (s swapHostUse) InUse(_ context.Context, p PortFacts) (bool, string) {
	if p.BDF == s.bdf {
		if *s.calls++; *s.calls < 2 {
			return false, ""
		}
		real, _ := filepath.EvalSymlinks(filepath.Join(s.root, "bus", "pci", "devices", s.bdf))
		os.WriteFile(filepath.Join(real, "vpd"), sysfstest.VPD("OTHER", ""), 0o644)
	}
	return false, ""
}

func TestCardSwappedBeforeBind(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, nil)
	p.HostUse = swapHostUse{root, "0000:15:01.0", new(int)}
	rep, _ := p.Apply(context.Background(), intent(ModeExplicit, map[string]string{prefix + "/port.CMP0-f0": "compute/vfio-pci"}))
	if len(b.bound) != 0 || portOf(rep, "0000:15:01.0").Result != ResultRefused {
		t.Errorf("bound %v, %+v", b.bound, portOf(rep, "0000:15:01.0"))
	}
}

func TestPortWithoutVisibleDeviceIsNotBound(t *testing.T) {
	root := node(t)
	real, _ := filepath.EvalSymlinks(filepath.Join(root, "bus", "pci", "devices", "0000:15:01.0"))
	os.RemoveAll(filepath.Join(real, "infiniband"))
	p, b := newPrep(t, root, nil)
	rep, _ := p.Apply(context.Background(), intent(ModeExplicit, map[string]string{prefix + "/port.CMP0-f0": "compute/vfio-pci"}))
	if len(b.bound) != 0 || !strings.Contains(portOf(rep, "0000:15:01.0").Reason, "RDMA device") {
		t.Errorf("bound %v, %+v", b.bound, portOf(rep, "0000:15:01.0"))
	}
}

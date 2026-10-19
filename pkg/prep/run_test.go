package prep

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/inerplat/dra-driver-fabric-nic/internal/sysfstest"
	"github.com/inerplat/dra-driver-fabric-nic/pkg/inventory"
)

type fakeProber map[string]string // ibdev -> SM GUID

func (f fakeProber) SMGUID(_ context.Context, ibdev string, _ int) (string, error) {
	return f[ibdev], nil
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

func (b *treeBinder) BindVFIO(bdf string) error {
	dev := filepath.Join(b.root, "bus", "pci", "devices", bdf)
	real, _ := filepath.EvalSymlinks(dev)
	os.Remove(filepath.Join(real, "driver"))
	os.RemoveAll(filepath.Join(real, "infiniband"))
	b.bound = append(b.bound, bdf)
	return os.Symlink("../../../bus/pci/drivers/vfio-pci", filepath.Join(real, "driver"))
}

const boot = "b1"

func node(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	var funcs []sysfstest.TestFunc
	for i, r := range []string{"15", "37"} {
		funcs = append(funcs,
			sysfstest.TestFunc{BDF: "0000:" + r + ":00.0", Root: "pci0000:" + r, Class: "0x030200", Vendor: "0x10de", Device: "0x2330", Driver: "vfio-pci", Group: "g" + r},
			sysfstest.TestFunc{BDF: "0000:" + r + ":01.0", Root: "pci0000:" + r, Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "mlx5_core", Group: "n" + r,
				IBDev: "mlx5_" + string(rune('0'+i)), PortState: "4: ACTIVE", SMLid: "0x1", GID0: "fe80:0000:0000:0000:0000:0000:0000:0001"},
		)
	}
	funcs = append(funcs,
		// Storage port on the first GPU's root, with VFs.
		sysfstest.TestFunc{BDF: "0000:15:02.0", Root: "pci0000:15", Class: "0x020700", Vendor: "0x15b3", Device: "0x101b", Driver: "mlx5_core", Group: "s15", NumVFs: 2,
			IBDev: "mlx5_9", PortState: "4: ACTIVE", SMLid: "0x9", GID0: "fe80:0000:0000:0000:0000:0000:0000:0009"},
		// A BMC VGA.
		sysfstest.TestFunc{BDF: "0000:03:00.0", Root: "pci0000:00", Class: "0x030000", Vendor: "0x1a03", Device: "0x2000", Driver: "ast"},
	)
	if err := sysfstest.BuildTestTree(root, funcs); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bus", "pci", "drivers", "vfio-pci"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func newPrep(t *testing.T, root, level string, hostUse fakeHostUse) (*Prep, *treeBinder) {
	t.Helper()
	b := &treeBinder{root: root}
	state := t.TempDir()
	return &Prep{
		Config: Config{
			DriverName: "fabric-nic.example.com", Version: "test", SysRoot: root, StateDir: state,
			AdminFile: filepath.Join(state, "attributes.json"), GPUVendors: []string{"10de"}, BootID: boot,
			RetryMin: time.Second, RetryMax: time.Minute,
		},
		Prober:  fakeProber{"mlx5_0": "0xcompute", "mlx5_1": "0xcompute", "mlx5_9": "0xstorage"},
		HostUse: hostUse,
		Binder:  b,
		Level:   func(context.Context) (string, error) { return level, nil },
		Now:     func() time.Time { return time.Unix(0, 0) },
	}, b
}

func readReport(t *testing.T, p *Prep) Report {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.StateDir, ReportFile))
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReportLevelBindsNothing(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, LevelReport, nil)
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := readReport(t, p)
	if r.Outcome.Result != Matched || strings.Join(r.Outcome.Compute, ",") != "0000:15:01.0,0000:37:01.0" {
		t.Fatalf("outcome %+v", r.Outcome)
	}
	if len(r.ExcludedGPUs) != 1 {
		t.Errorf("BMC VGA not reported: %v", r.ExcludedGPUs)
	}
	if len(b.bound) != 0 {
		t.Errorf("report level bound %v", b.bound)
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, ProposedFile)); err != nil {
		t.Errorf("no proposed file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, GeneratedFile)); err == nil {
		t.Errorf("report level wrote the generated file")
	}
}

func TestBindLevel(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, LevelBind, fakeHostUse{"0000:37:01.0": "has address 10.0.0.1"})
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(b.bound, ",") != "0000:15:01.0" {
		t.Fatalf("bound %v", b.bound)
	}
	m := inventory.LoadAll([]string{p.AdminFile, filepath.Join(p.StateDir, GeneratedFile)}, boot, inventory.Reserved(p.DriverName))
	if m.Source["0000:15:01.0"] != inventory.SourceAuto || m.Source["0000:15:02.0"] != "" {
		t.Errorf("generated file %+v", m)
	}
	r := readReport(t, p)
	if len(r.Binds) != 1 || !strings.Contains(strings.Join(r.BindNotes, "\n"), "host uses it") {
		t.Errorf("report binds %+v notes %v", r.Binds, r.BindNotes)
	}

	// A later sweep keeps the frozen classification: it must not probe.
	p.HostUse = fakeHostUse{}
	p.Prober = failingProber{t}
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(b.bound, ",") != "0000:15:01.0,0000:37:01.0" {
		t.Errorf("after the host released 37:01.0, bound %v", b.bound)
	}
	if r := readReport(t, p); !r.Frozen || r.Outcome.Result != Matched {
		t.Errorf("classification not kept: %+v", r.Outcome)
	}
}

type failingProber struct{ t *testing.T }

func (f failingProber) SMGUID(context.Context, string, int) (string, error) {
	f.t.Errorf("probed after the classification was frozen")
	return "", os.ErrInvalid
}
func (f failingProber) Neighbor(context.Context, string, int) (string, error) { return "", nil }

func TestBindRespectsAdminAndOperator(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, LevelBind, fakeHostUse{})
	admin := `{"version":"v1","devices":{"0000:37:01.0":{"example.com/fabric":{"string":"host"}}}}`
	if err := os.WriteFile(p.AdminFile, []byte(admin), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(b.bound, ",") != "0000:15:01.0" {
		t.Fatalf("bound %v", b.bound)
	}
	r := readReport(t, p)
	if len(r.Conflicts) != 1 {
		t.Errorf("conflicts %v", r.Conflicts)
	}

	// An operator returns 15:01.0 to the host driver: prep leaves it there.
	dev, _ := filepath.EvalSymlinks(filepath.Join(root, "bus", "pci", "devices", "0000:15:01.0"))
	os.Remove(filepath.Join(dev, "driver"))
	os.Symlink("../../../bus/pci/drivers/mlx5_core", filepath.Join(dev, "driver"))
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.bound) != 1 {
		t.Errorf("prep bound a port the operator returned: %v", b.bound)
	}
	if !strings.Contains(strings.Join(readReport(t, p).BindNotes, "\n"), "returned-by-operator") {
		t.Errorf("notes %v", readReport(t, p).BindNotes)
	}
}

func TestIncompleteRetries(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, LevelBind, fakeHostUse{})
	p.Prober = fakeProber{"mlx5_1": "0xcompute", "mlx5_9": "0xstorage"} // mlx5_0 refuses
	refusing := p.Prober.(fakeProber)
	p.Prober = refusingProber{fakeProber: refusing, refuse: "mlx5_0"}
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := readReport(t, p); r.Outcome.Result != Incomplete || r.Retries != 1 {
		t.Fatalf("outcome %+v retries %d", r.Outcome, r.Retries)
	}
	if len(b.bound) != 0 {
		t.Errorf("bound on an incomplete pass: %v", b.bound)
	}
	// Once the port answers and the backoff has passed, the pass matches.
	p.Prober = fakeProber{"mlx5_0": "0xcompute", "mlx5_1": "0xcompute", "mlx5_9": "0xstorage"}
	p.Now = func() time.Time { return time.Unix(3600, 0) }
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readReport(t, p).Outcome.Result != Matched || len(b.bound) != 2 {
		t.Errorf("after recovery: %+v bound %v", readReport(t, p).Outcome, b.bound)
	}
}

type refusingProber struct {
	fakeProber
	refuse string
}

func (r refusingProber) SMGUID(ctx context.Context, ibdev string, port int) (string, error) {
	if ibdev == r.refuse {
		return "", os.ErrPermission
	}
	return r.fakeProber.SMGUID(ctx, ibdev, port)
}

func TestDisabledDoesNothing(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, LevelDisabled, nil)
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, ReportFile)); err == nil || len(b.bound) != 0 {
		t.Errorf("disabled level acted")
	}
}

func TestUserQPs(t *testing.T) {
	kernel := []byte(`[{"ifindex":5,"ifname":"mlx5_0","port":1,"lqpn":1,"type":"GSI","state":"RTS","comm":"[ib_core]"}]`)
	if used, _ := userQPs(kernel, "mlx5_0"); used {
		t.Errorf("kernel QP counted as host use")
	}
	user := []byte(`[{"ifname":"mlx5_0","lqpn":77,"type":"RC","pid":4242,"comm":"ib_write_bw"}]`)
	if used, _ := userQPs(user, "mlx5_0"); !used {
		t.Errorf("user QP not counted")
	}
}

func TestDefaultRouteVia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route")
	body := "Iface\tDestination\tGateway\nib0\t00000000\t0102A8C0\nib1\t0002A8C0\t00000000\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := defaultRouteVia(path, "ib0"); err != nil || !v {
		t.Errorf("IPv4 default route through ib0 missed: %v", err)
	}
	if v, _ := defaultRouteVia(path, "ib1"); v {
		t.Errorf("non-default route counted")
	}
	if _, err := defaultRouteVia(filepath.Join(t.TempDir(), "absent"), "ib0"); err == nil {
		t.Errorf("unreadable route table did not fail")
	}
	path6 := filepath.Join(t.TempDir(), "ipv6_route")
	body6 := "00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003 ib0\n"
	if err := os.WriteFile(path6, []byte(body6), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := defaultRoute6Via(path6, "ib0"); err != nil || !v {
		t.Errorf("IPv6 default route through ib0 missed: %v", err)
	}
}

func TestInvalidAdminFileStopsBinding(t *testing.T) {
	root := node(t)
	p, b := newPrep(t, root, LevelBind, fakeHostUse{})
	if err := os.WriteFile(p.AdminFile, []byte(`{"version":"v1","devices":{"0000:37:01.0":{"fabric":{"string":"host"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.bound) != 0 {
		t.Errorf("bound with an unusable admin file: %v", b.bound)
	}
}

func TestOutsidePortWithoutFactsIsIncomplete(t *testing.T) {
	gpus := []GPUInfo{gpu("g15", "pci0000:15")}
	ports := []PortFacts{port("c15", "pci0000:15", "0xa"), {BDF: "x", Root: "pci0000:99", Status: FactPortNotActive}}
	if got := classifyHGX(gpus, ports, nil); got.Result != Incomplete {
		t.Errorf("got %+v", got)
	}
	ports[1] = PortFacts{BDF: "x", Root: "pci0000:99", Status: FactUnsupported, StatusErr: "function has 2 InfiniBand ports"}
	if got := classifyHGX(gpus, ports, nil); got.Result != Unmatched {
		t.Errorf("multi-port function: got %+v", got)
	}
}

func TestChildNetdevs(t *testing.T) {
	sys := t.TempDir()
	mk := func(name, idx, link string) {
		d := filepath.Join(sys, "class", "net", name)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "ifindex"), []byte(idx), 0o644)
		os.WriteFile(filepath.Join(d, "iflink"), []byte(link), 0o644)
	}
	mk("ibp9s0", "5", "5")
	mk("storage", "9", "5")  // a renamed P_Key child
	mk("vlan100", "11", "9") // a VLAN on the child
	mk("eth0", "2", "2")
	mk("bond0", "12", "12")
	os.WriteFile(filepath.Join(sys, "class", "net", "bonding_masters"), []byte("bond0"), 0o644)
	os.Symlink("../bond0", filepath.Join(sys, "class", "net", "ibp9s0", "upper_bond0"))
	got, err := SystemHostUse{SysRoot: sys}.netdevsWithChildren([]string{"ibp9s0"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"ibp9s0": true, "storage": true, "vlan100": true, "bond0": true}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected %s in %v", n, got)
		}
	}
}

func TestUserQPsFiltersDevice(t *testing.T) {
	other := []byte(`[{"ifname":"mlx5_1","lqpn":77,"type":"RC","pid":4242,"comm":"ib_write_bw"}]`)
	if used, _ := userQPs(other, "mlx5_0"); used {
		t.Errorf("another device's QP counted")
	}
}

func TestFrozenFileMustValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, GeneratedFile)
	reserved := inventory.Reserved("fabric-nic.example.com")
	bad := `{"version":"v2","bootID":"b1","detector":"` + Detector + `","devices":{"0000:15:01.0":{"example.com/fabric":{"string":"compute"}}}}`
	os.WriteFile(path, []byte(bad), 0o644)
	if _, ok := frozenFor(path, boot, reserved); ok {
		t.Errorf("a file the plugin rejects froze the classification")
	}
	other := `{"version":"v1","bootID":"b1","detector":"another/v9","devices":{}}`
	os.WriteFile(path, []byte(other), 0o644)
	if _, ok := frozenFor(path, boot, reserved); ok {
		t.Errorf("a file from another detector froze the classification")
	}
}

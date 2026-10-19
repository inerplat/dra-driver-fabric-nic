package prep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/inventory"
	"github.com/inerplat/dra-driver-fabric-nic/pkg/sysfs"
)

// Results of one port.
const (
	ResultBound      = "bound"   // bound to vfio-pci by this run
	ResultOnVFIO     = "on-vfio" // already on vfio-pci
	ResultKept       = "kept"    // left on its driver as declared
	ResultRefused    = "refused" // declared or detected for vfio-pci, not bound
	ResultReturned   = "returned-by-operator"
	ResultUndeclared = "undeclared" // no declaration and no detected role
	ResultNoID       = "no-id"
	ResultPreview    = "preview" // report mode: what auto or explicit would do
)

// Config is what a Prep needs to know about the node.
type Config struct {
	DriverName string
	SysRoot    string
	ProcRoot   string
	StateDir   string
	GPUVendors []string
	// Uptime is the time since boot. Deadline bounds how long an incomplete
	// detection is retried after boot.
	Uptime     time.Duration
	Deadline   time.Duration
	VPDTimeout time.Duration
	Now        func() time.Time
}

// Prep applies an Intent once.
type Prep struct {
	Config
	Prober  Prober
	HostUse HostUse
	Binder  Binder
	// HostView checks that prep sees the host's network and processes; a
	// failure stops binding.
	HostView func() error
}

type decision struct {
	Decl
	Source string
}

// Apply decides every port's role, binds the vfio-pci ones, writes the
// plugin's attribute files and the report, and returns the exit code: 1 asks
// the kubelet to run prep again (an incomplete detection soon after boot), 0
// lets the plugin start.
func (p *Prep) Apply(ctx context.Context, in Intent) (Report, int) {
	logger := klog.FromContext(ctx)
	rep := Report{Node: in.Node, BootID: in.BootID, Mode: in.Mode, Detector: Detector, Updated: p.Now().UTC().Format(time.RFC3339),
		Invalid: in.Invalid, Unknown: in.Unknown}
	reserved := inventory.Reserved(p.DriverName)

	unlock, err := lock(filepath.Join(p.StateDir, LockFile))
	if err != nil {
		rep.Fatal = fmt.Sprintf("lock state directory: %v", err)
		return rep, 1
	}
	defer unlock()

	declared, generated := map[string]string{}, map[string]string{}
	finish := func(code int) (Report, int) {
		for path, roles := range map[string]map[string]string{DeclaredFile: declared, GeneratedFile: generated} {
			if err := writeAttributes(filepath.Join(p.StateDir, path), in.BootID, roles, reserved); err != nil {
				rep.Fatal = fmt.Sprintf("write %s: %v", path, err)
				code = 1
			}
		}
		if err := writeJSON(filepath.Join(p.StateDir, ReportFile), rep); err != nil {
			logger.Error(err, "write report")
			code = 1
		}
		return rep, code
	}
	failClosed := func(why string) (Report, int) {
		declared, generated = map[string]string{}, map[string]string{}
		rep.Fatal = why
		return finish(0)
	}

	if in.ModeErr != "" {
		return failClosed(in.ModeErr)
	}
	if in.Mode == ModeOff {
		return finish(0)
	}
	gpus, excluded, err := gpuInfo(p.SysRoot, p.GPUVendors)
	if err != nil {
		rep.Fatal = fmt.Sprintf("read GPUs: %v", err)
		return finish(1)
	}
	rep.GPUs, rep.ExcludedGPUs = gpus, excluded
	funcs, err := enumerate(p.SysRoot, gpus, p.VPDTimeout)
	if err != nil {
		rep.Fatal = fmt.Sprintf("list network functions: %v", err)
		return finish(1)
	}
	binds, err := loadBinds(p.StateDir, in.BootID)
	if err != nil {
		rep.Fatal = fmt.Sprintf("read %s: %v", BindFile, err)
		return finish(1)
	}
	ours := map[string]bool{} // BDF of a function prep bound this boot, still with the same ID
	ids := map[string]string{}
	for _, f := range funcs {
		ids[f.BDF] = f.ID
	}
	for _, b := range binds {
		if b.ID != "" && ids[b.BDF] == b.ID {
			ours[b.BDF] = true
		}
	}
	rep.Binds = binds

	present := map[string]bool{}
	for _, f := range funcs {
		if f.ID != "" {
			present[f.ID] = true
		}
	}
	for _, id := range sortedKeys(in.Ports) {
		if !present[id] {
			rep.Stale = append(rep.Stale, id)
		}
	}

	dec := map[string]decision{} // by BDF
	for _, f := range funcs {
		if d, ok := in.Ports[f.ID]; ok && f.ID != "" {
			dec[f.BDF] = decision{d, "declared"}
		}
	}
	var outcome Outcome
	var facts []PortFacts
	if in.Mode == ModeAuto || in.Mode == ModeReport {
		outcome, facts = p.detect(ctx, in.BootID, gpus, funcs)
		rep.Auto, rep.Facts = &outcome, facts
		compute := map[string]bool{}
		for _, id := range outcome.Compute {
			compute[id] = true
		}
		for _, f := range funcs {
			if _, ok := dec[f.BDF]; !ok && f.ID != "" && compute[f.ID] {
				dec[f.BDF] = decision{Decl{Role: "compute", Bind: BindVFIO}, "auto"}
			}
		}
	}
	fabric := map[string]string{}
	for _, f := range facts {
		if f.Status == FactOK {
			fabric[f.BDF] = f.group()
		}
	}

	if (in.Mode == ModeAuto || in.Mode == ModeExplicit) && len(in.Invalid) > 0 {
		for _, f := range funcs {
			rep.Ports = append(rep.Ports, PortResult{Function: f, Result: ResultRefused, Reason: "invalid declarations on the node", Fabric: fabric[f.BDF]})
		}
		return failClosed("the node has invalid port declarations; nothing is bound until they are fixed")
	}

	canBind := in.Mode == ModeAuto || in.Mode == ModeExplicit
	if canBind && p.HostView != nil {
		if err := p.HostView(); err != nil {
			canBind = false
			rep.Fatal = fmt.Sprintf("cannot see the host's network and processes, binding nothing: %v", err)
		}
	}
	for _, f := range funcs {
		r := PortResult{Function: f, Fabric: fabric[f.BDF]}
		d, ok := dec[f.BDF]
		switch {
		case f.ID == "":
			r.Result, r.Reason = ResultNoID, f.IDError
		case !ok:
			r.Result = ResultUndeclared
			if f.NVLinkManagement {
				r.Reason = "NVLink management bridge"
			}
		default:
			r.Role, r.Bind, r.Source = d.Role, d.Bind, d.Source
			if in.Mode == ModeReport {
				r.Result = ResultPreview
				if why := refusal(f, d); why != "" {
					r.Reason = "would refuse: " + why
				}
				break
			}
			r.Result, r.Reason = p.place(ctx, in.BootID, f, d, canBind, ours, &binds)
			// A role the hardware contradicts is not published; one that only
			// could not be bound is.
			if why := contradiction(f, d); why != "" {
				r.Reason = why
			} else if d.Source == "declared" {
				declared[f.BDF] = d.Role
			} else {
				generated[f.BDF] = d.Role
			}
		}
		rep.Ports = append(rep.Ports, r)
	}
	rep.Binds = binds

	code := 0
	if in.Mode == ModeAuto && outcome.Result == Incomplete && p.Uptime < p.Deadline {
		code = 1
	}
	return finish(code)
}

// contradiction returns why the hardware rules out a role.
func contradiction(f Function, d decision) string {
	switch {
	case f.NVLinkManagement && d.Role != "other":
		return "an NVLink management bridge can only be other"
	case d.Role == "compute" && f.GPU == "":
		return "a compute port must share a PCIe root with a GPU"
	}
	return ""
}

// refusal returns why a declaration cannot be bound to vfio-pci, from facts
// that need no probing.
func refusal(f Function, d decision) string {
	if d.Bind != BindVFIO {
		return ""
	}
	switch {
	case f.NVLinkManagement:
		return "NVLink management bridge"
	case f.Vendor != MellanoxVendor:
		return "prep binds only Mellanox functions"
	case contradiction(f, d) != "":
		return contradiction(f, d)
	case f.NumVFs > 0:
		return fmt.Sprintf("has %d VFs enabled", f.NumVFs)
	}
	return ""
}

// place carries out one decision. It never moves a function off vfio-pci.
func (p *Prep) place(ctx context.Context, bootID string, f Function, d decision, canBind bool, ours map[string]bool, binds *[]BindRecord) (string, string) {
	onVFIO := sysfs.IsVFIODriver(f.Driver)
	if d.Bind == BindKeep {
		if onVFIO {
			return ResultOnVFIO, ""
		}
		return ResultKept, ""
	}
	if why := refusal(f, d); why != "" {
		return ResultRefused, why
	}
	switch {
	case onVFIO:
		return ResultOnVFIO, ""
	case ours[f.BDF]:
		return ResultReturned, "prep bound it earlier in this boot and someone moved it back; left for this boot"
	case !canBind:
		return ResultRefused, "binding is off for this run"
	}
	if err := groupPlan(p.SysRoot, f.IOMMUGroup, f.BDF); err != nil {
		return ResultRefused, err.Error()
	}
	// A driver that creates no interface prep can see may have handed it to
	// another network namespace, where the host-use checks cannot look.
	switch {
	case f.Driver == "":
	case f.InfiniBand && f.IBDev == "":
		return ResultRefused, "no RDMA device visible in the host's namespaces"
	case !f.InfiniBand && len(f.Netdevs) == 0:
		return ResultRefused, "no network interface visible in the host's network namespace"
	}
	if used, why := p.HostUse.InUse(ctx, PortFacts{BDF: f.BDF, IBDev: f.IBDev, Netdevs: f.Netdevs}); used {
		return ResultRefused, "host uses it: " + why
	}
	// Check again right before detaching: the host may have taken the port
	// since the first look.
	now, ok := pfNow(p.SysRoot, f.BDF)
	if !ok || now.Driver != f.Driver || now.NumVFs > 0 || now.IOMMUGroup != f.IOMMUGroup {
		return ResultRefused, "changed during the run"
	}
	if used, why := p.HostUse.InUse(ctx, PortFacts{BDF: f.BDF, IBDev: now.IBDev, Netdevs: now.Netdevs}); used {
		return ResultRefused, "host uses it: " + why
	}
	// Record first: a function prep moved must never be moved again this
	// boot, even if prep dies right after the write.
	*binds = append(*binds, BindRecord{
		BDF: f.BDF, ID: f.ID, IBDev: now.IBDev, Netdevs: now.Netdevs, OriginalDriver: now.Driver,
		IOMMUGroup: now.IOMMUGroup, At: p.Now().UTC().Format(time.RFC3339),
	})
	if err := saveBinds(p.StateDir, bootID, *binds); err != nil {
		*binds = (*binds)[:len(*binds)-1]
		return ResultRefused, fmt.Sprintf("cannot save the bind record: %v", err)
	}
	// The card in the slot is checked last, right before it is detached.
	verify := func() error {
		vpd, err := sysfs.ReadVPD(filepath.Join(p.SysRoot, "bus", "pci", "devices", f.BDF), p.VPDTimeout)
		if err != nil || vpd.Serial+"-f"+f.BDF[len(f.BDF)-1:] != f.ID || vpd.NVLinkManagement {
			return errors.New("the card in this slot changed or its VPD cannot be read")
		}
		return nil
	}
	if err := p.Binder.BindVFIO(f.BDF, verify); err != nil {
		return ResultRefused, fmt.Sprintf("not bound: %v", err)
	}
	klog.FromContext(ctx).Info("bound to vfio-pci", "bdf", f.BDF, "id", f.ID, "from", now.Driver)
	return ResultBound, ""
}

// detect classifies the ports, or returns this boot's earlier match: ports
// prep has bound since cannot be probed again.
func (p *Prep) detect(ctx context.Context, bootID string, gpus []GPUInfo, funcs []Function) (Outcome, []PortFacts) {
	layout := fingerprint(gpus, funcs)
	if a, ok := loadAuto(p.StateDir, bootID); ok && a.Fingerprint == layout {
		return Outcome{Result: Matched, Compute: a.Compute, Reason: "classified earlier in this boot"}, a.Facts
	}
	skip := map[string]bool{}
	for _, f := range funcs {
		switch {
		case f.NVLinkManagement:
			skip[f.BDF] = true
		case !f.InfiniBand:
		case f.ID == "":
			return Outcome{Result: Incomplete, Reason: fmt.Sprintf("function %s has no ID: %s", f.BDF, f.IDError)}, nil
		case f.Root == "":
			return Outcome{Result: Incomplete, Reason: "cannot find the PCIe root of " + f.BDF}, nil
		}
	}
	first, err := readFacts(ctx, p.SysRoot, p.Prober, skip)
	if err != nil {
		return Outcome{Result: Incomplete, Reason: fmt.Sprintf("read ports: %v", err)}, nil
	}
	// Every InfiniBand function must have been probed: the fabric of any
	// other one is unknown, even one prep bound earlier for a declaration.
	probed := map[string]bool{}
	for _, f := range first {
		probed[f.BDF] = true
	}
	var unprobed []string
	for _, f := range funcs {
		if f.InfiniBand && !skip[f.BDF] && !probed[f.BDF] {
			unprobed = append(unprobed, f.BDF)
		}
	}
	outcome := classifyHGX(gpus, first, unprobed)
	if outcome.Result == Unmatched {
		return outcome, first
	}
	second, err := readFacts(ctx, p.SysRoot, p.Prober, skip)
	if err != nil {
		return Outcome{Result: Incomplete, Reason: fmt.Sprintf("read ports: %v", err)}, first
	}
	if same, why := sameFabricFacts(first, second); !same {
		return Outcome{Result: Incomplete, Reason: why}, second
	}
	if outcome.Result != Matched {
		return outcome, second
	}
	id := map[string]string{}
	for _, f := range funcs {
		id[f.BDF] = f.ID
	}
	ids := make([]string, 0, len(outcome.Compute))
	for _, bdf := range outcome.Compute {
		ids = append(ids, id[bdf])
	}
	sort.Strings(ids)
	outcome.Compute = ids
	if err := writeJSON(filepath.Join(p.StateDir, AutoFile), autoFile{BootID: bootID, Detector: Detector, Fingerprint: layout, Compute: ids, Facts: second}); err != nil {
		return Outcome{Result: Incomplete, Reason: fmt.Sprintf("save %s: %v", AutoFile, err)}, second
	}
	return outcome, second
}

// fingerprint names the PCI layout a detection was made on: every network
// PF with its ID and PCIe root, and every GPU with its root. Drivers are left
// out, since binding changes them.
func fingerprint(gpus []GPUInfo, funcs []Function) string {
	var parts []string
	for _, g := range gpus {
		parts = append(parts, "gpu "+g.BDF+" "+g.Root)
	}
	for _, f := range funcs {
		parts = append(parts, fmt.Sprintf("pf %s %s %s %v %v", f.BDF, f.ID, f.Root, f.InfiniBand, f.NVLinkManagement))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func pfNow(sysRoot, bdf string) (sysfs.NetworkPF, bool) {
	pfs, err := sysfs.NetworkPFs(sysRoot)
	if err != nil {
		return sysfs.NetworkPF{}, false
	}
	for _, p := range pfs {
		if p.BDF == bdf {
			return p, true
		}
	}
	return sysfs.NetworkPF{}, false
}

func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

package prep

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/inventory"
	"github.com/inerplat/dra-driver-fabric-nic/pkg/sysfs"
)

// Levels a node label can select.
const (
	LevelReport   = "report"
	LevelBind     = "bind"
	LevelDisabled = "disabled"
)

// Config is what a Prep needs to know about the node and its files.
type Config struct {
	DriverName string // attribute domain; reserved in the files
	Version    string
	SysRoot    string
	StateDir   string
	AdminFile  string
	GPUVendors []string
	BootID     string
	RetryMin   time.Duration
	RetryMax   time.Duration
}

// Prep runs classification and binding passes for one node.
type Prep struct {
	Config
	Prober  Prober
	HostUse HostUse
	Binder  Binder
	// Level returns the level the node label selects.
	Level func(ctx context.Context) (string, error)
	// Event records an event on the node; it may be nil.
	Event func(ctx context.Context, reason, message string)
	Now   func() time.Time

	last          Outcome
	facts         []PortFacts
	gpus          []GPUInfo
	excluded      []string
	unprobed      []string
	retries       int
	nextTry       time.Time
	unmatchedSeen bool
	conflictKey   string
}

// Sweep runs one pass and writes the report.
func (p *Prep) Sweep(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	level, err := p.Level(ctx)
	if err != nil {
		return fmt.Errorf("read node level: %w", err)
	}
	if level != LevelReport && level != LevelBind {
		logger.V(2).Info("prep is not enabled on this node", "level", level)
		return nil
	}
	target := ProposedFile
	if level == LevelBind {
		target = GeneratedFile
	}
	reserved := inventory.Reserved(p.DriverName)

	compute, frozen := frozenFor(filepath.Join(p.StateDir, target), p.BootID, reserved)
	if !frozen {
		p.classify(ctx, level, target, reserved)
		compute, frozen = frozenFor(filepath.Join(p.StateDir, target), p.BootID, reserved)
	}

	report := Report{
		BootID: p.BootID, Level: level, Detector: Detector, Updated: p.Now().UTC().Format(time.RFC3339),
		Outcome: p.last, Frozen: frozen, Retries: p.retries,
		GPUs: p.gpus, ExcludedGPUs: p.excluded, Ports: p.facts, Unprobed: p.unprobed,
	}
	if frozen && p.last.Result == "" {
		report.Outcome = Outcome{Result: Matched, Compute: sortedKeys(compute), Reason: "classified earlier in this boot"}
	}

	// An admin file that exists but cannot be read may hold the very
	// exclusion that protects a port, so it stops binding altogether.
	admin, adminErr := inventory.LoadExtra(p.AdminFile, p.BootID, reserved)
	if adminErr != nil {
		report.Conflicts = append(report.Conflicts, fmt.Sprintf("admin file unusable: %v", adminErr))
	}
	for _, bdf := range sortedKeys(compute) {
		if v := admin[bdf][RoleAttribute].StringValue; v != nil && *v != "compute" {
			report.Conflicts = append(report.Conflicts, fmt.Sprintf("%s: admin file says %s, inferred compute; the admin file wins", bdf, *v))
		}
	}
	if key := strings.Join(report.Conflicts, "\n"); key != p.conflictKey {
		p.conflictKey = key
		if key != "" {
			p.event(ctx, "AttributeConflict", key)
		}
	}

	if level == LevelBind && frozen && adminErr != nil {
		report.BindNotes = []string{"admin file unusable; binding nothing until it is fixed"}
		report.Binds, _ = loadBinds(p.StateDir, p.BootID)
	} else if level == LevelBind && frozen {
		notes, binds := p.bind(ctx, compute, admin)
		report.BindNotes, report.Binds = notes, binds
	} else if binds, err := loadBinds(p.StateDir, p.BootID); err == nil {
		report.Binds = binds
	}
	return writeJSON(filepath.Join(p.StateDir, ReportFile), report)
}

// classify runs one classification unless the last outcome says not to.
func (p *Prep) classify(ctx context.Context, level, target string, reserved func(resourceapi.QualifiedName) bool) {
	logger := klog.FromContext(ctx)
	if p.unmatchedSeen {
		return // deterministic until the hardware changes; retried on restart
	}
	if p.last.Result == Incomplete && p.Now().Before(p.nextTry) {
		return
	}

	outcome := p.pass(ctx)
	if outcome.Result == Matched {
		if err := writeAttributes(filepath.Join(p.StateDir, target), p.BootID, level, p.Version, outcome.Compute, reserved); err != nil {
			outcome = Outcome{Result: Incomplete, Reason: fmt.Sprintf("write %s: %v", target, err)}
		}
	}
	switch outcome.Result {
	case Incomplete:
		p.retries++
		p.nextTry = p.Now().Add(p.backoff())
	case Unmatched:
		p.unmatchedSeen = true
	}
	if outcome.Result != p.last.Result || outcome.Reason != p.last.Reason {
		logger.Info("classification", "result", outcome.Result, "reason", outcome.Reason, "compute", outcome.Compute)
		p.event(ctx, "Classification", fmt.Sprintf("%s: %s", outcome.Result, outcome.Reason))
	}
	p.last = outcome
}

// pass reads the facts twice around the classification so that a fabric
// change during the pass is caught.
func (p *Prep) pass(ctx context.Context) Outcome {
	gpus, excluded, err := gpuInfo(p.SysRoot, p.GPUVendors)
	if err != nil {
		return Outcome{Result: Incomplete, Reason: fmt.Sprintf("read GPUs: %v", err)}
	}
	p.gpus, p.excluded = gpus, excluded
	binds, _ := loadBinds(p.StateDir, p.BootID)
	ours := map[string]bool{}
	for _, b := range binds {
		ours[b.BDF] = true
	}
	unprobed, err := unprobedOnGPURoots(p.SysRoot, gpus, ours)
	if err != nil {
		return Outcome{Result: Incomplete, Reason: fmt.Sprintf("scan vfio functions: %v", err)}
	}
	p.unprobed = unprobed
	first, err := readFacts(ctx, p.SysRoot, p.Prober)
	if err != nil {
		return Outcome{Result: Incomplete, Reason: fmt.Sprintf("read ports: %v", err)}
	}
	p.facts = first
	outcome := classifyHGX(gpus, first, unprobed)
	if outcome.Result == Unmatched && len(unprobed) > 0 {
		return outcome
	}
	second, err := readFacts(ctx, p.SysRoot, p.Prober)
	if err != nil {
		return Outcome{Result: Incomplete, Reason: fmt.Sprintf("read ports: %v", err)}
	}
	if same, why := sameFabricFacts(first, second); !same {
		return Outcome{Result: Incomplete, Reason: why}
	}
	return outcome
}

func (p *Prep) backoff() time.Duration {
	d := p.RetryMin
	for i := 1; i < p.retries && d < p.RetryMax; i++ {
		d *= 2
	}
	if d > p.RetryMax {
		d = p.RetryMax
	}
	jitter := time.Duration(float64(d) * (rand.Float64()*0.4 - 0.2))
	return d + jitter
}

// bind moves the eligible compute ports to vfio-pci, each at most once per
// boot. It never moves anything off vfio-pci.
func (p *Prep) bind(ctx context.Context, compute map[string]bool, admin map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) ([]string, []BindRecord) {
	var notes []string
	records, err := loadBinds(p.StateDir, p.BootID)
	if err != nil {
		return []string{fmt.Sprintf("bind record unreadable, binding nothing: %v", err)}, nil
	}
	recorded := map[string]bool{}
	for _, r := range records {
		recorded[r.BDF] = true
	}
	host, err := sysfs.HostIBPorts(p.SysRoot)
	if err != nil {
		return []string{fmt.Sprintf("scan host ports, binding nothing: %v", err)}, records
	}
	onHost := map[string]sysfs.HostPort{}
	for _, h := range host {
		onHost[h.BDF] = h
	}

	eligible := map[string]bool{}
	for _, bdf := range sortedKeys(compute) {
		h, isHost := onHost[bdf]
		switch {
		case recorded[bdf] && isHost:
			notes = append(notes, bdf+": returned-by-operator; left on "+h.Driver+" for this boot")
			continue
		case !isHost:
			continue // already on vfio-pci, or gone
		}
		if v := admin[bdf][RoleAttribute].StringValue; v != nil && *v != "compute" {
			notes = append(notes, fmt.Sprintf("%s: admin file says %s; not bound", bdf, *v))
			continue
		}
		if h.NumVFs > 0 {
			notes = append(notes, fmt.Sprintf("%s: has %d VFs enabled; not bound", bdf, h.NumVFs))
			continue
		}
		facts := PortFacts{BDF: bdf, IBDev: h.IBDev, Netdevs: h.Netdevs}
		if used, why := p.HostUse.InUse(ctx, facts); used {
			notes = append(notes, fmt.Sprintf("%s: host uses it (%s); not bound", bdf, why))
			continue
		}
		eligible[bdf] = true
	}

	for _, bdf := range sortedKeys(eligible) {
		h := onHost[bdf]
		if err := groupPlan(p.SysRoot, h.IOMMUGroup, bdf); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v; not bound", bdf, err))
			continue
		}
		// Check again right before detaching: the host may have taken the
		// port since the first check.
		now, ok := hostPortNow(p.SysRoot, bdf)
		if !ok || now.Driver != h.Driver || now.NumVFs > 0 || now.IOMMUGroup != h.IOMMUGroup {
			notes = append(notes, bdf+": changed during the pass; not bound")
			continue
		}
		h = now
		if used, why := p.HostUse.InUse(ctx, PortFacts{BDF: bdf, IBDev: h.IBDev, Netdevs: h.Netdevs}); used {
			notes = append(notes, fmt.Sprintf("%s: host uses it (%s); not bound", bdf, why))
			continue
		}
		records = append(records, BindRecord{
			BDF: bdf, IBDev: h.IBDev, Netdevs: h.Netdevs, OriginalDriver: h.Driver,
			IOMMUGroup: h.IOMMUGroup, At: p.Now().UTC().Format(time.RFC3339),
		})
		// Record first: a function prep moved must never be moved again this
		// boot, even if prep dies right after the write.
		if err := saveBinds(p.StateDir, p.BootID, records); err != nil {
			notes = append(notes, fmt.Sprintf("%s: cannot save bind record, not bound: %v", bdf, err))
			records = records[:len(records)-1]
			continue
		}
		if err := p.Binder.BindVFIO(bdf); err != nil {
			notes = append(notes, fmt.Sprintf("%s: bind failed: %v", bdf, err))
			continue
		}
		klog.FromContext(ctx).Info("bound to vfio-pci", "bdf", bdf, "from", h.Driver)
	}
	return notes, records
}

func hostPortNow(sysRoot, bdf string) (sysfs.HostPort, bool) {
	ports, err := sysfs.HostIBPorts(sysRoot)
	if err != nil {
		return sysfs.HostPort{}, false
	}
	for _, p := range ports {
		if p.BDF == bdf {
			return p, true
		}
	}
	return sysfs.HostPort{}, false
}

func (p *Prep) event(ctx context.Context, reason, message string) {
	if p.Event != nil {
		p.Event(ctx, reason, message)
	}
}

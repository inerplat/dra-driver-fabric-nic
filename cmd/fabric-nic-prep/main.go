// Command fabric-nic-prep runs once, as an init container of the plugin pod.
// It reads the node's mode label and port annotations, decides the role of
// every Mellanox PF, binds the vfio-pci ones, writes the plugin's attribute
// files and exits. It never writes the Node.
//
// The pod stays in its own network and PID namespaces. prep reads the Node
// there, then runs the rest of itself in the host's network namespace through
// nsenter, and reads the host's processes through the host's procfs. A PID
// namespace cannot be entered from a descendant, so prep never tries.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/prep"
)

type options struct {
	driverName  string
	nodeName    string
	kubeconfig  string
	sysRoot     string
	procRoot    string
	stateDir    string
	gpuVendors  string
	defaultMode string
	deadline    time.Duration
	vpdTimeout  time.Duration
	hostNS      bool
	namespace   string
	podLabel    string
	apply       bool
}

func main() {
	klog.InitFlags(nil)
	var o options
	flag.StringVar(&o.driverName, "driver-name", envOr("DRIVER_NAME", "fabric-nic.inerplat.github.io"), "driver name; also the prefix of the node label and annotations")
	flag.StringVar(&o.nodeName, "node-name", os.Getenv("NODE_NAME"), "name of this node")
	flag.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig path; in-cluster config when empty")
	flag.StringVar(&o.sysRoot, "sys-root", envOr("SYS_ROOT", "/host/sys"), "the host's sysfs")
	flag.StringVar(&o.procRoot, "proc-root", envOr("PROC_ROOT", "/host/proc"), "the host's procfs")
	flag.StringVar(&o.stateDir, "state-dir", envOr("STATE_DIR", "/var/lib/fabric-nic"), "directory for the attribute files and the report")
	flag.StringVar(&o.gpuVendors, "gpu-vendors", "10de", "comma-separated PCI vendor IDs whose display-class devices count as GPUs; 3D controllers always count")
	flag.StringVar(&o.defaultMode, "default-mode", envOr("DEFAULT_MODE", prep.ModeReport), "mode of a node without the mode label: auto, report or off")
	flag.DurationVar(&o.deadline, "deadline", 10*time.Minute, "time after boot during which an incomplete detection makes prep exit 1 so that the kubelet runs it again")
	flag.DurationVar(&o.vpdTimeout, "vpd-timeout", 5*time.Second, "longest wait for one function's VPD")
	flag.BoolVar(&o.hostNS, "host-namespaces", true, "run the node work in the host's network namespace through nsenter")
	flag.StringVar(&o.namespace, "namespace", envOr("POD_NAMESPACE", "fabric-nic"), "namespace of the plugin pods, for the printed commands")
	flag.StringVar(&o.podLabel, "pod-selector", "app=fabric-nic-plugin", "label selector of the plugin pods, for the printed commands")
	flag.BoolVar(&o.apply, "apply", false, "internal: read the intent from stdin and apply it here")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var code int
	var err error
	if o.apply {
		code, err = applyFromStdin(ctx, o)
	} else {
		code, err = run(ctx, o)
	}
	if err != nil {
		klog.ErrorS(err, "fabric-nic-prep")
	}
	os.Exit(code)
}

func run(ctx context.Context, o options) (int, error) {
	if o.nodeName == "" {
		return 1, errors.New("--node-name or NODE_NAME is required")
	}
	if o.defaultMode == prep.ModeExplicit {
		return 1, errors.New("--default-mode explicit would apply no declarations on unlabelled nodes; label the nodes instead")
	}
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return 1, fmt.Errorf("read boot ID: %w", err)
	}
	client, err := kubeClient(o.kubeconfig)
	if err != nil {
		return 1, err
	}
	var labels, annotations map[string]string
	for attempt := 0; ; attempt++ {
		node, err := client.CoreV1().Nodes().Get(ctx, o.nodeName, metav1.GetOptions{})
		if err == nil {
			labels, annotations = node.Labels, node.Annotations
			break
		}
		if attempt == 4 {
			return 1, fmt.Errorf("read node %s: %w", o.nodeName, err)
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
	in := prep.ParseIntent(o.driverName, labels, annotations, o.defaultMode)
	in.Node, in.BootID = o.nodeName, strings.TrimSpace(string(b))
	if !o.hostNS {
		return apply(ctx, o, in)
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return 1, err
	}
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}
	args := []string{"--net=" + filepath.Join(o.procRoot, "1", "ns", "net"), "--", self, "--apply"}
	for _, name := range []string{"driver-name", "sys-root", "proc-root", "state-dir", "gpu-vendors", "deadline", "vpd-timeout", "namespace", "pod-selector", "v"} {
		if f := flag.Lookup(name); f != nil {
			args = append(args, "--"+name+"="+f.Value.String())
		}
	}
	cmd := exec.CommandContext(ctx, "nsenter", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(string(payload)), os.Stdout, os.Stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 1, fmt.Errorf("enter the host's namespaces: %w", err)
	}
	return 0, nil
}

func applyFromStdin(ctx context.Context, o options) (int, error) {
	var in prep.Intent
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		return 1, fmt.Errorf("read intent: %w", err)
	}
	return apply(ctx, o, in)
}

func apply(ctx context.Context, o options, in prep.Intent) (int, error) {
	if err := os.MkdirAll(o.stateDir, 0o755); err != nil {
		return 1, err
	}
	uptime, err := readUptime(filepath.Join(o.procRoot, "uptime"))
	if err != nil {
		return 1, err
	}
	// This process as the host's procfs numbers it, so the fd scan skips it.
	selfPID := os.Getpid()
	if target, err := os.Readlink(filepath.Join(o.procRoot, "self")); err == nil {
		if n, err := strconv.Atoi(target); err == nil {
			selfPID = n
		}
	}
	hostUse := prep.SystemHostUse{
		SysRoot: o.sysRoot, ProcRoot: o.procRoot, SelfPID: selfPID,
		// The host's root, as its PID 1 sees it: the prep image has no lnetctl.
		LnetCmd: []string{"chroot", filepath.Join(o.procRoot, "1", "root"), "lnetctl"},
	}
	p := &prep.Prep{
		Config: prep.Config{
			DriverName: o.driverName, SysRoot: o.sysRoot, ProcRoot: o.procRoot, StateDir: o.stateDir,
			GPUVendors: strings.Split(o.gpuVendors, ","),
			Uptime:     uptime, Deadline: o.deadline, VPDTimeout: o.vpdTimeout, Now: time.Now,
		},
		Prober:   prep.DiagsProber{},
		HostUse:  hostUse,
		Binder:   prep.SysfsBinder{SysRoot: o.sysRoot},
		HostView: hostUse.HostView,
	}
	rep, code := p.Apply(ctx, in)
	prep.PrintReport(os.Stdout, rep, o.driverName, o.namespace, o.podLabel)
	return code, nil
}

func readUptime(path string) (time.Duration, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read uptime: %w", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("parse %s", path)
	}
	s, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return time.Duration(s * float64(time.Second)), nil
}

func kubeClient(kubeconfig string) (kubernetes.Interface, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kube config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

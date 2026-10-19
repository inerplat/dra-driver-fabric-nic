// Command fabric-nic-prep reads fabric facts from InfiniBand ports while they
// are on their host driver, infers the compute fabric, and, at the bind
// level, hands the compute ports to vfio-pci. The node label
// <driver>/prep selects the level: report, bind or disabled.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/prep"
)

var version = "dev"

func main() {
	klog.InitFlags(nil)
	var (
		driverName = flag.String("driver-name", envOr("DRIVER_NAME", "fabric-nic.inerplat.github.io"), "driver name; also the node label domain")
		nodeName   = flag.String("node-name", os.Getenv("NODE_NAME"), "name of this node")
		kubeconfig = flag.String("kubeconfig", "", "kubeconfig path; in-cluster config when empty")
		sysRoot    = flag.String("sys-root", envOr("SYS_ROOT", "/sys"), "host sysfs")
		procRoot   = flag.String("proc-root", envOr("PROC_ROOT", "/proc"), "host procfs (the pod needs the host PID namespace)")
		stateDir   = flag.String("state-dir", envOr("STATE_DIR", "/var/lib/fabric-nic"), "directory for the report and generated files")
		gpuVendors = flag.String("gpu-vendors", "10de", "comma-separated PCI vendor IDs whose display-class devices count as GPUs; 3D controllers always count")
		interval   = flag.Duration("interval", 30*time.Second, "time between passes")
		retryMax   = flag.Duration("retry-max", 10*time.Minute, "longest wait before retrying an incomplete pass")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *driverName, *nodeName, *kubeconfig, *sysRoot, *procRoot, *stateDir, *gpuVendors, *interval, *retryMax); err != nil {
		klog.ErrorS(err, "exiting")
		os.Exit(1)
	}
}

func run(ctx context.Context, driverName, nodeName, kubeconfig, sysRoot, procRoot, stateDir, gpuVendors string, interval, retryMax time.Duration) error {
	if nodeName == "" {
		return fmt.Errorf("--node-name or NODE_NAME is required")
	}
	client, err := kubeClient(kubeconfig)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return fmt.Errorf("read boot ID: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	labelKey := driverName + "/prep"

	p := &prep.Prep{
		Config: prep.Config{
			DriverName: driverName, Version: version, SysRoot: sysRoot, StateDir: stateDir,
			AdminFile:  filepath.Join(stateDir, "attributes.json"),
			GPUVendors: strings.Split(gpuVendors, ","),
			BootID:     strings.TrimSpace(string(b)),
			RetryMin:   30 * time.Second, RetryMax: retryMax,
		},
		Prober:  prep.DiagsProber{},
		HostUse: prep.SystemHostUse{SysRoot: sysRoot, ProcRoot: procRoot, SelfPID: os.Getpid()},
		Binder:  prep.SysfsBinder{SysRoot: sysRoot},
		Level: func(ctx context.Context) (string, error) {
			node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			if v, ok := node.Labels[labelKey]; ok {
				return v, nil
			}
			return prep.LevelDisabled, nil
		},
		Event: func(ctx context.Context, reason, message string) {
			nodeEvent(ctx, client, nodeName, reason, message)
		},
		Now: time.Now,
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := p.Sweep(ctx); err != nil {
			klog.ErrorS(err, "pass failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func nodeEvent(ctx context.Context, client kubernetes.Interface, nodeName, reason, message string) {
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: nodeName + ".fabric-nic-", Namespace: metav1.NamespaceDefault},
		InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: nodeName, UID: "", APIVersion: "v1"},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: "fabric-nic-prep", Host: nodeName},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if _, err := client.CoreV1().Events(metav1.NamespaceDefault).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		klog.ErrorS(err, "record node event", "reason", reason)
	}
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

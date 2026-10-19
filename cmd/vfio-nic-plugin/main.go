// Command vfio-nic-plugin publishes the network functions a node has bound to
// vfio-pci as DRA devices, for VM runtimes such as KubeVirt and Kata.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metadatav1alpha1 "k8s.io/dynamic-resource-allocation/api/metadata/v1alpha1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/inerplat/dra-driver-vfio-nic/pkg/cdi"
	"github.com/inerplat/dra-driver-vfio-nic/pkg/inventory"
	"github.com/inerplat/dra-driver-vfio-nic/pkg/plugin"
	"github.com/inerplat/dra-driver-vfio-nic/pkg/sysfs"
)

type options struct {
	driverName     string
	nodeName       string
	kubeconfig     string
	sysRoot        string
	cdiDir         string
	attributesFile string
	defaultBackend string
	interval       time.Duration
	registrarDir   string
	pluginsDir     string
}

func main() {
	klog.InitFlags(nil)
	var o options
	flag.StringVar(&o.driverName, "driver-name", envOr("DRIVER_NAME", "vfio-nic.inerplat.github.io"), "DRA driver name, also the attribute domain")
	flag.StringVar(&o.nodeName, "node-name", os.Getenv("NODE_NAME"), "name of this node")
	flag.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig path; in-cluster config when empty")
	flag.StringVar(&o.sysRoot, "sys-root", envOr("SYS_ROOT", "/sys"), "sysfs mount to scan")
	flag.StringVar(&o.cdiDir, "cdi-dir", envOr("CDI_DIR", "/var/run/cdi"), "directory for the CDI spec")
	flag.StringVar(&o.attributesFile, "attributes-file", os.Getenv("ATTRIBUTES_FILE"), "JSON file with extra attributes per PCI address")
	flag.StringVar(&o.defaultBackend, "vfio-backend", envOr("VFIO_BACKEND", cdi.BackendLegacy), "VFIO backend when a claim does not choose one: legacy or iommufd")
	flag.DurationVar(&o.interval, "rescan-interval", 30*time.Second, "how often to rescan sysfs and the attribute file")
	flag.StringVar(&o.registrarDir, "registrar-dir", kubeletplugin.KubeletRegistryDir, "kubelet plugin registration directory")
	flag.StringVar(&o.pluginsDir, "plugins-dir", kubeletplugin.KubeletPluginsDir, "kubelet plugins directory")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o); err != nil {
		klog.ErrorS(err, "exiting")
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) error {
	if o.nodeName == "" {
		return errors.New("--node-name or NODE_NAME is required")
	}
	if o.defaultBackend != cdi.BackendLegacy && o.defaultBackend != cdi.BackendIommufd {
		return fmt.Errorf("--vfio-backend %q: want %s or %s", o.defaultBackend, cdi.BackendLegacy, cdi.BackendIommufd)
	}
	client, err := kubeClient(o.kubeconfig)
	if err != nil {
		return err
	}

	p := &plugin.Plugin{
		DriverName:     o.driverName,
		ConfigVersion:  o.driverName + "/v1alpha1",
		DefaultBackend: o.defaultBackend,
		CDI:            cdi.Writer{Dir: o.cdiDir, Kind: o.driverName + "/nic"},
	}

	// The inventory must exist before the kubelet can call prepare.
	snap, err := snapshot(o)
	if err != nil {
		return err
	}
	if err := p.CDI.Write(snap.devices); err != nil {
		return fmt.Errorf("write CDI spec: %w", err)
	}
	p.SetDevices(snap.devices)

	helper, err := kubeletplugin.Start(ctx, p,
		kubeletplugin.DriverName(o.driverName),
		kubeletplugin.NodeName(o.nodeName),
		kubeletplugin.KubeClient(client),
		kubeletplugin.RegistrarDirectoryPath(o.registrarDir),
		kubeletplugin.PluginDataDirectoryPath(filepath.Join(o.pluginsDir, o.driverName)),
		kubeletplugin.EnableDeviceMetadata(true),
		kubeletplugin.CDIDirectory(o.cdiDir),
		kubeletplugin.MetadataVersions(metadatav1alpha1.SchemeGroupVersion),
	)
	if err != nil {
		return fmt.Errorf("start kubelet plugin: %w", err)
	}
	defer helper.Stop()

	if err := publish(ctx, helper, o.nodeName, snap); err != nil {
		return err
	}
	current, last := snap.devices, snap.hash

	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		snap, err := snapshot(o)
		if err != nil {
			klog.ErrorS(err, "rescan failed, keeping the published devices")
			continue
		}
		if snap.hash == last {
			continue
		}
		// Add devices to the CDI spec before publishing them, and drop
		// them from it only after they are no longer published.
		if err := p.CDI.Write(union(current, snap.devices)); err != nil {
			klog.ErrorS(err, "write CDI spec, keeping the published devices")
			continue
		}
		p.SetDevices(snap.devices)
		if err := publish(ctx, helper, o.nodeName, snap); err != nil {
			klog.ErrorS(err, "publish failed")
			continue
		}
		if err := p.CDI.Write(snap.devices); err != nil {
			klog.ErrorS(err, "write CDI spec")
		}
		current, last = snap.devices, snap.hash
	}
}

// union keeps every device of b and the devices of a that b does not have.
func union(a, b []inventory.Device) []inventory.Device {
	seen := make(map[string]bool, len(b))
	out := append([]inventory.Device(nil), b...)
	for _, d := range b {
		seen[d.Name] = true
	}
	for _, d := range a {
		if !seen[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

type state struct {
	devices []inventory.Device
	hash    string
}

func snapshot(o options) (state, error) {
	funcs, skipped, err := sysfs.Scan(o.sysRoot)
	if err != nil {
		return state{}, err
	}
	for _, s := range skipped {
		klog.V(2).InfoS("skipping function", "reason", s)
	}
	extra, err := inventory.LoadExtra(o.attributesFile, bootID(o.sysRoot), inventory.Reserved(o.driverName))
	if err != nil {
		// Publish without the extra attributes rather than with wrong ones.
		klog.ErrorS(err, "ignoring the attribute file")
		extra = nil
	}
	devices := inventory.Build(funcs, extra, deviceattribute.WithFSFromRoot(o.sysRoot))
	slices := make([]resourceapi.Device, 0, len(devices))
	for _, d := range devices {
		slices = append(slices, d.Slice)
	}
	b, err := json.Marshal(slices)
	if err != nil {
		return state{}, err
	}
	sum := sha256.Sum256(b)
	return state{devices: devices, hash: fmt.Sprintf("%x", sum)}, nil
}

func publish(ctx context.Context, helper *kubeletplugin.Helper, nodeName string, s state) error {
	devices := make([]resourceapi.Device, 0, len(s.devices))
	for _, d := range s.devices {
		devices = append(devices, d.Slice)
	}
	klog.InfoS("publishing devices", "count", len(devices))
	return helper.PublishResources(ctx, resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {Slices: []resourceslice.Slice{{Devices: devices}}},
		},
	})
}

// bootID reads the kernel's boot ID through the proc mount beside sysRoot,
// or /proc when sysRoot is /sys.
func bootID(sysRoot string) string {
	path := filepath.Join(filepath.Dir(sysRoot), "proc", "sys", "kernel", "random", "boot_id")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
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

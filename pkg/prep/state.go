package prep

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	"github.com/inerplat/dra-driver-fabric-nic/pkg/inventory"
)

// File names under the state directory.
const (
	ReportFile = "report.json"
	// DeclaredFile and GeneratedFile are the plugin's attribute files, in
	// precedence order: declared roles first, so the plugin stamps them
	// roleSource=file, then detected roles, stamped roleSource=auto.
	DeclaredFile  = "attributes.json"
	GeneratedFile = "attributes.generated.json"
	AutoFile      = "auto.json" // this boot's detection, by port ID
	BindFile      = "binds.json"
	LockFile      = "lock"
)

// RoleAttribute carries a port's role to the plugin.
const RoleAttribute resourceapi.QualifiedName = "example.com/fabric"

// Detector names the classifier and its version.
const Detector = "classifyHGX/v2"

// BindRecord is a function prep moved to vfio-pci during one boot.
type BindRecord struct {
	BDF            string   `json:"bdf"`
	ID             string   `json:"id"`
	IBDev          string   `json:"ibdev,omitempty"`
	Netdevs        []string `json:"netdevs,omitempty"`
	OriginalDriver string   `json:"originalDriver"`
	IOMMUGroup     string   `json:"iommuGroup"`
	At             string   `json:"at"`
}

type bindFile struct {
	BootID string       `json:"bootID"`
	Binds  []BindRecord `json:"binds"`
}

// autoFile keeps a matched detection for the rest of the boot: once prep
// binds the compute ports it can no longer probe them.
type autoFile struct {
	BootID      string      `json:"bootID"`
	Detector    string      `json:"detector"`
	Fingerprint string      `json:"fingerprint"`
	Compute     []string    `json:"compute"` // port IDs
	Facts       []PortFacts `json:"facts"`
}

// PortResult is what prep decided and did for one function.
type PortResult struct {
	Function
	Role   string `json:"role,omitempty"`
	Bind   string `json:"bind,omitempty"`
	Source string `json:"source,omitempty"` // declared or auto
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	Fabric string `json:"fabric,omitempty"` // subnet prefix / SM GUID
}

// Report is everything one run saw and decided.
type Report struct {
	Node         string       `json:"node"`
	BootID       string       `json:"bootID"`
	Mode         string       `json:"mode"`
	Detector     string       `json:"detector"`
	Updated      string       `json:"updated"`
	Fatal        string       `json:"fatal,omitempty"` // why nothing was bound
	Auto         *Outcome     `json:"auto,omitempty"`
	GPUs         []GPUInfo    `json:"gpus"`
	ExcludedGPUs []string     `json:"excludedDisplayDevices,omitempty"`
	Ports        []PortResult `json:"ports"`
	Stale        []string     `json:"stale,omitempty"`
	Invalid      []string     `json:"invalid,omitempty"`
	Unknown      []string     `json:"unknown,omitempty"`
	Facts        []PortFacts  `json:"facts,omitempty"`
	Binds        []BindRecord `json:"binds,omitempty"`
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeAttributes writes roles by PCI address as an attribute file for this
// boot, and checks it with the plugin's own loader before it replaces
// anything.
func writeAttributes(path, bootID string, roles map[string]string, reserved func(resourceapi.QualifiedName) bool) error {
	f := inventory.ExtraFile{
		Version: inventory.ExtraVersion,
		BootID:  bootID,
		Devices: map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
	}
	for bdf, role := range roles {
		f.Devices[bdf] = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{RoleAttribute: {StringValue: ptr.To(role)}}
	}
	check := path + ".check"
	if err := writeJSON(check, f); err != nil {
		return err
	}
	if _, err := inventory.LoadExtra(check, bootID, reserved); err != nil {
		os.Remove(check)
		return fmt.Errorf("attribute file failed validation: %w", err)
	}
	return os.Rename(check, path)
}

func loadAuto(dir, bootID string) (autoFile, bool) {
	b, err := os.ReadFile(filepath.Join(dir, AutoFile))
	if err != nil {
		return autoFile{}, false
	}
	var f autoFile
	if json.Unmarshal(b, &f) != nil || f.BootID != bootID || f.Detector != Detector {
		return autoFile{}, false
	}
	return f, true
}

func loadBinds(dir, bootID string) ([]BindRecord, error) {
	b, err := os.ReadFile(filepath.Join(dir, BindFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f bindFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.BootID != bootID {
		return nil, nil
	}
	return f.Binds, nil
}

func saveBinds(dir, bootID string, binds []BindRecord) error {
	return writeJSON(filepath.Join(dir, BindFile), bindFile{BootID: bootID, Binds: binds})
}

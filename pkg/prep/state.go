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
	ReportFile    = "report.json"
	GeneratedFile = "attributes.generated.json" // read by the plugin
	ProposedFile  = "attributes.proposed.json"  // report level; for review only
	BindFile      = "binds.json"
)

// RoleAttribute is the attribute prep writes for a compute port.
const RoleAttribute resourceapi.QualifiedName = "example.com/fabric"

// Detector names the classifier and its version in generated files.
const Detector = "classifyHGX/v1"

// generatedFile is the attribute file schema with prep's header fields,
// which the plugin ignores.
type generatedFile struct {
	inventory.ExtraFile
	GeneratedBy string `json:"generatedBy"`
	Level       string `json:"level"`
	Detector    string `json:"detector"`
}

// BindRecord is a function prep moved to vfio-pci during one boot.
type BindRecord struct {
	BDF            string   `json:"bdf"`
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

// Report is everything a pass saw and decided, for people and alerts.
type Report struct {
	BootID       string       `json:"bootID"`
	Level        string       `json:"level"`
	Detector     string       `json:"detector"`
	Updated      string       `json:"updated"`
	Outcome      Outcome      `json:"outcome"`
	Frozen       bool         `json:"frozen,omitempty"`
	Retries      int          `json:"retries,omitempty"`
	GPUs         []GPUInfo    `json:"gpus"`
	ExcludedGPUs []string     `json:"excludedDisplayDevices,omitempty"`
	Ports        []PortFacts  `json:"ports"`
	Unprobed     []string     `json:"unprobed,omitempty"`
	Conflicts    []string     `json:"conflicts,omitempty"`
	BindNotes    []string     `json:"bindNotes,omitempty"`
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

// writeAttributes writes the compute roles as an attribute file, and checks
// the result with the plugin's own loader before it replaces anything.
func writeAttributes(path, bootID, level, version string, compute []string, reserved func(resourceapi.QualifiedName) bool) error {
	f := generatedFile{
		ExtraFile: inventory.ExtraFile{
			Version: inventory.ExtraVersion,
			BootID:  bootID,
			Devices: map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		},
		GeneratedBy: "fabric-nic-prep " + version,
		Level:       level,
		Detector:    Detector,
	}
	for _, bdf := range compute {
		f.Devices[bdf] = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{RoleAttribute: {StringValue: ptr.To("compute")}}
	}
	check := path + ".check"
	if err := writeJSON(check, f); err != nil {
		return err
	}
	if _, err := inventory.LoadExtra(check, bootID, reserved); err != nil {
		os.Remove(check)
		return fmt.Errorf("generated file failed validation: %w", err)
	}
	return os.Rename(check, path)
}

// frozenFor reports whether path holds a valid classification for this
// boot: it must pass the plugin's own loader and carry this detector.
func frozenFor(path, bootID string, reserved func(resourceapi.QualifiedName) bool) (map[string]bool, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var f generatedFile
	if json.Unmarshal(b, &f) != nil || f.BootID != bootID || f.Detector != Detector {
		return nil, false
	}
	devices, err := inventory.LoadExtra(path, bootID, reserved)
	if err != nil {
		return nil, false
	}
	compute := map[string]bool{}
	for bdf, attrs := range devices {
		if v := attrs[RoleAttribute].StringValue; v != nil && *v == "compute" {
			compute[bdf] = true
		}
	}
	return compute, true
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

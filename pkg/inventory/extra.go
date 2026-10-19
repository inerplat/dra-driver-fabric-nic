package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
)

// ExtraVersion is the only version of the attribute file this driver reads.
const ExtraVersion = "v1"

// ExtraFile is the attribute file another component writes for the driver to
// publish: attributes keyed by PCI address. The driver does not judge what
// they mean.
//
//	{
//	  "version": "v1",
//	  "bootID": "0f3c6a9e-...",
//	  "devices": {
//	    "0000:9c:00.0": {"example.com/fabric": {"string": "compute"}}
//	  }
//	}
type ExtraFile struct {
	Version string                                                               `json:"version"`
	BootID  string                                                               `json:"bootID,omitempty"`
	Devices map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute `json:"devices"`
}

// ErrStale marks a file written during another boot.
var ErrStale = errors.New("attribute file is from another boot")

// LoadExtra reads the attribute file at path. A missing file is not an error.
// A file whose bootID differs from bootID is rejected as a whole: attributes
// about devices can change across a reboot, and publishing old ones is worse
// than publishing none.
func LoadExtra(path, bootID string, reserved func(resourceapi.QualifiedName) bool) (map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f ExtraFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Version != ExtraVersion {
		return nil, fmt.Errorf("%s: version %q, want %q", path, f.Version, ExtraVersion)
	}
	if f.BootID != "" && f.BootID != bootID {
		return nil, fmt.Errorf("%s: %w (file %s, node %s)", path, ErrStale, f.BootID, bootID)
	}
	for bdf, attrs := range f.Devices {
		for name, value := range attrs {
			if !strings.Contains(string(name), "/") {
				return nil, fmt.Errorf("%s: %s: attribute %q has no domain", path, bdf, name)
			}
			if reserved(name) {
				return nil, fmt.Errorf("%s: %s: attribute %q is set by the driver", path, bdf, name)
			}
			if countValues(value) != 1 {
				return nil, fmt.Errorf("%s: %s: attribute %q must have exactly one value", path, bdf, name)
			}
		}
	}
	return f.Devices, nil
}

func countValues(v resourceapi.DeviceAttribute) int {
	n := 0
	if v.IntValue != nil {
		n++
	}
	if v.BoolValue != nil {
		n++
	}
	if v.StringValue != nil {
		n++
	}
	if v.VersionValue != nil {
		n++
	}
	return n
}

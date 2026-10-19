package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/blang/semver/v4"
	"k8s.io/apimachinery/pkg/util/validation"

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
			if err := validAttribute(name, value); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, bdf, err)
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

// Sources of a device's file attributes, published as the roleSource
// attribute so a DeviceClass can tell written attributes from generated ones.
const (
	SourceFile = "file" // the first file: declared roles
	SourceAuto = "auto" // a later file: detected roles
	SourceNone = "none"
)

// FileState is the outcome of loading one attribute file.
type FileState struct {
	Path  string
	State string // ok, missing, stale or invalid
	Err   error
}

// Merged is the attributes of every listed file, with the file each device's
// entry came from.
type Merged struct {
	Attrs  map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	Source map[string]string
	Files  []FileState
}

// LoadAll loads the files in order. The first holds declared roles; every later
// file is generated and must carry a bootID. An earlier file wins for a whole
// device: once a file names a device, later files are not consulted for it.
// A file that fails to load is skipped as a whole and reported in Files.
func LoadAll(paths []string, bootID string, reserved func(resourceapi.QualifiedName) bool) Merged {
	m := Merged{
		Attrs:  map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		Source: map[string]string{},
	}
	for i, path := range paths {
		state := FileState{Path: path, State: "ok"}
		devices, err := LoadExtra(path, bootID, reserved)
		switch {
		case errors.Is(err, ErrStale):
			state.State, state.Err = "stale", err
		case err != nil:
			state.State, state.Err = "invalid", err
		case devices == nil:
			if _, statErr := os.Stat(path); statErr != nil {
				state.State = "missing"
			}
		}
		if err == nil && i > 0 && devices != nil {
			if id, _ := fileBootID(path); id == "" {
				state.State, state.Err = "invalid", fmt.Errorf("%s: a generated file must carry a bootID", path)
				devices = nil
			}
		}
		m.Files = append(m.Files, state)
		source := SourceFile
		if i > 0 {
			source = SourceAuto
		}
		for bdf, attrs := range devices {
			if _, seen := m.Attrs[bdf]; seen {
				continue
			}
			m.Attrs[bdf] = attrs
			m.Source[bdf] = source
		}
	}
	return m
}

func fileBootID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var f ExtraFile
	if err := json.Unmarshal(b, &f); err != nil {
		return "", err
	}
	return f.BootID, nil
}

var cIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validAttribute applies the ResourceSlice rules a published attribute must
// meet, so that one bad entry rejects its file instead of the whole slice.
func validAttribute(name resourceapi.QualifiedName, v resourceapi.DeviceAttribute) error {
	domain, id, _ := strings.Cut(string(name), "/")
	if errs := validation.IsDNS1123Subdomain(domain); len(errs) > 0 || len(domain) > 63 {
		return fmt.Errorf("attribute %q: invalid domain %q", name, domain)
	}
	if len(id) > 32 || !cIdentifier.MatchString(id) {
		return fmt.Errorf("attribute %q: name must be a C identifier of at most 32 characters", name)
	}
	if v.StringValue != nil && len(*v.StringValue) > 64 {
		return fmt.Errorf("attribute %q: string longer than 64 bytes", name)
	}
	if v.VersionValue != nil {
		if _, err := semver.Parse(*v.VersionValue); err != nil || len(*v.VersionValue) > 64 {
			return fmt.Errorf("attribute %q: %q is not a semantic version", name, *v.VersionValue)
		}
	}
	return nil
}

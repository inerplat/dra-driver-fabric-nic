package prep

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Modes the node label selects.
const (
	ModeAuto     = "auto"
	ModeExplicit = "explicit"
	ModeReport   = "report"
	ModeOff      = "off"
)

// Bind intents of a port declaration.
const (
	BindVFIO = "vfio-pci" // bind to vfio-pci at every pod start
	BindKeep = "keep"     // leave the driver as it is
)

// Decl is what a person declared for one port.
type Decl struct {
	Role string `json:"role"`
	Bind string `json:"bind"`
}

// Intent is the node's labels and annotations under the driver's prefix,
// parsed. prep never writes them.
type Intent struct {
	Node    string          `json:"node"`
	BootID  string          `json:"bootID"`
	Mode    string          `json:"mode"`
	ModeErr string          `json:"modeError,omitempty"`
	Ports   map[string]Decl `json:"ports,omitempty"` // by port ID
	Invalid []string        `json:"invalid,omitempty"`
	Unknown []string        `json:"unknown,omitempty"`
}

var (
	portKeyRe   = regexp.MustCompile(`^port\.([A-Za-z0-9]{1,48}-f[0-7])$`)
	portValueRe = regexp.MustCompile(`^(compute|storage|other)/(vfio-pci|keep)$`)
)

// ParseIntent reads the mode label and the port annotations. The mode label
// falls back to defaultMode only when it is absent.
func ParseIntent(prefix string, labels, annotations map[string]string, defaultMode string) Intent {
	in := Intent{Mode: defaultMode, Ports: map[string]Decl{}}
	p := prefix + "/"
	if v, ok := labels[p+"mode"]; ok {
		in.Mode = v
	}
	switch in.Mode {
	case ModeAuto, ModeExplicit, ModeReport, ModeOff:
	default:
		in.ModeErr = fmt.Sprintf("label %smode is %q; want auto, explicit, report or off", p, in.Mode)
	}
	for k := range labels {
		if name, ok := strings.CutPrefix(k, p); ok && name != "mode" && name != "enabled" {
			in.Unknown = append(in.Unknown, "label "+k)
		}
	}
	for k, v := range annotations {
		name, ok := strings.CutPrefix(k, p)
		if !ok {
			continue
		}
		if !strings.HasPrefix(name, "port.") {
			in.Unknown = append(in.Unknown, "annotation "+k)
			continue
		}
		km, vm := portKeyRe.FindStringSubmatch(name), portValueRe.FindStringSubmatch(v)
		if km == nil || vm == nil {
			in.Invalid = append(in.Invalid, fmt.Sprintf("annotation %s=%q: want %sport.<serial>-f<function>=<compute|storage|other>/<vfio-pci|keep>", k, v, p))
			continue
		}
		in.Ports[km[1]] = Decl{Role: vm[1], Bind: vm[2]}
	}
	sort.Strings(in.Unknown)
	sort.Strings(in.Invalid)
	return in
}

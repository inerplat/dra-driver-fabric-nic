package prep

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// PrintReport writes the run as a table, one row per port, followed by the
// commands that declare what this run saw. Values the run could not decide
// are left as comments for a person to fill in.
func PrintReport(w io.Writer, rep Report, prefix, namespace, podLabel string) {
	fmt.Fprintf(w, "fabric-nic-prep: node %s, mode %s, boot %s\n", rep.Node, rep.Mode, rep.BootID)
	if rep.Fatal != "" {
		fmt.Fprintf(w, "NOTHING BOUND: %s\n", rep.Fatal)
	}
	if rep.Auto != nil {
		fmt.Fprintf(w, "detection: %s %s\n", rep.Auto.Result, rep.Auto.Reason)
	}
	for _, s := range rep.Invalid {
		fmt.Fprintf(w, "invalid: %s\n", s)
	}
	for _, s := range rep.Stale {
		fmt.Fprintf(w, "stale: %sport.%s names no function on this node\n", prefix+"/", s)
	}
	for _, s := range rep.Unknown {
		fmt.Fprintf(w, "unknown key ignored: %s\n", s)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT-ID\tBDF\tDRIVER\tROOT\tGPU\tNVLINK-MGMT\tFABRIC\tROLE\tSOURCE\tRESULT\tREASON")
	for _, p := range rep.Ports {
		role := "-"
		if p.Role != "" {
			role = p.Role + "/" + p.Bind
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\t%s\t%s\t%s\t%s\t%s\n",
			dash(p.ID), p.BDF, dash(p.Driver), dash(p.Root), dash(p.GPU), p.NVLinkManagement, dash(p.Fabric), role, dash(p.Source), p.Result, p.Reason)
	}
	tw.Flush()

	var lines, todo []string
	for _, p := range rep.Ports {
		if p.ID == "" {
			continue
		}
		key := prefix + "/port." + p.ID
		switch {
		case p.Role != "":
			lines = append(lines, key+"="+p.Role+"/"+p.Bind)
		case p.NVLinkManagement:
			lines = append(lines, key+"=other/keep")
		default:
			todo = append(todo, "#   "+key+"=<compute|storage|other>/<vfio-pci|keep>")
		}
	}
	for _, id := range rep.Stale {
		lines = append(lines, prefix+"/port."+id+"-")
	}
	if len(lines) == 0 && len(todo) == 0 {
		return
	}
	fmt.Fprintln(w, "\nTo declare these ports (review each value first):")
	if len(lines) > 0 {
		fmt.Fprintf(w, "kubectl annotate node %s --overwrite \\\n  %s\n", rep.Node, strings.Join(lines, " \\\n  "))
	}
	if len(todo) > 0 {
		fmt.Fprintln(w, "# Not decided by this run; add to the command above as needed:")
		fmt.Fprintln(w, strings.Join(todo, "\n"))
	}
	fmt.Fprintf(w, "kubectl label node %s --overwrite %s/mode=explicit\n", rep.Node, prefix)
	fmt.Fprintf(w, "kubectl -n %s delete pod -l %s --field-selector spec.nodeName=%s\n", namespace, podLabel, rep.Node)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

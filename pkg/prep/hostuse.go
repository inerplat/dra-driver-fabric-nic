package prep

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// HostUse decides whether the host is using a port, which makes it
// ineligible for binding. A check that cannot run counts as in use.
type HostUse interface {
	InUse(ctx context.Context, p PortFacts) (bool, string)
}

// SystemHostUse checks the host through sysfs, procfs, netlink and the rdma
// and lnetctl tools. It needs the host's network and PID namespaces.
type SystemHostUse struct {
	SysRoot  string
	ProcRoot string
	SelfPID  int
}

func (h SystemHostUse) InUse(ctx context.Context, p PortFacts) (bool, string) {
	stacked, err := h.netdevsWithChildren(p.Netdevs)
	if err != nil {
		return true, fmt.Sprintf("cannot find interfaces stacked on %v: %v", p.Netdevs, err)
	}
	for _, nd := range stacked {
		if used, why := h.netdevInUse(nd); used {
			return true, why
		}
	}
	if used, why := h.lnetInUse(ctx, p.Netdevs); used {
		return true, why
	}
	if p.IBDev != "" {
		if used, why := rdmaUserQPs(ctx, p.IBDev); used {
			return true, why
		}
		if used, why := h.openRDMANodes(p.IBDev); used {
			return true, why
		}
	}
	return false, ""
}

// netdevsWithChildren adds every interface stacked on the given ones, found
// through the kernel's links rather than names: a P_Key or VLAN child names
// its parent in iflink, and a bond or bridge appears as an upper_ link. It
// recurses, so a VLAN on a P_Key child is found too.
func (h SystemHostUse) netdevsWithChildren(netdevs []string) ([]string, error) {
	netDir := filepath.Join(h.SysRoot, "class", "net")
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return nil, err
	}
	ifindex := map[string]string{}
	iflink := map[string]string{}
	for _, e := range entries {
		// Interfaces are directories (symlinks to them in sysfs); files such
		// as bonding_masters are not.
		if st, err := os.Stat(filepath.Join(netDir, e.Name())); err != nil || !st.IsDir() {
			continue
		}
		idx, err := os.ReadFile(filepath.Join(netDir, e.Name(), "ifindex"))
		if err != nil {
			return nil, err
		}
		link, err := os.ReadFile(filepath.Join(netDir, e.Name(), "iflink"))
		if err != nil {
			return nil, err
		}
		ifindex[e.Name()] = strings.TrimSpace(string(idx))
		iflink[e.Name()] = strings.TrimSpace(string(link))
	}
	seen := map[string]bool{}
	queue := append([]string(nil), netdevs...)
	for len(queue) > 0 {
		nd := queue[0]
		queue = queue[1:]
		if seen[nd] {
			continue
		}
		seen[nd] = true
		for name := range ifindex {
			if name != nd && ifindex[nd] != "" && iflink[name] == ifindex[nd] && ifindex[name] != iflink[name] {
				queue = append(queue, name)
			}
		}
		uppers, err := filepath.Glob(filepath.Join(netDir, nd, "upper_*"))
		if err != nil {
			return nil, err
		}
		for _, u := range uppers {
			queue = append(queue, strings.TrimPrefix(filepath.Base(u), "upper_"))
		}
	}
	out := make([]string, 0, len(seen))
	for nd := range seen {
		out = append(out, nd)
	}
	return out, nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h SystemHostUse) netdevInUse(nd string) (bool, string) {
	if _, err := os.Lstat(filepath.Join(h.SysRoot, "class", "net", nd, "master")); err == nil {
		return true, nd + " is a bond or bridge member"
	}
	iface, err := net.InterfaceByName(nd)
	if err != nil {
		return true, fmt.Sprintf("cannot inspect %s: %v", nd, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return true, fmt.Sprintf("cannot list addresses of %s: %v", nd, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if ok && ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		return true, fmt.Sprintf("%s has address %s", nd, a.String())
	}
	for _, r := range []struct {
		file string
		via  func(string, string) (bool, error)
	}{{"route", defaultRouteVia}, {"ipv6_route", defaultRoute6Via}} {
		via, err := r.via(filepath.Join(h.ProcRoot, "net", r.file), nd)
		if err != nil {
			return true, fmt.Sprintf("cannot read routes: %v", err)
		}
		if via {
			return true, "default route through " + nd
		}
	}
	return false, ""
}

// defaultRouteVia reads /proc/net/route for an IPv4 default route through nd.
func defaultRouteVia(path, nd string) (bool, error) {
	return scanRoutes(path, func(fields []string) bool {
		return len(fields) > 2 && fields[0] == nd && fields[1] == "00000000"
	})
}

// defaultRoute6Via reads /proc/net/ipv6_route for a default route (::/0)
// through nd.
func defaultRoute6Via(path, nd string) (bool, error) {
	return scanRoutes(path, func(fields []string) bool {
		return len(fields) >= 10 && fields[9] == nd && fields[0] == strings.Repeat("0", 32) && fields[1] == "00"
	})
}

func scanRoutes(path string, match func([]string) bool) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if match(strings.Fields(s.Text())) {
			return true, nil
		}
	}
	return false, s.Err()
}

func (h SystemHostUse) lnetInUse(ctx context.Context, netdevs []string) (bool, string) {
	if _, err := os.Stat(filepath.Join(h.SysRoot, "module", "lnet")); err != nil {
		return false, ""
	}
	out, err := exec.CommandContext(ctx, "lnetctl", "net", "show").CombinedOutput()
	if err != nil {
		return true, fmt.Sprintf("lnet is loaded and lnetctl failed: %v", err)
	}
	for _, nd := range netdevs {
		if strings.Contains(string(out), nd) {
			return true, "LNet uses " + nd
		}
	}
	return false, ""
}

// rdmaUserQPs reports queue pairs owned by a process. Kernel QPs, such as
// the management QPs and IPoIB's, carry no pid.
func rdmaUserQPs(ctx context.Context, ibdev string) (bool, string) {
	out, err := exec.CommandContext(ctx, "rdma", "-j", "resource", "show", "qp").Output()
	if err != nil {
		return true, fmt.Sprintf("cannot list RDMA resources of %s: %v", ibdev, err)
	}
	return userQPs(out, ibdev)
}

func userQPs(out []byte, ibdev string) (bool, string) {
	var qps []map[string]any
	if err := json.Unmarshal(out, &qps); err != nil {
		return true, fmt.Sprintf("cannot parse RDMA resources of %s: %v", ibdev, err)
	}
	for _, qp := range qps {
		if qp["ifname"] != ibdev {
			continue
		}
		if pid, ok := qp["pid"]; ok {
			return true, fmt.Sprintf("%s has a queue pair owned by pid %v (%v)", ibdev, pid, qp["comm"])
		}
	}
	return false, ""
}

// openRDMANodes reports a process other than prep holding one of the
// port's umad, issm or uverbs character devices, such as a host subnet
// manager or a verbs user. Devices are compared by number, so a node opened
// through another path is still found. Anything it cannot inspect counts as
// use.
func (h SystemHostUse) openRDMANodes(ibdev string) (bool, string) {
	devs := map[uint64]string{}
	for _, class := range []string{"infiniband_mad", "infiniband_verbs"} {
		dir := filepath.Join(h.SysRoot, "class", class)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return true, fmt.Sprintf("cannot read %s: %v", dir, err)
		}
		for _, e := range entries {
			owner := readTrim(filepath.Join(dir, e.Name(), "ibdev"))
			if owner == "" {
				continue // abi_version and other non-device entries
			}
			if owner != ibdev {
				continue
			}
			majmin := readTrim(filepath.Join(dir, e.Name(), "dev"))
			maj, min, ok := strings.Cut(majmin, ":")
			if !ok {
				return true, fmt.Sprintf("cannot read the device number of %s", e.Name())
			}
			ma, _ := strconv.ParseUint(maj, 10, 32)
			mi, _ := strconv.ParseUint(min, 10, 32)
			devs[unix.Mkdev(uint32(ma), uint32(mi))] = e.Name()
		}
	}
	if len(devs) == 0 {
		return false, ""
	}
	procs, err := os.ReadDir(h.ProcRoot)
	if err != nil {
		return true, fmt.Sprintf("cannot scan processes: %v", err)
	}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid == h.SelfPID {
			continue
		}
		fdDir := filepath.Join(h.ProcRoot, p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // the process exited
			}
			return true, fmt.Sprintf("cannot read %s: %v", fdDir, err)
		}
		for _, fd := range fds {
			var st unix.Stat_t
			if err := unix.Stat(filepath.Join(fdDir, fd.Name()), &st); err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue // the fd was closed meanwhile
				}
				return true, fmt.Sprintf("cannot inspect %s/%s: %v", fdDir, fd.Name(), err)
			}
			if st.Mode&unix.S_IFMT != unix.S_IFCHR {
				continue
			}
			if name, ok := devs[st.Rdev]; ok {
				return true, fmt.Sprintf("pid %d holds %s", pid, name)
			}
		}
	}
	return false, ""
}

// Package preflight checks the operating-system limits that cap how much
// load one machine can generate.
package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Item is one checked limit.
type Item struct {
	Name   string
	Value  string
	OK     bool
	Advice string
}

// Check inspects this machine. conns is the number of concurrent
// connections the test is expected to need (0 if unknown).
func Check(conns int) []Item {
	var out []Item
	out = append(out, Item{Name: "CPU cores", Value: strconv.Itoa(runtime.NumCPU()), OK: true})

	if soft, hard, err := fileLimit(); err == nil {
		need := uint64(conns) + 256
		it := Item{Name: "open files (ulimit -n)", Value: fmt.Sprintf("%d (hard %d)", soft, hard), OK: soft >= need && soft >= 10000}
		if !it.OK {
			it.Advice = "each connection is a file; raise it with `ulimit -n 65535` (Linux: also /etc/security/limits.conf)"
		}
		out = append(out, it)
	}

	if lo, hi, ok := portRange(); ok {
		n := hi - lo + 1
		// With keep-alive each connection holds one port, so the range only
		// matters when it is close to the connection count.
		it := Item{Name: "ephemeral ports", Value: fmt.Sprintf("%d-%d (%d ports)", lo, hi, n), OK: n >= 10000 && n >= conns*2}
		if !it.OK {
			if runtime.GOOS == "darwin" {
				it.Advice = "widen with `sudo sysctl -w net.inet.ip.portrange.first=10000`"
			} else {
				it.Advice = "widen with `sudo sysctl -w net.ipv4.ip_local_port_range=\"10000 65535\"`"
			}
		}
		out = append(out, it)
	}

	if runtime.GOOS == "linux" {
		if v, ok := readInt("/proc/sys/net/ipv4/tcp_tw_reuse"); ok {
			it := Item{Name: "tcp_tw_reuse", Value: strconv.Itoa(v), OK: v >= 1}
			if !it.OK {
				it.Advice = "short-lived connections pile up in TIME_WAIT; `sudo sysctl -w net.ipv4.tcp_tw_reuse=1`"
			}
			out = append(out, it)
		}
		if v, ok := readInt("/proc/sys/net/core/somaxconn"); ok {
			out = append(out, Item{Name: "somaxconn (if testing a local server)", Value: strconv.Itoa(v), OK: v >= 1024,
				Advice: adviceIf(v < 1024, "`sudo sysctl -w net.core.somaxconn=4096`")})
		}
	}
	return out
}

func adviceIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

func readInt(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return v, err == nil
}

func portRange() (int, int, bool) {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
		if err != nil {
			return 0, 0, false
		}
		f := strings.Fields(string(b))
		if len(f) != 2 {
			return 0, 0, false
		}
		lo, e1 := strconv.Atoi(f[0])
		hi, e2 := strconv.Atoi(f[1])
		return lo, hi, e1 == nil && e2 == nil
	case "darwin", "freebsd":
		lo, e1 := sysctlInt("net.inet.ip.portrange.first")
		hi, e2 := sysctlInt("net.inet.ip.portrange.last")
		return lo, hi, e1 == nil && e2 == nil
	}
	return 0, 0, false
}

func sysctlInt(name string) (int, error) {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

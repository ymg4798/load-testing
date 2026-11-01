package monitor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procUsage is the summed CPU time and memory of matching processes.
type procUsage struct {
	cpu   time.Duration
	rss   int64 // bytes
	count int
}

// readProcess finds processes by pid or by name and sums their usage.
// It reads /proc on Linux and asks ps elsewhere.
func readProcess(ctx context.Context, which string) (procUsage, error) {
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		return readProc(which)
	}
	return readPS(ctx, which)
}

func readPS(ctx context.Context, which string) (procUsage, error) {
	var u procUsage
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,rss=,time=,comm=").Output()
	if err != nil {
		return u, fmt.Errorf("ps: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		comm := strings.Join(f[3:], " ")
		if f[0] != which && filepath.Base(comm) != which {
			continue
		}
		rss, _ := strconv.ParseInt(f[1], 10, 64)
		cpu, err := parseCPUTime(f[2])
		if err != nil {
			continue
		}
		u.rss += rss * 1024
		u.cpu += cpu
		u.count++
	}
	if u.count == 0 {
		return u, fmt.Errorf("no process %q", which)
	}
	return u, nil
}

// parseCPUTime reads ps TIME: [DD-][HH:]MM:SS[.ss].
func parseCPUTime(s string) (time.Duration, error) {
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil {
			return 0, err
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	var sec float64
	for _, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, err
		}
		sec = sec*60 + n
	}
	return time.Duration((days*86400 + sec) * float64(time.Second)), nil
}

func readProc(which string) (procUsage, error) {
	var u procUsage
	var pids []string
	if _, err := strconv.Atoi(which); err == nil {
		pids = []string{which}
	} else {
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			b, err := os.ReadFile("/proc/" + e.Name() + "/comm")
			if err == nil && strings.TrimSpace(string(b)) == which {
				pids = append(pids, e.Name())
			}
		}
	}
	page := int64(os.Getpagesize())
	for _, pid := range pids {
		b, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			continue
		}
		// Fields after the ")" that ends the command name.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 22 {
			continue
		}
		ut, _ := strconv.ParseInt(f[11], 10, 64) // utime, field 14
		st, _ := strconv.ParseInt(f[12], 10, 64) // stime, field 15
		rss, _ := strconv.ParseInt(f[21], 10, 64)
		const ticks = 100 // USER_HZ on practically every Linux
		u.cpu += time.Duration(ut+st) * time.Second / ticks
		u.rss += rss * page
		u.count++
	}
	if u.count == 0 {
		return u, fmt.Errorf("no process %q", which)
	}
	return u, nil
}

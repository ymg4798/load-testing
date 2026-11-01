//go:build unix

package preflight

import "syscall"

// fileLimit returns the open-file limit. The Go runtime already raises the
// soft limit to the hard limit at startup on most systems.
func fileLimit() (soft, hard uint64, err error) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, 0, err
	}
	return uint64(r.Cur), uint64(r.Max), nil
}

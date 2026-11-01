//go:build unix

package main

import (
	"syscall"
	"time"
)

// processCPU is user + system CPU time of this process.
func processCPU() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	d := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	return d.Seconds()
}

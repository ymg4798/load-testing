//go:build !unix

package metrics

import "time"

func processCPU() time.Duration { return 0 }

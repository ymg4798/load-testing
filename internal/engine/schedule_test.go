package engine

import (
	"math"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/scenario"
	"github.com/ymg4798/lt/internal/yamlx"
)

func stage(d time.Duration, target float64) scenario.Stage {
	return scenario.Stage{Duration: yamlx.Duration(d), Target: target}
}

// count arrivals the way runArrival does: arrival k is due at TimeOf(k).
func countArrivals(s *schedule) int {
	n := 0
	for k := 0.0; ; k++ {
		at, ok := s.TimeOf(k)
		if !ok || at >= s.total {
			return n
		}
		n++
	}
}

func TestScheduleConstant(t *testing.T) {
	s := newSchedule(100, []scenario.Stage{stage(10*time.Second, 100)})
	if got := countArrivals(s); got != 1000 {
		t.Fatalf("arrivals = %d, want 1000", got)
	}
	s = newSchedule(100, []scenario.Stage{stage(10*time.Second, 100)})
	at, _ := s.TimeOf(50)
	if math.Abs(at-0.5) > 1e-9 {
		t.Fatalf("50th arrival at %v, want 0.5s", at)
	}
}

func TestScheduleRamp(t *testing.T) {
	// 0→100/s over 10s is 500 arrivals, then 100→0 over 10s another 500.
	s := newSchedule(0, []scenario.Stage{stage(10*time.Second, 100), stage(10*time.Second, 0)})
	got := countArrivals(s)
	if got < 999 || got > 1001 {
		t.Fatalf("arrivals = %d, want ~1000", got)
	}
	if r := s.At(5); math.Abs(r-50) > 1e-9 {
		t.Fatalf("rate at 5s = %v, want 50", r)
	}
	if r := s.At(15); math.Abs(r-50) > 1e-9 {
		t.Fatalf("rate at 15s = %v, want 50", r)
	}
}

func TestSchedulePause(t *testing.T) {
	s := newSchedule(10, []scenario.Stage{stage(time.Second, 10), stage(time.Second, 0), stage(time.Second, 0), stage(time.Second, 10)})
	got := countArrivals(s)
	if got < 19 || got > 21 { // 10 + 5 (ramp down) + 0 + 5 (ramp up)
		t.Fatalf("arrivals = %d, want ~20", got)
	}
}

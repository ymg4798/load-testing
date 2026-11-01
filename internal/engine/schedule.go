package engine

import (
	"math"

	"github.com/ymg4798/lt/internal/scenario"
)

// schedule is a piecewise-linear target over time (rate or user count).
type schedule struct {
	segs  []segment
	total float64 // seconds
	pos   int     // cached segment index for monotonic TimeOf calls
}

type segment struct {
	start, dur float64 // seconds
	v0, v1     float64 // target at segment start and end
	n0         float64 // integral of the target before this segment
}

func newSchedule(start float64, stages []scenario.Stage) *schedule {
	s := &schedule{}
	v, t, n := start, 0.0, 0.0
	for _, st := range stages {
		d := st.Duration.D().Seconds()
		seg := segment{start: t, dur: d, v0: v, v1: st.Target, n0: n}
		s.segs = append(s.segs, seg)
		n += (v + st.Target) / 2 * d
		t += d
		v = st.Target
	}
	s.total = t
	return s
}

// At returns the target at time t (seconds).
func (s *schedule) At(t float64) float64 {
	if t < 0 || len(s.segs) == 0 {
		return 0
	}
	for _, g := range s.segs {
		if t <= g.start+g.dur {
			return g.v0 + (g.v1-g.v0)*(t-g.start)/g.dur
		}
	}
	return 0
}

// TimeOf returns when the integral of the target reaches n, i.e. when the
// n-th arrival is due. It must be called with non-decreasing n.
func (s *schedule) TimeOf(n float64) (float64, bool) {
	for s.pos < len(s.segs) {
		g := s.segs[s.pos]
		area := (g.v0 + g.v1) / 2 * g.dur
		if n <= g.n0+area {
			need := n - g.n0
			if need <= 0 {
				return g.start, true
			}
			a := (g.v1 - g.v0) / (2 * g.dur)
			b := g.v0
			var x float64
			if math.Abs(a) < 1e-12 {
				if b == 0 {
					return 0, false
				}
				x = need / b
			} else {
				x = (-b + math.Sqrt(b*b+4*a*need)) / (2 * a)
			}
			if x < 0 {
				x = 0
			}
			return g.start + x, true
		}
		s.pos++
	}
	return 0, false
}

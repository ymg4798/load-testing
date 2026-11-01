// Package builtin holds steps that are not protocols:
//
//   - sleep: 500ms          # fixed pause
//   - sleep: 500ms..2s      # random pause in a range
//   - set: { page: "{{ randInt(1, 10) }}" }
package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ymg4798/load-testing/internal/protocol"
	"github.com/ymg4798/load-testing/internal/tmpl"
	"github.com/ymg4798/load-testing/internal/yamlx"
)

func init() {
	protocol.Register("sleep", compileSleep)
	protocol.Register("set", compileSet)
}

type sleepStep struct{ min, max time.Duration }

func compileSleep(raw any, _ *protocol.Env) (protocol.Step, error) {
	s := strings.TrimSpace(tmpl.ToString(raw))
	lo, hi, isRange := strings.Cut(s, "..")
	a, err := yamlx.ParseDuration(lo)
	if err != nil {
		return nil, fmt.Errorf("sleep: %w", err)
	}
	b := a
	if isRange {
		if b, err = yamlx.ParseDuration(hi); err != nil {
			return nil, fmt.Errorf("sleep: %w", err)
		}
		if b < a {
			return nil, fmt.Errorf("sleep: range end is before start")
		}
	}
	return &sleepStep{a, b}, nil
}

func (s *sleepStep) Run(ctx context.Context, vu *protocol.VU) error {
	d := s.min
	if s.max > s.min {
		d += time.Duration(vu.Rand.Int64N(int64(s.max - s.min)))
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type setStep struct{ vals map[string]tmpl.Value }

func compileSet(raw any, _ *protocol.Env) (protocol.Step, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("set: expected a map of variable: value")
	}
	s := &setStep{vals: map[string]tmpl.Value{}}
	for k, v := range m {
		c, err := tmpl.CompileValue(v)
		if err != nil {
			return nil, fmt.Errorf("set %s: %w", k, err)
		}
		s.vals[k] = c
	}
	return s, nil
}

func (s *setStep) Run(_ context.Context, vu *protocol.VU) error {
	for k, v := range s.vals {
		r, err := v.Render(vu)
		if err != nil {
			return protocol.Fail("set", "template", err.Error())
		}
		vu.Vars[k] = r
	}
	return nil
}

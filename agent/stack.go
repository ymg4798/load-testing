package agent

import (
	"bufio"
	"bytes"
	"runtime/pprof"
	"strconv"
	"strings"
)

// Active stacks: the middleware runs each request with a pprof goroutine
// label lt_tx=<span id>. Once a second, and only while some request has
// run longer than Config.StackAfter, the agent takes the goroutine profile
// (debug=1 prints each group of identical stacks with its labels) and
// keeps the stacks of those requests. Nothing is captured while every
// request is fast; the label costs one small allocation per request.

const (
	labelKey       = "lt_tx"
	maxStackBytes  = 8 << 10 // per stack
	maxStacksPerRp = 20      // stacks per active report
)

const agentPkg = "github.com/ymg4798/lt/agent."

// captureStacks returns the current stack of each goroutine labelled with
// one of the wanted transaction labels. buf is reused between calls.
func captureStacks(buf *bytes.Buffer, want map[string]bool) map[string]string {
	buf.Reset()
	if err := pprof.Lookup("goroutine").WriteTo(buf, 1); err != nil {
		return nil
	}
	return parseGoroutineProfile(buf.Bytes(), want)
}

// parseGoroutineProfile reads a debug=1 goroutine profile:
//
//	goroutine profile: total 7
//	1 @ 0x1004 0x1008
//	# labels: {"lt_tx":"00f067aa0ba902b7"}
//	#	0x1003	main.chargeCard+0x33	/src/main.go:12
//	...
//	(blank line)
//
// and returns, per wanted label value, the stack of that transaction,
// trimmed (see trimFrames). When a transaction has several goroutines,
// the one running the HTTP handler wins.
func parseGoroutineProfile(b []byte, want map[string]bool) map[string]string {
	out := map[string]string{}
	handler := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var (
		id     string // label value of the current record, if wanted
		frames []frame
	)
	finish := func() {
		if id != "" && len(frames) > 0 {
			isHandler := hasMiddleware(frames)
			if _, ok := out[id]; !ok || (isHandler && !handler[id]) {
				out[id] = formatStack(trimFrames(frames))
				handler[id] = isHandler
			}
		}
		id, frames = "", frames[:0]
	}
	labelsPrefix, framePrefix, key := []byte("# labels: "), []byte("#\t"), []byte(`"`+labelKey+`"`)
	for sc.Scan() {
		line := sc.Bytes() // only lines of wanted records become strings
		switch {
		case len(line) == 0:
			finish()
		case bytes.HasPrefix(line, labelsPrefix):
			if !bytes.Contains(line, key) {
				continue
			}
			if v := parseLabels(string(line[len(labelsPrefix):]))[labelKey]; want[v] {
				id = v
			}
		case bytes.HasPrefix(line, framePrefix):
			if id == "" {
				continue
			}
			if f, ok := parseFrame(string(line)); ok {
				frames = append(frames, f)
			}
		}
	}
	finish()
	return out
}

// parseLabels reads pprof's label set text: {"k":"v", "k2":"v2"}.
func parseLabels(s string) map[string]string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	out := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ,")
		if s == "" {
			return out
		}
		kq, err := strconv.QuotedPrefix(s)
		if err != nil {
			return out
		}
		s = strings.TrimPrefix(s[len(kq):], ":")
		vq, err := strconv.QuotedPrefix(s)
		if err != nil {
			return out
		}
		s = s[len(vq):]
		k, err1 := strconv.Unquote(kq)
		v, err2 := strconv.Unquote(vq)
		if err1 == nil && err2 == nil {
			out[k] = v
		}
	}
}

type frame struct{ fn, loc string }

// parseFrame reads "#\t0x1003\tmain.chargeCard+0x33\t\t/src/main.go:12"
// (the columns are aligned with runs of tabs).
func parseFrame(line string) (frame, bool) {
	p := strings.SplitN(strings.TrimLeft(line[1:], "\t"), "\t", 2) // pc, rest
	if len(p) < 2 {
		return frame{}, false
	}
	fn, loc, ok := strings.Cut(strings.TrimLeft(p[1], "\t"), "\t")
	if !ok {
		return frame{}, false
	}
	if i := strings.LastIndex(fn, "+0x"); i > 0 {
		fn = fn[:i]
	}
	return frame{fn: fn, loc: strings.TrimLeft(loc, "\t")}, true
}

func hasMiddleware(fs []frame) bool {
	for _, f := range fs {
		if strings.HasPrefix(f.fn, agentPkg+"Middleware") {
			return true
		}
	}
	return false
}

// trimFrames drops what does not help find the slow code: runtime and
// profiler frames, the agent's own wrappers (not its tests), net/http's
// request dispatch, and any net/http server frames under the handler.
func trimFrames(fs []frame) []frame {
	keep := make([]frame, 0, len(fs))
	for _, f := range fs {
		if isNoise(f) {
			continue
		}
		keep = append(keep, f)
	}
	for len(keep) > 0 && strings.HasPrefix(keep[len(keep)-1].fn, "net/http.") {
		keep = keep[:len(keep)-1]
	}
	if len(keep) == 0 {
		return fs
	}
	return keep
}

// httpDispatch are net/http frames that only pass a request along.
var httpDispatch = map[string]bool{
	"net/http.HandlerFunc.ServeHTTP":       true,
	"net/http.(*ServeMux).ServeHTTP":       true,
	"net/http.serverHandler.ServeHTTP":     true,
	"net/http.(*conn).serve":               true,
	"net/http.(*timeoutHandler).ServeHTTP": true,
}

func isNoise(f frame) bool {
	switch {
	case strings.HasPrefix(f.fn, "runtime."),
		strings.HasPrefix(f.fn, "internal/runtime/"),
		strings.HasPrefix(f.fn, "runtime/pprof."),
		strings.HasPrefix(f.fn, "runtime/debug."):
		return true
	case httpDispatch[f.fn]:
		return true
	case strings.HasPrefix(f.fn, agentPkg):
		file, _, _ := strings.Cut(f.loc, ":")
		return !strings.HasSuffix(file, "_test.go")
	}
	return false
}

// formatStack writes frames like a Go traceback ("func\n\tfile:line\n"),
// cut at a frame boundary to maxStackBytes.
func formatStack(fs []frame) string {
	var b strings.Builder
	for i, f := range fs {
		n := len(f.fn) + len(f.loc) + 3
		if b.Len()+n > maxStackBytes-16 {
			b.WriteString("...")
			b.WriteString(strconv.Itoa(len(fs) - i))
			b.WriteString(" more\n")
			break
		}
		b.WriteString(f.fn)
		b.WriteString("\n\t")
		b.WriteString(f.loc)
		b.WriteByte('\n')
	}
	return b.String()
}

// panicStack trims a runtime/debug.Stack taken while recovering: the
// header, the frames up to and including panic(), and the http server
// frames under the handler.
func panicStack(st []byte) string {
	lines := strings.Split(strings.TrimRight(string(st), "\n"), "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "goroutine ") {
		lines = lines[1:]
	}
	// Go tracebacks are pairs: "func(args)" then "\tfile:line +0x.."
	var fs []frame
	for i := 0; i+1 < len(lines); i += 2 {
		fn := lines[i]
		if strings.HasPrefix(fn, "created by ") {
			continue
		}
		if j := strings.LastIndexByte(fn, '('); j > 0 {
			fn = fn[:j]
		}
		loc := strings.TrimSpace(lines[i+1])
		if j := strings.LastIndex(loc, " +0x"); j > 0 {
			loc = loc[:j]
		}
		fs = append(fs, frame{fn: fn, loc: loc})
	}
	// The last panic() is the original one (a handler that recovers and
	// panics again adds frames above it).
	for i := len(fs) - 1; i >= 0; i-- {
		if fs[i].fn == "panic" || fs[i].fn == "runtime.gopanic" {
			fs = fs[i+1:]
			break
		}
	}
	return formatStack(trimFrames(fs))
}

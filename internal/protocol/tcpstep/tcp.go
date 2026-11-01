// Package tcpstep is the "tcp" step: send bytes over a raw TCP (or TLS)
// connection and read a reply.
//
//   - tcp:
//     name: ping                     # default: "TCP <addr>"
//     addr: localhost:16379
//     tls: false
//     send: 'AUTH lt\r\nPING\r\n'    # templated; \r \n \t \\ \xHH escapes work in any quoting
//     readUntil: "+PONG\r\n"         # or readBytes: 7; default: until expect matches
//     expect: "+PONG"                # substring, or /regex/
//     keep: true                     # reuse the connection across iterations
//     timeout: 2s
//     extract: { reply: body, n: "regex:count=(\\d+)" }
//
// Measurements: "<name>:connect" for the TCP (and TLS) handshake, "<name>"
// from the send to the end of the read. Without readUntil/readBytes the
// step reads until expect matches (or, with no expect, does not read).
// readUntil without expect only waits for the delimiter.
package tcpstep

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ymg4798/lt/internal/protocol"
	"github.com/ymg4798/lt/internal/tmpl"
	"github.com/ymg4798/lt/internal/yamlx"
)

func init() { protocol.Register("tcp", compile) }

// maxRead caps how much a step reads while waiting for expect/readUntil.
const maxRead = 4 << 20

type spec struct {
	Name       string            `yaml:"name"`
	Addr       string            `yaml:"addr"`
	TLS        bool              `yaml:"tls"`
	ServerName string            `yaml:"serverName"`
	Send       string            `yaml:"send"`
	Expect     string            `yaml:"expect"`
	ReadUntil  string            `yaml:"readUntil"`
	ReadBytes  int               `yaml:"readBytes"`
	Keep       bool              `yaml:"keep"`
	Conn       string            `yaml:"conn"`
	Timeout    yamlx.Duration    `yaml:"timeout"`
	MaxLatency yamlx.Duration    `yaml:"maxLatency"`
	Extract    map[string]string `yaml:"extract"`
}

type step struct {
	name       string
	addr       *tmpl.String
	tls        bool
	serverName string
	insecure   bool
	send       *tmpl.String
	expectStr  *tmpl.String
	expectRe   *regexp.Regexp
	until      []byte
	readBytes  int
	keep       bool
	conn       string
	timeout    time.Duration
	maxLatency time.Duration
	extract    *protocol.Extract
}

func compile(raw any, env *protocol.Env) (protocol.Step, error) {
	var sp spec
	if err := yamlx.Decode(raw, &sp); err != nil {
		return nil, fmt.Errorf("tcp: %w", err)
	}
	// yamlx.Decode round-trips through YAML text, which loses some
	// whitespace-only strings (a lone "\n"); take these from the raw map.
	if m, ok := raw.(map[string]any); ok {
		for k, dst := range map[string]*string{"send": &sp.Send, "readUntil": &sp.ReadUntil, "expect": &sp.Expect} {
			if v, ok := m[k].(string); ok {
				*dst = v
			}
		}
	}
	if sp.Addr == "" {
		return nil, fmt.Errorf("tcp: addr is required")
	}
	s := &step{name: sp.Name, tls: sp.TLS, serverName: sp.ServerName, insecure: env.Options.Insecure,
		readBytes: sp.ReadBytes, keep: sp.Keep, conn: sp.Conn, timeout: sp.Timeout.D(), maxLatency: sp.MaxLatency.D()}
	if s.name == "" {
		s.name = "TCP " + sp.Addr
	}
	if s.conn == "" {
		s.conn = s.name
	}
	if s.timeout == 0 {
		s.timeout = env.Options.Timeout
	}
	var err error
	if s.addr, err = tmpl.Compile(sp.Addr); err != nil {
		return nil, fmt.Errorf("tcp %s: addr: %w", s.name, err)
	}
	if sp.Send != "" {
		src, err := unescape(sp.Send)
		if err != nil {
			return nil, fmt.Errorf("tcp %s: send: %w", s.name, err)
		}
		if s.send, err = tmpl.Compile(src); err != nil {
			return nil, fmt.Errorf("tcp %s: send: %w", s.name, err)
		}
	}
	if sp.ReadUntil != "" && sp.ReadBytes > 0 {
		return nil, fmt.Errorf("tcp %s: use readUntil or readBytes, not both", s.name)
	}
	if sp.ReadBytes < 0 || sp.ReadBytes > maxRead {
		return nil, fmt.Errorf("tcp %s: readBytes must be between 1 and %d", s.name, maxRead)
	}
	if sp.ReadUntil != "" {
		u, err := unescape(sp.ReadUntil)
		if err != nil {
			return nil, fmt.Errorf("tcp %s: readUntil: %w", s.name, err)
		}
		s.until = []byte(u)
	}
	if e := sp.Expect; e != "" {
		if len(e) >= 2 && e[0] == '/' && e[len(e)-1] == '/' {
			if s.expectRe, err = regexp.Compile(e[1 : len(e)-1]); err != nil {
				return nil, fmt.Errorf("tcp %s: expect: %w", s.name, err)
			}
		} else {
			u, err := unescape(e)
			if err != nil {
				return nil, fmt.Errorf("tcp %s: expect: %w", s.name, err)
			}
			if s.expectStr, err = tmpl.Compile(u); err != nil {
				return nil, fmt.Errorf("tcp %s: expect: %w", s.name, err)
			}
		}
	}
	if s.send == nil && s.expectStr == nil && s.expectRe == nil && s.until == nil && s.readBytes == 0 {
		return nil, fmt.Errorf("tcp %s: nothing to do: give send, expect, readUntil or readBytes", s.name)
	}
	for name, e := range sp.Extract {
		if e != "body" && !strings.HasPrefix(e, "regex:") {
			return nil, fmt.Errorf("tcp %s: extract %s: use body or regex:...", s.name, name)
		}
	}
	if s.extract, err = protocol.CompileExtract(sp.Extract); err != nil {
		return nil, fmt.Errorf("tcp %s: %w", s.name, err)
	}
	return s, nil
}

// unescape interprets \r \n \t \0 \\ \xHH so that escapes work in
// single-quoted and plain YAML strings too.
func unescape(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		case 'x':
			if i+2 >= len(s) {
				return "", fmt.Errorf(`bad \x escape`)
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf(`bad \x escape %q`, s[i-1:i+3])
			}
			b.WriteByte(byte(v))
			i += 2
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}

type conn struct {
	c  net.Conn
	rd *bufio.Reader
}

func (s *step) key() string { return "tcp:" + s.conn }

func (s *step) Run(ctx context.Context, vu *protocol.VU) error {
	addr, err := s.addr.RenderString(vu)
	if err != nil {
		return protocol.Fail(s.name, "template", err.Error())
	}
	var payload []byte
	if s.send != nil {
		p, err := s.send.RenderString(vu)
		if err != nil {
			return protocol.Fail(s.name, "template", err.Error())
		}
		payload = []byte(p)
	}
	var want string
	if s.expectStr != nil {
		if want, err = s.expectStr.RenderString(vu); err != nil {
			return protocol.Fail(s.name, "template", err.Error())
		}
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	var cn *conn
	if s.keep {
		if v, ok := vu.GetState(s.key()); ok {
			cn = v.(*conn)
		}
	}
	if cn == nil {
		start := time.Now()
		c, err := s.dial(ctx, addr)
		lat := time.Since(start)
		if err != nil {
			kind := protocol.ClassifyErr(err)
			vu.Record(protocol.Result{Step: s.name + ":connect", Latency: lat, Err: kind})
			return protocol.Fail(s.name, kind, err.Error())
		}
		vu.Record(protocol.Result{Step: s.name + ":connect", Latency: lat})
		cn = &conn{c: c, rd: bufio.NewReader(c)}
		if s.keep {
			vu.SetState(s.key(), cn)
			vu.AtClose(func() {
				if cur, ok := vu.GetState(s.key()); ok && cur == cn {
					cn.c.Close()
					vu.SetState(s.key(), nil)
				}
			})
		} else {
			defer cn.c.Close()
		}
	}
	drop := func() {
		cn.c.Close()
		if s.keep {
			vu.SetState(s.key(), nil)
		}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = cn.c.SetDeadline(dl)
	} else {
		_ = cn.c.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { _ = cn.c.SetDeadline(time.Now()) })
	defer stop()

	start := time.Now()
	if len(payload) > 0 {
		if _, err := cn.c.Write(payload); err != nil {
			lat := time.Since(start)
			drop()
			kind := s.errKind(ctx, err)
			vu.Record(protocol.Result{Step: s.name, Latency: lat, Err: kind})
			return protocol.Fail(s.name, kind, err.Error())
		}
	}
	data, err := s.read(cn.rd, want)
	lat := time.Since(start)
	n := int64(len(payload) + len(data))
	if err != nil {
		drop()
		kind := s.errKind(ctx, err)
		msg := err.Error()
		if len(data) > 0 {
			msg += fmt.Sprintf(" (got %q)", trim(data))
		}
		vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: n, Err: kind})
		return protocol.Fail(s.name, kind, msg)
	}
	kind, msg := "", ""
	switch {
	case s.maxLatency > 0 && lat > s.maxLatency:
		kind, msg = "check:latency", fmt.Sprintf("%s > %s", lat, s.maxLatency)
	case s.expectStr != nil && !bytes.Contains(data, []byte(want)):
		kind, msg = "check:expect", fmt.Sprintf("reply %q does not contain %q", trim(data), want)
	case s.expectRe != nil && !s.expectRe.Match(data):
		kind, msg = "check:expect", fmt.Sprintf("reply %q does not match /%s/", trim(data), s.expectRe)
	}
	r := &protocol.Response{Body: data, Latency: lat}
	if kind == "" {
		kind, msg = s.extract.Apply(r, vu.Vars)
	}
	vu.Last = r
	vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: n, Err: kind})
	if kind != "" {
		return protocol.Fail(s.name, kind, msg)
	}
	return nil
}

func trim(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// errKind maps a read past expect (EOF before a match) and our own
// deadline to stable kinds.
func (s *step) errKind(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, errNoMatch) {
		return "check:expect"
	}
	return protocol.ClassifyErr(err)
}

var errNoMatch = errors.New("connection closed before the expected reply")

func (s *step) read(rd *bufio.Reader, want string) ([]byte, error) {
	switch {
	case s.readBytes > 0:
		buf := make([]byte, s.readBytes)
		n, err := io.ReadFull(rd, buf)
		return buf[:n], err
	case s.until != nil:
		return readUntil(rd, func(b []byte, _ bool) bool { return bytes.HasSuffix(b, s.until) }, nil)
	case s.expectRe != nil:
		// A regex is tried at chunk and line ends only, so on a kept
		// connection it may consume bytes past the match.
		return readUntil(rd, func(b []byte, boundary bool) bool { return boundary && s.expectRe.Match(b) }, errNoMatch)
	case s.expectStr != nil:
		// The first occurrence ends at the byte just read.
		w := []byte(want)
		return readUntil(rd, func(b []byte, _ bool) bool { return bytes.HasSuffix(b, w) }, errNoMatch)
	}
	return nil, nil
}

// readUntil reads byte-wise from the buffered reader until done reports
// true, so a kept connection keeps the bytes after the match for the next
// step. boundary is true at a line end or when no more bytes are buffered.
func readUntil(rd *bufio.Reader, done func(b []byte, boundary bool) bool, eofErr error) ([]byte, error) {
	var buf []byte
	for len(buf) < maxRead {
		c, err := rd.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && eofErr != nil {
				return buf, eofErr
			}
			return buf, err
		}
		buf = append(buf, c)
		if done(buf, c == '\n' || rd.Buffered() == 0) {
			return buf, nil
		}
	}
	return buf, fmt.Errorf("no match in %d bytes", maxRead)
}

func (s *step) dial(ctx context.Context, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !s.tls {
		return d.DialContext(ctx, "tcp", addr)
	}
	sn := s.serverName
	if sn == "" {
		sn, _, _ = net.SplitHostPort(addr)
	}
	cfg := &tls.Config{ServerName: sn, InsecureSkipVerify: s.insecure} //nolint:gosec // opt-in
	return (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", addr)
}

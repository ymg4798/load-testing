// Package redisstep is the "redis" step: send one command, or a pipeline,
// over a per-VU Redis connection (RESP2).
//
//   - redis:
//     name: set session                    # default: "REDIS SET"
//     url: redis://:lt@localhost:16379/0   # rediss:// for TLS; or addr/password/db/tls
//     command: [SET, "session:{{ vu }}", "{{ uuid() }}", EX, 60]
//     check: { reply: OK }
//
//   - redis:
//     url: "{{ redisUrl }}"
//     # pipeline: sent together, replies read in order
//     commands: [[INCR, "counter:{{ vu }}"], [GET, "session:{{ vu }}"]]
//     check: { exists: true }              # last reply is not nil
//     extract: { sid: reply, n: replies.0 }
//
// Checks look at the last reply: reply (equal, templated), contains,
// exists, maxLatency. Error replies fail the step as redis:<PREFIX>
// (redis:WRONGTYPE, redis:NOAUTH ...). extract reads reply (last reply),
// replies.N (Nth reply of a pipeline) and paths into array replies
// (reply.0). A connection is opened on first use ("<name>:connect" is
// recorded), kept for the VU's lifetime and redialed after a network error.
package redisstep

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ymg4798/load-testing/internal/protocol"
	"github.com/ymg4798/load-testing/internal/tmpl"
	"github.com/ymg4798/load-testing/internal/yamlx"
)

func init() { protocol.Register("redis", compile) }

type checkSpec struct {
	Reply      any            `yaml:"reply"`
	Contains   string         `yaml:"contains"`
	Exists     *bool          `yaml:"exists"`
	MaxLatency yamlx.Duration `yaml:"maxLatency"`
}

type spec struct {
	Name     string            `yaml:"name"`
	URL      string            `yaml:"url"`
	Addr     string            `yaml:"addr"`
	Username string            `yaml:"username"`
	Password string            `yaml:"password"`
	DB       int               `yaml:"db"`
	TLS      bool              `yaml:"tls"`
	Command  []any             `yaml:"command"`
	Commands [][]any           `yaml:"commands"`
	Timeout  yamlx.Duration    `yaml:"timeout"`
	Check    *checkSpec        `yaml:"check"`
	Extract  map[string]string `yaml:"extract"`
}

type step struct {
	name     string
	addr     *tmpl.String // url or host:port
	isURL    bool
	user     string
	pass     string
	db       int
	tls      bool
	insecure bool
	cmds     [][]*tmpl.String
	timeout  time.Duration

	reply      tmpl.Value
	contains   *tmpl.String
	exists     *bool
	maxLatency time.Duration
	extract    map[string][]string
}

func compile(raw any, env *protocol.Env) (protocol.Step, error) {
	var sp spec
	if err := yamlx.Decode(raw, &sp); err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	s := &step{name: sp.Name, user: sp.Username, pass: sp.Password, db: sp.DB, tls: sp.TLS,
		insecure: env.Options.Insecure, timeout: sp.Timeout.D()}
	if s.timeout == 0 {
		s.timeout = env.Options.Timeout
	}
	var err error
	switch {
	case sp.URL != "" && sp.Addr != "":
		return nil, fmt.Errorf("redis: use url or addr, not both")
	case sp.URL != "":
		s.isURL = true
		if s.addr, err = tmpl.Compile(sp.URL); err != nil {
			return nil, fmt.Errorf("redis: url: %w", err)
		}
	case sp.Addr != "":
		if s.addr, err = tmpl.Compile(sp.Addr); err != nil {
			return nil, fmt.Errorf("redis: addr: %w", err)
		}
	default:
		return nil, fmt.Errorf("redis: url or addr is required")
	}
	if len(sp.Command) > 0 && len(sp.Commands) > 0 {
		return nil, fmt.Errorf("redis: use command or commands, not both")
	}
	cmds := sp.Commands
	if len(sp.Command) > 0 {
		cmds = [][]any{sp.Command}
	}
	if len(cmds) == 0 {
		return nil, fmt.Errorf("redis: command is required")
	}
	for i, c := range cmds {
		if len(c) == 0 {
			return nil, fmt.Errorf("redis: command %d is empty", i+1)
		}
		var args []*tmpl.String
		for _, a := range c {
			t, err := tmpl.Compile(tmpl.ToString(a))
			if err != nil {
				return nil, fmt.Errorf("redis: %w", err)
			}
			args = append(args, t)
		}
		s.cmds = append(s.cmds, args)
	}
	if s.name == "" {
		if len(s.cmds) == 1 {
			s.name = "REDIS " + strings.ToUpper(s.cmds[0][0].Raw())
		} else {
			s.name = fmt.Sprintf("REDIS pipeline(%d)", len(s.cmds))
		}
	}
	if c := sp.Check; c != nil {
		if c.Reply != nil {
			if s.reply, err = tmpl.CompileValue(c.Reply); err != nil {
				return nil, fmt.Errorf("redis %s: check.reply: %w", s.name, err)
			}
		}
		if c.Contains != "" {
			if s.contains, err = tmpl.Compile(c.Contains); err != nil {
				return nil, fmt.Errorf("redis %s: check.contains: %w", s.name, err)
			}
		}
		s.exists, s.maxLatency = c.Exists, c.MaxLatency.D()
	}
	s.extract = map[string][]string{}
	for name, e := range sp.Extract {
		p := strings.Split(strings.TrimSpace(e), ".")
		if p[0] != "reply" && p[0] != "replies" {
			return nil, fmt.Errorf("redis %s: extract %s: use reply, reply.N or replies.N", s.name, name)
		}
		s.extract[name] = p
	}
	return s, nil
}

// conn is a per-VU connection.
type conn struct {
	c  net.Conn
	rd *bufio.Reader
}

func (s *step) Run(ctx context.Context, vu *protocol.VU) error {
	target, err := s.addr.RenderString(vu)
	if err != nil {
		return protocol.Fail(s.name, "template", err.Error())
	}
	cmds := make([][]string, len(s.cmds))
	for i, c := range s.cmds {
		cmds[i] = make([]string, len(c))
		for j, a := range c {
			if cmds[i][j], err = a.RenderString(vu); err != nil {
				return protocol.Fail(s.name, "template", err.Error())
			}
		}
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	key := "redis:" + target + ":" + s.user + ":" + strconv.Itoa(s.db)
	var cn *conn
	if v, ok := vu.GetState(key); ok {
		cn = v.(*conn)
	} else {
		start := time.Now()
		cn, err = s.dial(ctx, target)
		lat := time.Since(start)
		if err != nil {
			kind := classify(err)
			vu.Record(protocol.Result{Step: s.name + ":connect", Latency: lat, Err: kind})
			return protocol.Fail(s.name, kind, err.Error())
		}
		vu.Record(protocol.Result{Step: s.name + ":connect", Latency: lat})
		vu.SetState(key, cn)
		vu.AtClose(func() {
			if cur, ok := vu.GetState(key); ok && cur == cn {
				cn.c.Close()
				vu.SetState(key, nil)
			}
		})
	}
	drop := func() {
		cn.c.Close()
		vu.SetState(key, nil)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = cn.c.SetDeadline(dl)
	} else {
		_ = cn.c.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { _ = cn.c.SetDeadline(time.Now()) })
	defer stop()

	start := time.Now()
	n, err := writeCommands(cn.c, cmds)
	replies := make([]any, 0, len(cmds))
	var firstErr error
	if err == nil {
		for range cmds {
			v, rerr := readReply(cn.rd)
			if rerr != nil && !isRedisError(rerr) {
				err = rerr
				break
			}
			if rerr != nil && firstErr == nil {
				firstErr = rerr
			}
			replies = append(replies, v)
		}
	}
	lat := time.Since(start)
	if err != nil {
		drop()
		kind := classify(err)
		if ctx.Err() != nil {
			kind = protocol.ClassifyErr(ctx.Err())
		}
		vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: n, Err: kind})
		return protocol.Fail(s.name, kind, err.Error())
	}
	size := n
	for _, r := range replies {
		size += replySize(r)
	}
	kind, msg := "", ""
	if firstErr != nil {
		kind, msg = errKind(firstErr), firstErr.Error()
	} else {
		kind, msg = s.verify(vu, replies, lat)
	}
	vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: size, Err: kind})
	if kind != "" {
		return protocol.Fail(s.name, kind, msg)
	}
	return nil
}

func (s *step) verify(vu *protocol.VU, replies []any, lat time.Duration) (string, string) {
	last := replies[len(replies)-1]
	if s.maxLatency > 0 && lat > s.maxLatency {
		return "check:latency", fmt.Sprintf("%s > %s", lat, s.maxLatency)
	}
	if s.exists != nil && (last != nil) != *s.exists {
		if *s.exists {
			return "check:exists", "reply is nil"
		}
		return "check:exists", fmt.Sprintf("reply is %v, want nil", last)
	}
	if s.reply != nil {
		want, err := s.reply.Render(vu)
		if err != nil {
			return "check:template", err.Error()
		}
		if replyString(last) != replyString(want) {
			return "check:reply", fmt.Sprintf("reply %q, want %q", replyString(last), replyString(want))
		}
	}
	if s.contains != nil {
		want, err := s.contains.RenderString(vu)
		if err != nil {
			return "check:template", err.Error()
		}
		if !strings.Contains(replyString(last), want) {
			return "check:reply", fmt.Sprintf("reply does not contain %q", want)
		}
	}
	root := map[string]any{"reply": last, "replies": replies}
	for name, p := range s.extract {
		v, ok := tmpl.LookupPath(root, p)
		if !ok {
			return "extract:" + name, strings.Join(p, ".") + " missing"
		}
		vu.Vars[name] = v
	}
	return "", ""
}

func replyString(v any) string {
	if l, ok := v.([]any); ok {
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = replyString(x)
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	return tmpl.ToString(v)
}

func replySize(v any) int64 {
	switch x := v.(type) {
	case string:
		return int64(len(x))
	case []any:
		var n int64
		for _, y := range x {
			n += replySize(y)
		}
		return n
	}
	return 8
}

func (s *step) dial(ctx context.Context, target string) (*conn, error) {
	addr, user, pass, db, useTLS := target, s.user, s.pass, s.db, s.tls
	serverName := ""
	if s.isURL {
		u, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("redis url: %w", err)
		}
		switch u.Scheme {
		case "redis":
		case "rediss":
			useTLS = true
		default:
			return nil, fmt.Errorf("redis url: scheme must be redis:// or rediss://")
		}
		addr = u.Host
		if u.Port() == "" {
			addr = net.JoinHostPort(u.Hostname(), "6379")
		}
		if u.User != nil {
			user = u.User.Username()
			if p, ok := u.User.Password(); ok {
				pass = p
			} else { // redis://secret@host: a bare password
				user, pass = "", user
			}
		}
		if p := strings.Trim(u.Path, "/"); p != "" {
			if db, err = strconv.Atoi(p); err != nil {
				return nil, fmt.Errorf("redis url: bad db %q", p)
			}
		}
		serverName = u.Hostname()
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var c net.Conn
	var err error
	if useTLS {
		if serverName == "" {
			serverName, _, _ = net.SplitHostPort(addr)
		}
		cfg := &tls.Config{ServerName: serverName, InsecureSkipVerify: s.insecure} //nolint:gosec // opt-in
		c, err = (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", addr)
	} else {
		c, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	cn := &conn{c: c, rd: bufio.NewReader(c)}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	var setup [][]string
	if pass != "" {
		if user != "" {
			setup = append(setup, []string{"AUTH", user, pass})
		} else {
			setup = append(setup, []string{"AUTH", pass})
		}
	}
	if db != 0 {
		setup = append(setup, []string{"SELECT", strconv.Itoa(db)})
	}
	for _, args := range setup {
		if _, err := writeCommands(c, [][]string{args}); err != nil {
			c.Close()
			return nil, err
		}
		if _, err := readReply(cn.rd); err != nil {
			c.Close()
			return nil, fmt.Errorf("redis %s: %w", strings.ToLower(args[0]), err)
		}
	}
	return cn, nil
}

// ---- RESP2 ----

type redisError string

func (e redisError) Error() string { return string(e) }

func isRedisError(err error) bool {
	var re redisError
	return errors.As(err, &re)
}

// errKind gives redis:WRONGTYPE from "WRONGTYPE Operation against ...".
func errKind(err error) string {
	var re redisError
	if errors.As(err, &re) {
		p, _, _ := strings.Cut(string(re), " ")
		if p == "" {
			p = "ERR"
		}
		return "redis:" + p
	}
	return classify(err)
}

func classify(err error) string {
	var re redisError
	if errors.As(err, &re) {
		return errKind(err)
	}
	return protocol.ClassifyErr(err)
}

func writeCommands(w io.Writer, cmds [][]string) (int64, error) {
	var b strings.Builder
	for _, args := range cmds {
		fmt.Fprintf(&b, "*%d\r\n", len(args))
		for _, a := range args {
			fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

const maxBulk = 512 << 20

// readReply reads one RESP2 (or the RESP3 subset a RESP2 client can meet)
// reply: simple and bulk strings as string, integers as int64, arrays as
// []any, nil as nil, error replies as redisError.
func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("redis: bad reply line %q", line)
	}
	kind, body := line[0], line[1:len(line)-2]
	switch kind {
	case '+':
		return body, nil
	case '-':
		return nil, redisError(body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n > maxBulk {
			return nil, fmt.Errorf("redis: bad bulk length %q", body)
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n > 1<<24 {
			return nil, fmt.Errorf("redis: bad array length %q", body)
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, 0, n)
		var first error
		for range n {
			v, err := readReply(r)
			if err != nil && !isRedisError(err) {
				return nil, err
			}
			if err != nil && first == nil {
				first = err
			}
			out = append(out, v)
		}
		if first != nil {
			return out, first
		}
		return out, nil
	}
	return nil, fmt.Errorf("redis: unknown reply type %q", kind)
}

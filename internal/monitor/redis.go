package monitor

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
	"sync"
	"time"
)

// redisSource speaks just enough RESP for INFO over one TCP connection.
type redisSource struct {
	addr       string
	tls        *tls.Config // rediss://
	user, pass string
	db         int

	mu   sync.Mutex // one command at a time on the connection
	conn net.Conn
	rd   *bufio.Reader
}

// newRedis accepts redis://[user:pass@]host[:6379][/db], rediss://... (TLS;
// ?insecure=true skips certificate checks) and plain host:port.
func newRedis(dsn string) (*redisSource, string, error) {
	if !strings.Contains(dsn, "://") {
		dsn = "redis://" + dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, "", fmt.Errorf("redis url: %w", err)
	}
	s := &redisSource{}
	switch u.Scheme {
	case "redis":
	case "rediss":
		s.tls = &tls.Config{ServerName: u.Hostname()}
		if v, _ := strconv.ParseBool(u.Query().Get("insecure")); v {
			s.tls.InsecureSkipVerify = true
		}
	default:
		return nil, "", fmt.Errorf("redis url: scheme must be redis:// or rediss://, got %q", u.Scheme)
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "6379"
	}
	s.addr = net.JoinHostPort(host, port)
	if u.User != nil {
		s.user = u.User.Username()
		s.pass, _ = u.User.Password()
		if s.pass == "" { // redis://secret@host: a bare password
			s.user, s.pass = "", s.user
		}
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		if s.db, err = strconv.Atoi(p); err != nil {
			return nil, "", fmt.Errorf("redis url: bad db %q", p)
		}
	}
	return s, s.addr, nil
}

func (s *redisSource) dial(ctx context.Context) error {
	d := &net.Dialer{Timeout: 2 * time.Second}
	var c net.Conn
	var err error
	if s.tls != nil {
		c, err = (&tls.Dialer{NetDialer: d, Config: s.tls}).DialContext(ctx, "tcp", s.addr)
	} else {
		c, err = d.DialContext(ctx, "tcp", s.addr)
	}
	if err != nil {
		return err
	}
	s.conn, s.rd = c, bufio.NewReader(c)
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	var setup [][]string
	if s.pass != "" {
		if s.user != "" {
			setup = append(setup, []string{"AUTH", s.user, s.pass})
		} else {
			setup = append(setup, []string{"AUTH", s.pass})
		}
	}
	if s.db != 0 {
		setup = append(setup, []string{"SELECT", strconv.Itoa(s.db)})
	}
	setup = append(setup, []string{"CLIENT", "SETNAME", appName})
	for i, args := range setup {
		if err := writeCommand(c, args...); err != nil {
			s.drop()
			return err
		}
		if _, err := readReply(s.rd); err != nil {
			if args[0] == "CLIENT" && i == len(setup)-1 && isRedisError(err) {
				continue // naming the connection is optional
			}
			s.drop()
			return fmt.Errorf("redis %s: %w", strings.ToLower(args[0]), err)
		}
	}
	return nil
}

func (s *redisSource) drop() {
	if s.conn != nil {
		s.conn.Close()
		s.conn, s.rd = nil, nil
	}
}

// do sends one command and returns its reply, reconnecting as needed.
func (s *redisSource) do(ctx context.Context, args ...string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		if err := s.dial(ctx); err != nil {
			return nil, err
		}
	}
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(2 * time.Second)
	}
	conn := s.conn
	_ = conn.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := writeCommand(s.conn, args...); err != nil {
		s.drop()
		return nil, err
	}
	v, err := readReply(s.rd)
	if err != nil && !isRedisError(err) {
		s.drop() // the stream may be out of step
	}
	return v, err
}

func (s *redisSource) close() {
	s.mu.Lock()
	s.drop()
	s.mu.Unlock()
}

func (s *redisSource) info(ctx context.Context, section ...string) (map[string]string, error) {
	v, err := s.do(ctx, append([]string{"INFO"}, section...)...)
	if err != nil {
		return nil, err
	}
	b, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("redis info: unexpected reply %T", v)
	}
	return parseInfo(b), nil
}

func (s *redisSource) health(ctx context.Context, c *calc) error {
	in, err := s.info(ctx)
	if err != nil {
		return err
	}
	num := func(k string) (float64, bool) {
		v, err := strconv.ParseFloat(in[k], 64)
		return v, err == nil
	}
	if v, ok := num("connected_clients"); ok {
		c.gauge("redis clients", "", maxf(0, v-1)) // minus the monitor
	}
	if v, ok := num("blocked_clients"); ok {
		c.gauge("redis blocked clients", "", v)
	}
	if v, ok := num("total_commands_processed"); ok {
		c.rate("redis ops", v)
	}
	if v, ok := num("used_memory"); ok {
		c.gauge("redis memory", "bytes", v)
	}
	h, ok1 := num("keyspace_hits")
	m, ok2 := num("keyspace_misses")
	if ok1 && ok2 {
		c.hitRatio("redis hit", h, m)
	}
	if v, ok := num("evicted_keys"); ok {
		c.rate("redis evicted keys", v)
	}
	if v, ok := num("expired_keys"); ok {
		c.rate("redis expired keys", v)
	}
	us, ok1 := num("used_cpu_user")
	sy, ok2 := num("used_cpu_sys")
	if ok1 && ok2 {
		if d, secs, ok := c.delta("redis cpu", us+sy); ok {
			c.gauge("redis cpu", "%", d/secs*100)
		}
	}
	var keys float64
	found := false
	for k, v := range in {
		if strings.HasPrefix(k, "db") {
			if n, ok := infoField(v, "keys"); ok {
				keys += n
				found = true
			}
		}
	}
	if found || in["redis_version"] != "" {
		c.gauge("redis keys", "", keys)
	}
	if in["role"] == "slave" {
		// Bytes the replica is behind is not exported; seconds since the
		// last interaction with the master is the closest thing.
		if v, ok := num("master_last_io_seconds_ago"); ok && v >= 0 {
			c.gauge("redis master last io", "ms", v*1000)
		}
	}
	return nil
}

// Commands the monitor sends itself, left out of Top SQL.
var monitorCommands = map[string]bool{"info": true, "auth": true, "select": true, "client|setname": true}

func (s *redisSource) statements(ctx context.Context) (map[string]stmtStat, error) {
	in, err := s.info(ctx, "commandstats")
	if err != nil {
		return nil, err
	}
	return parseCommandStats(in), nil
}

func parseCommandStats(in map[string]string) map[string]stmtStat {
	out := map[string]stmtStat{}
	for k, v := range in {
		cmd, ok := strings.CutPrefix(k, "cmdstat_")
		if !ok || monitorCommands[cmd] {
			continue
		}
		calls, ok1 := infoField(v, "calls")
		usec, ok2 := infoField(v, "usec")
		if !ok1 || !ok2 {
			continue
		}
		out[cmd] = stmtStat{query: strings.ToUpper(cmd), calls: int64(calls), totalMs: usec / 1000}
	}
	return out
}

// parseInfo reads INFO's "key:value" lines; "# Section" lines are skipped.
func parseInfo(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[k] = v
		}
	}
	return m
}

// infoField reads one field of "calls=10,usec=20,..." or "keys=1,expires=0".
func infoField(s, key string) (float64, bool) {
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			f, err := strconv.ParseFloat(v, 64)
			return f, err == nil
		}
	}
	return 0, false
}

// ---- RESP ----

type redisError string

func (e redisError) Error() string { return string(e) }

func isRedisError(err error) bool {
	var re redisError
	return errors.As(err, &re)
}

func writeCommand(w io.Writer, args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

const maxBulk = 64 << 20

// readReply reads one RESP2 reply: simple strings and bulk strings come back
// as string, integers as int64, arrays as []any, nil bulk as nil, and
// error replies as a redisError.
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
		if err != nil || n > 1<<20 {
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
		return out, first
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line)
}

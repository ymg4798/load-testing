// Package sqlstep is the "sql" step: run one statement against a database
// directly (PostgreSQL or MySQL).
//
//   - sql:
//     name: find user                      # default: "SQL SELECT users"
//     driver: postgres                     # postgres | mysql
//     dsn: "{{ pgUrl }}"                    # rendered once, on first use
//     query: SELECT id, name FROM users WHERE id = {{ users.id }}
//     exec: false                          # true for INSERT/UPDATE/DDL (no rows read)
//     pool: 32                             # max open connections, default maxInFlight or 64
//     timeout: 2s
//     check: { rows: ">= 1", json: { "$[0].name": kim }, maxLatency: 50ms }
//     extract: { id: row.0.id, n: rows }
//
// Every {{ expr }} in the query becomes a bind parameter ($1, $2 ... for
// PostgreSQL, ? for MySQL); values are never spliced into the SQL text. A
// quoted literal that contains placeholders, like '%{{ q }}%', becomes one
// parameter holding the rendered string.
//
// Rows (up to maxRows, default 1000) are kept as a JSON array of objects, so
// check.json / check.bodyContains and JSONPath extraction ($[0].id) work on
// them. extract also understands row.N.column, rows (row count) and
// affected (rows affected by exec).
package sqlstep

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver

	"github.com/ymg4798/load-testing/internal/protocol"
	"github.com/ymg4798/load-testing/internal/tmpl"
	"github.com/ymg4798/load-testing/internal/yamlx"
)

func init() { protocol.Register("sql", compile) }

type checkSpec struct {
	Rows            any            `yaml:"rows"`     // 3, ">= 1", "0"
	Affected        any            `yaml:"affected"` // for exec
	BodyContains    string         `yaml:"bodyContains"`
	BodyNotContains string         `yaml:"bodyNotContains"`
	JSON            map[string]any `yaml:"json"`
	MaxLatency      yamlx.Duration `yaml:"maxLatency"`
}

type spec struct {
	Name    string            `yaml:"name"`
	Driver  string            `yaml:"driver"`
	DSN     string            `yaml:"dsn"`
	Query   string            `yaml:"query"`
	Exec    bool              `yaml:"exec"`
	Pool    int               `yaml:"pool"`
	MaxRows int               `yaml:"maxRows"`
	Timeout yamlx.Duration    `yaml:"timeout"`
	Check   *checkSpec        `yaml:"check"`
	Extract map[string]string `yaml:"extract"`
}

type step struct {
	env     *protocol.Env
	name    string
	driver  string // database/sql driver name
	dsn     *tmpl.String
	query   string
	args    []*tmpl.String
	exec    bool
	pool    int
	maxRows int
	timeout time.Duration

	rows, affected func(int64) bool
	check          *protocol.Check
	needJSON       bool
	extract        *protocol.Extract
	special        []specialExtract

	once  sync.Once
	db    *sql.DB
	dbErr error
}

type specialExtract struct {
	name string
	kind string // rows, affected, row
	row  int
	col  string // "" = whole row
}

func compile(raw any, env *protocol.Env) (protocol.Step, error) {
	var sp spec
	if err := yamlx.Decode(raw, &sp); err != nil {
		return nil, fmt.Errorf("sql: %w", err)
	}
	if sp.DSN == "" || strings.TrimSpace(sp.Query) == "" {
		return nil, fmt.Errorf("sql: dsn and query are required")
	}
	s := &step{env: env, name: sp.Name, exec: sp.Exec, pool: sp.Pool, maxRows: sp.MaxRows, timeout: sp.Timeout.D()}
	style := placeholderDollar
	switch strings.ToLower(sp.Driver) {
	case "postgres", "postgresql", "pg", "pgx":
		s.driver = "pgx"
	case "mysql", "mariadb":
		s.driver, style = "mysql", placeholderQuestion
	case "":
		return nil, fmt.Errorf("sql: driver is required (postgres or mysql)")
	default:
		return nil, fmt.Errorf("sql: unknown driver %q (use postgres or mysql)", sp.Driver)
	}
	if s.name == "" {
		s.name = defaultName(sp.Query)
	}
	if s.timeout == 0 {
		s.timeout = env.Options.Timeout
	}
	if s.pool <= 0 {
		s.pool = env.Options.MaxInFlight
		if s.pool <= 0 {
			s.pool = 64
		}
	}
	if s.maxRows <= 0 {
		s.maxRows = 1000
	}
	var err error
	if s.dsn, err = tmpl.Compile(sp.DSN); err != nil {
		return nil, fmt.Errorf("sql %s: dsn: %w", s.name, err)
	}
	if s.query, s.args, err = compileQuery(sp.Query, style); err != nil {
		return nil, fmt.Errorf("sql %s: %w", s.name, err)
	}
	cs := &protocol.CheckSpec{}
	if c := sp.Check; c != nil {
		if c.Rows != nil {
			if s.rows, err = countMatcher(c.Rows); err != nil {
				return nil, fmt.Errorf("sql %s: check.rows: %w", s.name, err)
			}
		}
		if c.Affected != nil {
			if s.affected, err = countMatcher(c.Affected); err != nil {
				return nil, fmt.Errorf("sql %s: check.affected: %w", s.name, err)
			}
		}
		cs = &protocol.CheckSpec{BodyContains: c.BodyContains, BodyNotContains: c.BodyNotContains, JSON: c.JSON, MaxLatency: c.MaxLatency}
		if s.exec && (c.Rows != nil || c.BodyContains != "" || c.BodyNotContains != "" || len(c.JSON) > 0) {
			return nil, fmt.Errorf("sql %s: exec reads no rows; use check.affected", s.name)
		}
	}
	if s.check, err = protocol.CompileCheck(cs); err != nil {
		return nil, fmt.Errorf("sql %s: %w", s.name, err)
	}
	rest := map[string]string{}
	for name, e := range sp.Extract {
		se, ok, err := parseSpecial(name, e)
		if err != nil {
			return nil, fmt.Errorf("sql %s: %w", s.name, err)
		}
		if ok {
			if s.exec && se.kind != "affected" {
				return nil, fmt.Errorf("sql %s: extract %s: exec reads no rows", s.name, name)
			}
			s.special = append(s.special, se)
			continue
		}
		rest[name] = e
	}
	if s.extract, err = protocol.CompileExtract(rest); err != nil {
		return nil, fmt.Errorf("sql %s: %w", s.name, err)
	}
	s.needJSON = env.HasScript || s.check.NeedsBody() || s.extract.NeedsBody()
	return s, nil
}

func parseSpecial(name, e string) (specialExtract, bool, error) {
	e = strings.TrimSpace(e)
	switch e {
	case "rows", "affected":
		return specialExtract{name: name, kind: e}, true, nil
	}
	if !strings.HasPrefix(e, "row.") {
		return specialExtract{}, false, nil
	}
	idx, col, _ := strings.Cut(strings.TrimPrefix(e, "row."), ".")
	n, err := strconv.Atoi(idx)
	if err != nil || n < 0 {
		return specialExtract{}, false, fmt.Errorf("extract %s: %q: want row.<index>.<column>", name, e)
	}
	return specialExtract{name: name, kind: "row", row: n, col: col}, true, nil
}

// countMatcher accepts 3, "3", ">= 1", "> 0", "< 10", "!= 0".
func countMatcher(v any) (func(int64) bool, error) {
	s := strings.TrimSpace(tmpl.ToString(v))
	op := "=="
	for _, o := range []string{">=", "<=", "==", "!=", ">", "<", "="} {
		if strings.HasPrefix(s, o) {
			op, s = o, strings.TrimSpace(s[len(o):])
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad value %v (use 3 or \">= 1\")", v)
	}
	switch op {
	case ">=":
		return func(x int64) bool { return x >= n }, nil
	case "<=":
		return func(x int64) bool { return x <= n }, nil
	case ">":
		return func(x int64) bool { return x > n }, nil
	case "<":
		return func(x int64) bool { return x < n }, nil
	case "!=":
		return func(x int64) bool { return x != n }, nil
	}
	return func(x int64) bool { return x == n }, nil
}

const (
	placeholderDollar   byte = '$'
	placeholderQuestion byte = '?'
)

// compileQuery turns {{ expr }} placeholders into bind parameters. It
// tracks quoted text so that a placeholder inside a string literal turns
// the whole literal into one parameter, and refuses placeholders inside
// quoted identifiers (parameters cannot name tables or columns).
func compileQuery(q string, style byte) (string, []*tmpl.String, error) {
	var (
		out  strings.Builder
		args []*tmpl.String
	)
	param := func(src string) error {
		t, err := tmpl.Compile(src)
		if err != nil {
			return err
		}
		args = append(args, t)
		if style == placeholderDollar {
			out.WriteString("$" + strconv.Itoa(len(args)))
		} else {
			out.WriteByte('?')
		}
		return nil
	}
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case strings.HasPrefix(q[i:], "{{"):
			j := strings.Index(q[i:], "}}")
			if j < 0 {
				return "", nil, fmt.Errorf("query: missing }}")
			}
			if err := param(q[i : i+j+2]); err != nil {
				return "", nil, err
			}
			i += j + 2
		case c == '\'':
			end, lit, err := scanLiteral(q, i, style)
			if err != nil {
				return "", nil, err
			}
			if strings.Contains(lit, "{{") {
				if err := param(lit); err != nil {
					return "", nil, err
				}
			} else {
				out.WriteString(q[i:end])
			}
			i = end
		case c == '"' || c == '`':
			j := strings.IndexByte(q[i+1:], c)
			if j < 0 {
				return "", nil, fmt.Errorf("query: unterminated %c", c)
			}
			ident := q[i : i+j+2]
			if strings.Contains(ident, "{{") {
				return "", nil, fmt.Errorf("query: {{ }} inside the quoted identifier %s: placeholders become bind parameters and can only carry values", ident)
			}
			out.WriteString(ident)
			i += j + 2
		case c == '-' && strings.HasPrefix(q[i:], "--"):
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = len(q) - i
			}
			out.WriteString(q[i : i+j])
			i += j
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), args, nil
}

// scanLiteral reads a single-quoted literal starting at q[i] and returns the
// index after it and its unescaped content.
func scanLiteral(q string, i int, style byte) (int, string, error) {
	var b strings.Builder
	for j := i + 1; j < len(q); j++ {
		switch q[j] {
		case '\'':
			if j+1 < len(q) && q[j+1] == '\'' {
				b.WriteByte('\'')
				j++
				continue
			}
			return j + 1, b.String(), nil
		case '\\':
			if style == placeholderQuestion && j+1 < len(q) { // MySQL backslash escapes
				b.WriteByte(q[j+1])
				j++
				continue
			}
		}
		b.WriteByte(q[j])
	}
	return 0, "", fmt.Errorf("query: unterminated string literal")
}

// defaultName gives "SQL SELECT users" from the query text.
func defaultName(q string) string {
	words := strings.Fields(stripTemplates(q))
	if len(words) == 0 {
		return "SQL"
	}
	verb := strings.ToUpper(words[0])
	after := func(kw string) string {
		for i, w := range words[:len(words)-1] {
			if strings.EqualFold(w, kw) {
				return cleanIdent(words[i+1])
			}
		}
		return ""
	}
	table := ""
	switch verb {
	case "SELECT", "DELETE":
		table = after("FROM")
	case "INSERT", "REPLACE":
		table = after("INTO")
	case "UPDATE":
		if len(words) > 1 {
			table = cleanIdent(words[1])
		}
	case "CREATE", "DROP", "TRUNCATE", "ALTER":
		table = after("TABLE")
		for _, kw := range []string{"IF", "NOT", "EXISTS"} {
			if strings.EqualFold(table, kw) {
				table = after(kw)
			}
		}
	case "WITH":
		return "SQL WITH"
	}
	if table == "" || strings.HasPrefix(table, "(") {
		return "SQL " + verb
	}
	return "SQL " + verb + " " + table
}

func stripTemplates(q string) string {
	for {
		i := strings.Index(q, "{{")
		if i < 0 {
			return q
		}
		j := strings.Index(q[i:], "}}")
		if j < 0 {
			return q
		}
		q = q[:i] + " ? " + q[i+j+2:]
	}
}

func cleanIdent(s string) string {
	s = strings.TrimRight(s, ";,")
	if i := strings.IndexByte(s, '('); i > 0 {
		s = s[:i]
	}
	return strings.Trim(s, "\"`")
}

func (s *step) open(vu *protocol.VU) (*sql.DB, error) {
	s.once.Do(func() {
		dsn, err := s.dsn.RenderString(vu)
		if err != nil {
			s.dbErr = err
			return
		}
		v, err := s.env.Shared("sql:"+s.driver+":"+dsn, func() (any, error) {
			db, err := sql.Open(s.driver, dsn)
			if err != nil {
				return nil, err
			}
			db.SetMaxOpenConns(s.pool)
			db.SetMaxIdleConns(s.pool)
			db.SetConnMaxIdleTime(time.Minute)
			s.env.OnClose(func() { db.Close() })
			return db, nil
		})
		if err != nil {
			s.dbErr = err
			return
		}
		s.db = v.(*sql.DB)
	})
	return s.db, s.dbErr
}

func (s *step) Run(ctx context.Context, vu *protocol.VU) error {
	db, err := s.open(vu)
	if err != nil {
		return protocol.Fail(s.name, "sql:open", err.Error())
	}
	args := make([]any, len(s.args))
	for i, a := range s.args {
		v, err := a.Render(vu)
		if err != nil {
			return protocol.Fail(s.name, "template", err.Error())
		}
		args[i] = bindValue(v)
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	start := time.Now()
	if s.exec {
		res, err := db.ExecContext(ctx, s.query, args...)
		lat := time.Since(start)
		if err != nil {
			return s.fail(ctx, vu, lat, 0, err)
		}
		n, _ := res.RowsAffected()
		return s.finish(vu, lat, 0, nil, 0, n)
	}
	rows, err := db.QueryContext(ctx, s.query, args...)
	if err != nil {
		return s.fail(ctx, vu, time.Since(start), 0, err)
	}
	kept, count, size, err := s.read(rows)
	lat := time.Since(start)
	if err != nil {
		return s.fail(ctx, vu, lat, size, err)
	}
	return s.finish(vu, lat, size, kept, count, 0)
}

func (s *step) read(rows *sql.Rows) ([]map[string]any, int64, int64, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, 0, 0, err
	}
	var (
		kept  []map[string]any
		count int64
		size  int64
	)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return kept, count, size, err
		}
		count++
		keep := len(kept) < s.maxRows
		var row map[string]any
		if keep {
			row = make(map[string]any, len(cols))
		}
		for i, v := range vals {
			v = plain(v)
			size += valueSize(v)
			if keep {
				row[cols[i]] = v
			}
		}
		if keep {
			kept = append(kept, row)
		}
	}
	return kept, count, size, rows.Err()
}

func (s *step) finish(vu *protocol.VU, lat time.Duration, size int64, rows []map[string]any, count, affected int64) error {
	r := &protocol.Response{Latency: lat}
	if s.needJSON {
		if rows == nil {
			rows = []map[string]any{}
		}
		r.Body, _ = json.Marshal(rows)
	}
	kind, msg := "", ""
	switch {
	case s.rows != nil && !s.rows(count):
		kind, msg = "check:rows", fmt.Sprintf("%d row(s)", count)
	case s.affected != nil && !s.affected(affected):
		kind, msg = "check:affected", fmt.Sprintf("%d row(s) affected", affected)
	}
	if kind == "" {
		kind, msg = s.check.Verify(r, vu, false)
	}
	if kind == "" {
		kind, msg = s.extract.Apply(r, vu.Vars)
	}
	if kind == "" {
		kind, msg = s.applySpecial(vu, rows, count, affected)
	}
	vu.Last = r
	vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: size, Err: kind})
	if kind != "" {
		return protocol.Fail(s.name, kind, msg)
	}
	return nil
}

func (s *step) applySpecial(vu *protocol.VU, rows []map[string]any, count, affected int64) (string, string) {
	for _, e := range s.special {
		switch e.kind {
		case "rows":
			vu.Vars[e.name] = count
		case "affected":
			vu.Vars[e.name] = affected
		case "row":
			if e.row >= len(rows) {
				return "extract:" + e.name, fmt.Sprintf("row %d missing (%d row(s))", e.row, count)
			}
			if e.col == "" {
				vu.Vars[e.name] = rows[e.row]
				continue
			}
			v, ok := rows[e.row][e.col]
			if !ok {
				return "extract:" + e.name, "column " + e.col + " missing"
			}
			vu.Vars[e.name] = v
		}
	}
	return "", ""
}

func (s *step) fail(ctx context.Context, vu *protocol.VU, lat time.Duration, size int64, err error) error {
	kind := classify(ctx, err)
	vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: size, Err: kind})
	return protocol.Fail(s.name, kind, err.Error())
}

// classify gives sql:timeout, sql:<SQLSTATE> (PostgreSQL), sql:<error
// number> (MySQL) or sql:<transport kind>.
func classify(ctx context.Context, err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if pg.Code == "57014" && ctx.Err() != nil { // query_canceled by our deadline
			return "sql:timeout"
		}
		return "sql:" + pg.Code
	}
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		return "sql:" + strconv.Itoa(int(my.Number))
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "sql:timeout"
	}
	kind := protocol.ClassifyErr(err)
	if kind == "canceled" {
		return kind
	}
	return "sql:" + kind
}

// bindValue makes a rendered template value acceptable to the drivers.
func bindValue(v any) any {
	switch x := v.(type) {
	case nil, string, bool, int64, float64, []byte, time.Time:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float32:
		return float64(x)
	case map[string]any, []any, map[string]string:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return tmpl.ToString(v)
}

// plain converts scanned values to JSON-friendly ones.
func plain(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	}
	return v
}

func valueSize(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		return int64(len(x))
	case bool:
		return 1
	}
	return 8
}

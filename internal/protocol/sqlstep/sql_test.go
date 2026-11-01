package sqlstep

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ymg4798/lt/internal/protocol"
)

func TestCompileQuery(t *testing.T) {
	cases := []struct {
		in    string
		style byte
		want  string
		args  []string
	}{
		{"SELECT * FROM t WHERE id = {{ id }}", placeholderDollar, "SELECT * FROM t WHERE id = $1", []string{"{{ id }}"}},
		{"SELECT * FROM t WHERE a = {{ a }} AND b = {{b}}", placeholderQuestion, "SELECT * FROM t WHERE a = ? AND b = ?", []string{"{{ a }}", "{{b}}"}},
		{"SELECT * FROM t WHERE name = '{{ n }}'", placeholderDollar, "SELECT * FROM t WHERE name = $1", []string{"{{ n }}"}},
		{"SELECT * FROM t WHERE name LIKE '%{{ q }}%' AND x = 'it''s'", placeholderDollar, "SELECT * FROM t WHERE name LIKE $1 AND x = 'it''s'", []string{"%{{ q }}%"}},
		{"SELECT '{{ not a tmpl' -- {{ x }}\n FROM t", placeholderDollar, "", nil}, // error: bad template inside literal
		{"SELECT \"col\" FROM t -- comment {{ x }}\nWHERE a = {{ a }}", placeholderDollar, "SELECT \"col\" FROM t -- comment {{ x }}\nWHERE a = $1", []string{"{{ a }}"}},
	}
	for _, c := range cases {
		got, args, err := compileQuery(c.in, c.style)
		if c.want == "" {
			if err == nil {
				t.Errorf("%q: want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%q:\n got %q\nwant %q", c.in, got, c.want)
		}
		if len(args) != len(c.args) {
			t.Fatalf("%q: %d args, want %d", c.in, len(args), len(c.args))
		}
		for i, a := range args {
			if a.Raw() != c.args[i] {
				t.Errorf("%q arg %d = %q, want %q", c.in, i, a.Raw(), c.args[i])
			}
		}
	}
	if _, _, err := compileQuery(`SELECT * FROM "{{ table }}"`, placeholderDollar); err == nil {
		t.Error("placeholder in identifier: want error")
	}
}

func TestDefaultName(t *testing.T) {
	for in, want := range map[string]string{
		"select id from users where id = {{ id }}":      "SQL SELECT users",
		"INSERT INTO orders(id, x) VALUES ({{ a }}, 1)": "SQL INSERT orders",
		"UPDATE accounts SET n = n + 1":                 "SQL UPDATE accounts",
		"CREATE TABLE IF NOT EXISTS t1 (id INT)":        "SQL CREATE t1",
		"drop table if exists t2":                       "SQL DROP t2",
		"TRUNCATE TABLE t3":                             "SQL TRUNCATE t3",
		"DELETE FROM sessions;":                         "SQL DELETE sessions",
		"SELECT 1":                                      "SQL SELECT",
		"SELECT * FROM (SELECT 1) x":                    "SQL SELECT",
	} {
		if got := defaultName(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestCountMatcher(t *testing.T) {
	for _, c := range []struct {
		spec any
		n    int64
		ok   bool
	}{{">= 1", 1, true}, {">= 1", 0, false}, {3, 3, true}, {"3", 2, false}, {"< 2", 1, true}, {"!= 0", 0, false}, {"> 0", 5, true}} {
		f, err := countMatcher(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		if f(c.n) != c.ok {
			t.Errorf("%v(%d) != %v", c.spec, c.n, c.ok)
		}
	}
	if _, err := countMatcher("lots"); err == nil {
		t.Error("want error")
	}
}

func TestCompileErrors(t *testing.T) {
	env := &protocol.Env{}
	for _, raw := range []map[string]any{
		{"dsn": "x", "query": "SELECT 1"},                     // no driver
		{"driver": "oracle", "dsn": "x", "query": "SELECT 1"}, // unknown driver
		{"driver": "postgres", "query": "SELECT 1"},           // no dsn
		{"driver": "postgres", "dsn": "x", "query": "DELETE FROM t", "exec": true, "check": map[string]any{"rows": 1}}, // rows on exec
		{"driver": "postgres", "dsn": "x", "query": "SELECT 1", "extract": map[string]any{"a": "row.x.y"}},
	} {
		if _, err := compile(raw, env); err == nil {
			t.Errorf("%v: want error", raw)
		}
	}
}

// Integration: LT_TEST_PG=postgres://lt:lt@localhost:15432/lt?sslmode=disable
// and/or LT_TEST_MYSQL=lt:lt@tcp(localhost:13306)/lt.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("LT_TEST_PG")
	if dsn == "" {
		t.Skip("LT_TEST_PG not set")
	}
	runDB(t, "postgres", dsn, "42P01")
}

func TestMySQL(t *testing.T) {
	dsn := os.Getenv("LT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("LT_TEST_MYSQL not set")
	}
	runDB(t, "mysql", dsn, "1146")
}

func runDB(t *testing.T, driver, dsn, missingTable string) {
	env := &protocol.Env{Options: protocol.Options{MaxInFlight: 8}}
	defer env.Close()
	var mu sync.Mutex
	var results []protocol.Result
	vu := protocol.NewVU(7, "t", map[string]any{"dsn": dsn, "name": "o'brien; DROP TABLE lt_sqlstep; --"}, func(r protocol.Result) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	})
	defer vu.Close()
	mk := func(m map[string]any) protocol.Step {
		t.Helper()
		m["driver"], m["dsn"] = driver, "{{ dsn }}"
		st, err := compile(m, env)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	ctx := context.Background()
	run := func(st protocol.Step) error { return st.Run(ctx, vu) }
	idType := "SERIAL PRIMARY KEY"
	if driver == "mysql" {
		idType = "INT AUTO_INCREMENT PRIMARY KEY"
	}
	for _, q := range []string{
		"DROP TABLE IF EXISTS lt_sqlstep",
		"CREATE TABLE lt_sqlstep (id " + idType + ", name VARCHAR(100), vu INT)",
	} {
		if err := run(mk(map[string]any{"query": q, "exec": true})); err != nil {
			t.Fatal(err)
		}
	}
	ins := mk(map[string]any{"query": "INSERT INTO lt_sqlstep (name, vu) VALUES ({{ name }}, {{ vu }})", "exec": true, "check": map[string]any{"affected": 1}})
	for range 3 {
		if err := run(ins); err != nil {
			t.Fatal(err)
		}
	}
	sel := mk(map[string]any{
		"query":   "SELECT id, name, vu FROM lt_sqlstep WHERE vu = {{ vu }} AND name LIKE '%{{ part }}%' ORDER BY id",
		"check":   map[string]any{"rows": ">= 3", "json": map[string]any{"$[0].name": "{{ name }}"}},
		"extract": map[string]any{"firstId": "row.0.id", "n": "rows", "nm": "$[1].name"},
	})
	vu.Vars["part"] = "brien"
	if err := run(sel); err != nil {
		t.Fatal(err)
	}
	if vu.Vars["n"] != int64(3) || vu.Vars["nm"] != vu.Vars["name"] || vu.Vars["firstId"] == nil {
		t.Fatalf("extract: %v %v %v", vu.Vars["n"], vu.Vars["nm"], vu.Vars["firstId"])
	}
	if sel.(*step).name != "SQL SELECT lt_sqlstep" {
		t.Errorf("name %q", sel.(*step).name)
	}
	// failing check
	vu.Vars["part"] = "nobody"
	if err := run(sel); err == nil || !strings.Contains(err.Error(), "check:rows") {
		t.Fatalf("want check:rows, got %v", err)
	}
	// SQL error classification
	err := run(mk(map[string]any{"query": "SELECT * FROM lt_missing_table"}))
	if err == nil || protocol.ClassifyErr(err) != "sql:"+missingTable {
		t.Fatalf("want sql:%s, got %v", missingTable, err)
	}
	// timeout
	slow := "SELECT pg_sleep(1)"
	if driver == "mysql" {
		slow = "SELECT SLEEP(1)"
	}
	err = run(mk(map[string]any{"query": slow, "timeout": "100ms"}))
	if driver == "postgres" && (err == nil || protocol.ClassifyErr(err) != "sql:timeout") {
		t.Fatalf("want sql:timeout, got %v", err)
	}
	if driver == "mysql" && err != nil && protocol.ClassifyErr(err) != "sql:timeout" {
		t.Fatalf("want sql:timeout, got %v", err)
	}
	if err := run(mk(map[string]any{"query": "DROP TABLE lt_sqlstep", "exec": true})); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) < 8 {
		t.Fatalf("only %d results", len(results))
	}
}

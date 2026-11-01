package monitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/run"
)

// Integration tests against real databases run only when given a DSN, e.g.
// with deploy/targets/docker-compose.yml up:
//
//	LT_TEST_PG='postgres://lt:lt@localhost:15432/lt?sslmode=disable' \
//	LT_TEST_MYSQL='lt:lt@tcp(localhost:13306)/lt' \
//	LT_TEST_REDIS='redis://:lt@localhost:16379/0' go test ./internal/monitor/ -run Integration -v

func dsnOrSkip(t *testing.T, env string) string {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		t.Skipf("%s not set", env)
	}
	return v
}

// exercise runs health twice around some load and returns the series and
// the Top SQL of that load.
func exercise(t *testing.T, src dbSource, load func(ctx context.Context) error) (map[string]val, []run.TopQuery) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got := map[string]val{}
	prev := map[string]point{}
	emit := func(n, u string, v float64) { got[n] = val{u, v} }
	same := func(s string) string { return s }
	base, err := src.statements(ctx)
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	hctx, hcancel := context.WithTimeout(ctx, healthTimeout)
	if err := src.health(hctx, newCalc(prev, time.Now(), emit, same)); err != nil {
		t.Fatalf("health: %v", err)
	}
	hcancel()
	if err := load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // PostgreSQL flushes its counters about once a second
	hctx, hcancel = context.WithTimeout(ctx, healthTimeout)
	if err := src.health(hctx, newCalc(prev, time.Now(), emit, same)); err != nil {
		t.Fatalf("health: %v", err)
	}
	hcancel()
	cur, err := src.statements(ctx)
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	top := topDelta("test", base, cur, topPerSource)
	for n, v := range got {
		t.Logf("%-26s %10.2f %s", n, v.v, v.unit)
	}
	for _, q := range top {
		t.Logf("top %6d calls %9.2fms %s", q.Calls, q.TotalMs, q.Query)
	}
	return got, top
}

func wantSeries(t *testing.T, got map[string]val, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, ok := got[n]; !ok {
			t.Errorf("missing %s", n)
		}
	}
}

func wantTop(t *testing.T, top []run.TopQuery, contains string, minCalls int64) {
	t.Helper()
	for _, q := range top {
		if strings.Contains(strings.ToLower(q.Query), strings.ToLower(contains)) && q.Calls >= minCalls {
			return
		}
	}
	t.Errorf("no top query with %q and ≥%d calls in %+v", contains, minCalls, top)
}

func TestIntegrationPostgres(t *testing.T) {
	dsn := dsnOrSkip(t, "LT_TEST_PG")
	src, _, err := newPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer src.close()
	app, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	got, top := exercise(t, src, func(ctx context.Context) error {
		if _, err := app.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS lt_it (id int PRIMARY KEY, v text)`); err != nil {
			return err
		}
		for i := range 200 {
			if _, err := app.ExecContext(ctx, `INSERT INTO lt_it VALUES ($1, 'x') ON CONFLICT (id) DO UPDATE SET v = excluded.v`, i); err != nil {
				return err
			}
			var n int
			if err := app.QueryRowContext(ctx, `SELECT count(*) FROM lt_it WHERE id < $1`, i).Scan(&n); err != nil {
				return err
			}
		}
		return nil
	})
	wantSeries(t, got, "pg connections", "pg active", "pg lock waits", "pg longest query", "pg longest transaction",
		"pg tps", "pg commits", "pg rollbacks", "pg deadlocks", "pg cache hit", "pg rows returned", "pg rows fetched")
	if got["pg tps"].v <= 0 {
		t.Errorf("pg tps = %v", got["pg tps"])
	}
	wantTop(t, top, "INSERT INTO lt_it", 200)
	wantTop(t, top, "FROM lt_it WHERE", 200)
	// A database without the extension: health works, Top SQL says why not.
	if u, err := url.Parse(dsn); err == nil {
		u.Path = "/postgres"
		other, _, err := newPostgres(u.String())
		if err != nil {
			t.Fatal(err)
		}
		defer other.close()
		_, err = other.statements(context.Background())
		var none noStatementsError
		if !errors.As(err, &none) {
			t.Logf("postgres database: %v (expected the extension to be missing there)", err)
		}
	}
	for _, q := range top {
		if strings.Contains(q.Query, "pg_stat") {
			t.Errorf("monitor's own query in top: %s", q.Query)
		}
	}
}

func TestIntegrationMySQL(t *testing.T) {
	dsn := dsnOrSkip(t, "LT_TEST_MYSQL")
	src, _, err := newMySQL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer src.close()
	// Text protocol: server-side prepared statements (the driver's default
	// for queries with arguments) are not summarized by digest.
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	app, err := sql.Open("mysql", dsn+sep+"interpolateParams=true")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	got, top := exercise(t, src, func(ctx context.Context) error {
		if _, err := app.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS lt_it (id int PRIMARY KEY, v varchar(10))`); err != nil {
			return err
		}
		for i := range 200 {
			if _, err := app.ExecContext(ctx, `REPLACE INTO lt_it VALUES (?, 'x')`, i); err != nil {
				return err
			}
			var n int
			if err := app.QueryRowContext(ctx, `SELECT count(*) FROM lt_it WHERE id < ?`, i).Scan(&n); err != nil {
				return err
			}
		}
		return nil
	})
	wantSeries(t, got, "mysql connections", "mysql threads running", "mysql qps", "mysql commits", "mysql rollbacks",
		"mysql slow queries", "mysql row lock waits", "mysql lock waits", "mysql aborted connects", "mysql longest query")
	if got["mysql qps"].v < 100 {
		t.Errorf("mysql qps = %v", got["mysql qps"])
	}
	wantTop(t, top, "REPLACE INTO `lt_it`", 200)
	wantTop(t, top, "FROM `lt_it`", 200)
}

func TestIntegrationRedis(t *testing.T) {
	dsn := dsnOrSkip(t, "LT_TEST_REDIS")
	src, _, err := newRedis(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer src.close()
	app, _, _ := newRedis(dsn)
	defer app.close()
	got, top := exercise(t, src, func(ctx context.Context) error {
		for i := range 300 {
			k := fmt.Sprintf("lt:it:%d", i%50)
			if _, err := app.do(ctx, "SET", k, "v"); err != nil {
				return err
			}
			if _, err := app.do(ctx, "GET", k); err != nil {
				return err
			}
			if _, err := app.do(ctx, "GET", "lt:it:missing"); err != nil {
				return err
			}
		}
		return nil
	})
	wantSeries(t, got, "redis clients", "redis blocked clients", "redis ops", "redis memory", "redis hit",
		"redis evicted keys", "redis expired keys", "redis cpu", "redis keys")
	if h := got["redis hit"].v; h < 45 || h > 55 { // half the GETs hit
		t.Errorf("redis hit = %v", h)
	}
	wantTop(t, top, "GET", 600)
	wantTop(t, top, "SET", 300)
}

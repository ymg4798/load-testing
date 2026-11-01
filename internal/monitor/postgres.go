package monitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The monitor's own sessions and statements are marked so they are left
// out of connection counts and Top SQL.
const (
	appName = "lt-monitor"
	marker  = "/*lt-monitor*/ "
)

type pgSource struct {
	db       *sql.DB
	totalCol string // total_exec_time (13+) or total_time
}

func newPostgres(dsn string) (*pgSource, string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, "", fmt.Errorf("postgres dsn: %w", err)
	}
	if cfg.ConnectTimeout == 0 || cfg.ConnectTimeout > 2*time.Second {
		cfg.ConnectTimeout = 2 * time.Second
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = appName
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)
	host := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return &pgSource{db: db, totalCol: "total_exec_time"}, host, nil
}

const pgHealthSQL = marker + `SELECT
  a.conns, a.active, a.waiting, a.longest_query, a.longest_tx,
  d.commits, d.rollbacks, d.hit, d.read, d.deadlocks, d.returned, d.fetched,
  (SELECT CASE WHEN pg_is_in_recovery()
     THEN COALESCE(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()), 0)::float8 END)
FROM
  (SELECT count(*)::float8 AS conns,
          (count(*) FILTER (WHERE state = 'active'))::float8 AS active,
          (count(*) FILTER (WHERE wait_event_type = 'Lock'))::float8 AS waiting,
          COALESCE(EXTRACT(EPOCH FROM max(now() - query_start) FILTER (WHERE state = 'active')), 0)::float8 AS longest_query,
          COALESCE(EXTRACT(EPOCH FROM max(now() - xact_start)), 0)::float8 AS longest_tx
   FROM pg_stat_activity
   WHERE backend_type = 'client backend' AND application_name <> '` + appName + `') a,
  (SELECT COALESCE(sum(xact_commit), 0)::float8 AS commits,
          COALESCE(sum(xact_rollback), 0)::float8 AS rollbacks,
          COALESCE(sum(blks_hit), 0)::float8 AS hit,
          COALESCE(sum(blks_read), 0)::float8 AS read,
          COALESCE(sum(deadlocks), 0)::float8 AS deadlocks,
          COALESCE(sum(tup_returned), 0)::float8 AS returned,
          COALESCE(sum(tup_fetched), 0)::float8 AS fetched
   FROM pg_stat_database) d`

func (p *pgSource) health(ctx context.Context, c *calc) error {
	var conns, active, waiting, longQ, longTx, commits, rollbacks, hit, read, deadlocks, returned, fetched float64
	var lag sql.NullFloat64
	err := p.db.QueryRowContext(ctx, pgHealthSQL).Scan(&conns, &active, &waiting, &longQ, &longTx,
		&commits, &rollbacks, &hit, &read, &deadlocks, &returned, &fetched, &lag)
	if err != nil {
		return err
	}
	c.gauge("pg connections", "", conns)
	c.gauge("pg active", "", active)
	c.gauge("pg lock waits", "", waiting)
	c.gauge("pg longest query", "ms", longQ*1000)
	c.gauge("pg longest transaction", "ms", longTx*1000)
	c.rate("pg tps", commits+rollbacks)
	c.rate("pg commits", commits)
	c.rate("pg rollbacks", rollbacks)
	c.rate("pg deadlocks", deadlocks)
	c.hitRatio("pg cache hit", hit, read)
	c.rate("pg rows returned", returned)
	c.rate("pg rows fetched", fetched)
	if lag.Valid {
		c.gauge("pg replication lag", "ms", lag.Float64*1000)
	}
	return nil
}

func (p *pgSource) statements(ctx context.Context) (map[string]stmtStat, error) {
	q := marker + `SELECT userid::text || '/' || dbid::text || '/' || queryid::text, query, calls, ` +
		p.totalCol + `::float8, rows FROM pg_stat_statements WHERE queryid IS NOT NULL`
	rows, err := p.db.QueryContext(ctx, q)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			switch pe.Code {
			case "42P01": // undefined_table
				return nil, noStatementsError{"pg_stat_statements is not installed (CREATE EXTENSION pg_stat_statements); Top SQL is off"}
			case "55000": // not in shared_preload_libraries
				return nil, noStatementsError{pe.Message + "; Top SQL is off"}
			case "42703": // undefined_column: PostgreSQL 12 or older
				if p.totalCol != "total_time" {
					p.totalCol = "total_time"
					return p.statements(ctx)
				}
			}
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]stmtStat{}
	for rows.Next() {
		var key, query string
		var calls, nrows int64
		var total float64
		if err := rows.Scan(&key, &query, &calls, &total, &nrows); err != nil {
			return nil, err
		}
		if strings.Contains(query, marker[:len(marker)-1]) {
			continue
		}
		s := out[key] // toplevel and nested rows share a key: add them
		s.query = query
		s.calls += calls
		s.totalMs += total
		s.rows += nrows
		out[key] = s
	}
	return out, rows.Err()
}

func (p *pgSource) close() { p.db.Close() }

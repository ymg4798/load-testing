package monitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type mysqlSource struct {
	db      *sql.DB
	replica int // 0 unknown, 1 replica, -1 not a replica (or no privilege)
}

// parseMySQLDSN accepts the driver's form (user:pass@tcp(host:3306)/db)
// and a URL (mysql://user:pass@host:3306/db?param=v).
func parseMySQLDSN(dsn string) (*mysql.Config, error) {
	if strings.HasPrefix(dsn, "mysql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, err
		}
		cfg := mysql.NewConfig()
		cfg.Net = "tcp"
		cfg.Addr = u.Host
		if u.Port() == "" {
			cfg.Addr = u.Host + ":3306"
		}
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		// Re-parse so query parameters (tls=true, ...) are handled by the driver.
		if q := u.RawQuery; q != "" {
			base, sep := cfg.FormatDSN(), "?"
			if strings.Contains(base, "?") {
				sep = "&"
			}
			with, err := mysql.ParseDSN(base + sep + q)
			if err != nil {
				return nil, err
			}
			cfg = with
		}
		return cfg, nil
	}
	return mysql.ParseDSN(dsn)
}

func newMySQL(dsn string) (*mysqlSource, string, error) {
	cfg, err := parseMySQLDSN(dsn)
	if err != nil {
		return nil, "", fmt.Errorf("mysql dsn: %w", err)
	}
	for _, d := range []*time.Duration{&cfg.Timeout, &cfg.ReadTimeout, &cfg.WriteTimeout} {
		if *d == 0 || *d > 2*time.Second {
			*d = 2 * time.Second
		}
	}
	if cfg.ConnectionAttributes == "" {
		cfg.ConnectionAttributes = "program_name:" + appName
	}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("mysql dsn: %w", err)
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)
	return &mysqlSource{db: db}, cfg.Addr, nil
}

var mysqlStatusVars = []string{
	"Threads_connected", "Threads_running", "Questions", "Com_commit", "Com_rollback",
	"Slow_queries", "Innodb_row_lock_waits", "Innodb_row_lock_current_waits",
	"Innodb_buffer_pool_read_requests", "Innodb_buffer_pool_reads", "Aborted_connects",
}

func (s *mysqlSource) health(ctx context.Context, c *calc) error {
	q := "SHOW GLOBAL STATUS WHERE Variable_name IN ('" + strings.Join(mysqlStatusVars, "','") + "')"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	st := map[string]float64{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			st[strings.ToLower(k)] = f
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if v, ok := st["threads_connected"]; ok {
		// Leave out the monitor's own connections.
		own := float64(s.db.Stats().OpenConnections)
		c.gauge("mysql connections", "", maxf(0, v-own))
	}
	if v, ok := st["threads_running"]; ok {
		c.gauge("mysql threads running", "", maxf(0, v-1)) // minus this query
	}
	rate := func(name, key string) {
		if v, ok := st[key]; ok {
			c.rate(name, v)
		}
	}
	rate("mysql qps", "questions")
	rate("mysql commits", "com_commit")
	rate("mysql rollbacks", "com_rollback")
	rate("mysql slow queries", "slow_queries")
	rate("mysql row lock waits", "innodb_row_lock_waits")
	if v, ok := st["innodb_row_lock_current_waits"]; ok {
		c.gauge("mysql lock waits", "", v)
	}
	req, ok1 := st["innodb_buffer_pool_read_requests"]
	disk, ok2 := st["innodb_buffer_pool_reads"]
	if ok1 && ok2 {
		c.hitRatio("mysql buffer pool hit", maxf(0, req-disk), disk)
	}
	rate("mysql aborted connects", "aborted_connects")

	var longest float64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(TIME), 0) FROM information_schema.PROCESSLIST "+
		"WHERE COMMAND = 'Query' AND ID <> CONNECTION_ID()").Scan(&longest); err == nil {
		c.gauge("mysql longest query", "ms", longest*1000)
	} else if ctx.Err() != nil {
		return err
	}
	if s.replica >= 0 {
		lag, ok, err := s.replicaLag(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return err
		case err != nil || (!ok && s.replica == 0):
			s.replica = -1
		case ok:
			s.replica = 1
			c.gauge("mysql replication lag", "ms", lag*1000)
		}
	}
	return nil
}

// replicaLag reads Seconds_Behind_Source from SHOW REPLICA STATUS
// (SHOW SLAVE STATUS before 8.0.22 and on MariaDB).
func (s *mysqlSource) replicaLag(ctx context.Context) (float64, bool, error) {
	rows, err := s.db.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		if rows, err = s.db.QueryContext(ctx, "SHOW SLAVE STATUS"); err != nil {
			return 0, false, err
		}
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || !rows.Next() {
		return 0, false, err
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return 0, false, err
	}
	for i, c := range cols {
		if c == "Seconds_Behind_Source" || c == "Seconds_Behind_Master" {
			v, err := strconv.ParseFloat(string(vals[i]), 64)
			return v, err == nil, nil // NULL: replication stopped
		}
	}
	return 0, false, nil
}

func (s *mysqlSource) statements(ctx context.Context) (map[string]stmtStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(SCHEMA_NAME, ''), DIGEST, DIGEST_TEXT, COUNT_STAR,
  SUM_TIMER_WAIT, SUM_ROWS_SENT
FROM performance_schema.events_statements_summary_by_digest WHERE DIGEST IS NOT NULL`)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && (me.Number == 1146 || me.Number == 1142) {
			return nil, noStatementsError{"cannot read performance_schema.events_statements_summary_by_digest (" +
				me.Message + "); Top SQL is off"}
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]stmtStat{}
	for rows.Next() {
		var schema, digest string
		var text sql.NullString
		var calls, timerPs, sent float64 // unsigned 64-bit columns
		if err := rows.Scan(&schema, &digest, &text, &calls, &timerPs, &sent); err != nil {
			return nil, err
		}
		if !text.Valid || isMonitorSQL(text.String) {
			continue
		}
		out[schema+"/"+digest] = stmtStat{
			query:   text.String,
			calls:   int64(calls),
			totalMs: timerPs / 1e9, // picoseconds
			rows:    int64(sent),
		}
	}
	return out, rows.Err()
}

// isMonitorSQL spots the statements the monitor itself sends (MySQL's
// digest text drops comments, so they cannot be marked).
func isMonitorSQL(q string) bool {
	u := strings.ToUpper(q)
	return strings.HasPrefix(u, "SHOW ") || strings.Contains(u, "`PERFORMANCE_SCHEMA`") ||
		strings.Contains(u, "`INFORMATION_SCHEMA`")
}

func (s *mysqlSource) close() { s.db.Close() }

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

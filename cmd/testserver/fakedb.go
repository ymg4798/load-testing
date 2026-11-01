package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"regexp"
	"strconv"
	"time"
)

// fakedb is a database/sql driver that runs no SQL: a statement takes as
// long as the duration in its first comment ("SELECT /* 12ms */ ...") and
// returns one row with one column. It makes database time visible through
// the APM agent without a real database.
func init() { sql.Register("fakedb", fakeDriver{}) }

var sqlDelay = regexp.MustCompile(`/\*\s*(\d+)\s*ms\s*\*/`)

func queryDelay(ctx context.Context, q string) error {
	m := sqlDelay.FindStringSubmatch(q)
	if m == nil {
		return ctx.Err()
	}
	ms, _ := strconv.Atoi(m[1])
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(q string) (driver.Stmt, error) { return fakeStmt{q}, nil }
func (fakeConn) Close() error                          { return nil }
func (fakeConn) Begin() (driver.Tx, error)             { return fakeTx{}, nil }

func (fakeConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := queryDelay(ctx, q); err != nil {
		return nil, err
	}
	return &fakeRows{}, nil
}

func (fakeConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	if err := queryDelay(ctx, q); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

type fakeStmt struct{ q string }

func (s fakeStmt) Close() error  { return nil }
func (s fakeStmt) NumInput() int { return -1 }

func (s fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return fakeConn{}.ExecContext(context.Background(), s.q, nil)
}

func (s fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	return fakeConn{}.QueryContext(context.Background(), s.q, nil)
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct{ done bool }

func (r *fakeRows) Columns() []string { return []string{"value"} }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

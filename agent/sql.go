package agent

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
)

// OpenDB is sql.Open with every statement traced: a client span carrying
// the SQL text, and the SQL as the request's current activity while it
// runs. db.system is the driver name.
func OpenDB(driverName, dsn string) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	d := db.Driver()
	_ = db.Close()
	c, err := (&wDriver{d: d, system: driverName}).OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

// WrapDriver returns a driver that traces every statement run through d,
// for sql.Register or sql.OpenDB.
func WrapDriver(d driver.Driver) driver.Driver { return &wDriver{d: d, system: "other_sql"} }

const maxSQL = 2048

// dbSpan starts a span for one statement; finish ends it (or discards it
// on driver.ErrSkip, when database/sql retries another way).
func dbSpan(ctx context.Context, system, query string) (finish func(error)) {
	s := newSpan(ctx, nil, sqlVerb(query), KindClient)
	if s == nil {
		return func(error) {}
	}
	q := query
	if len(q) > maxSQL {
		q = q[:maxSQL]
	}
	s.attrs = append(s.attrs, attr{"db.system", system}, attr{"db.query.text", q})
	act := strings.Join(strings.Fields(query), " ")
	if len(act) > 300 {
		act = act[:300] + "..."
	}
	restore := setActivity(ctx, act)
	return func(err error) {
		restore()
		if errors.Is(err, driver.ErrSkip) {
			return
		}
		s.End(err)
	}
}

// sqlVerb is the statement's first word, as the span name.
func sqlVerb(q string) string {
	q = strings.TrimSpace(q)
	for strings.HasPrefix(q, "/*") {
		i := strings.Index(q, "*/")
		if i < 0 {
			break
		}
		q = strings.TrimSpace(q[i+2:])
	}
	if i := strings.IndexAny(q, " \t\r\n("); i >= 0 {
		q = q[:i]
	}
	if q == "" {
		return "SQL"
	}
	return strings.ToUpper(q)
}

func namedToValues(args []driver.NamedValue) ([]driver.Value, error) {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		if a.Name != "" {
			return nil, errors.New("agent: driver does not support named parameters")
		}
		out[i] = a.Value
	}
	return out, nil
}

type wDriver struct {
	d      driver.Driver
	system string
}

func (w *wDriver) Open(name string) (driver.Conn, error) {
	c, err := w.d.Open(name)
	if err != nil {
		return nil, err
	}
	return &wConn{c: c, system: w.system}, nil
}

func (w *wDriver) OpenConnector(name string) (driver.Connector, error) {
	if dc, ok := w.d.(driver.DriverContext); ok {
		c, err := dc.OpenConnector(name)
		if err != nil {
			return nil, err
		}
		return &wConnector{c: c, d: w}, nil
	}
	return &wConnector{c: dsnConnector{name: name, d: w.d}, d: w}, nil
}

type dsnConnector struct {
	name string
	d    driver.Driver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.d.Open(c.name) }
func (c dsnConnector) Driver() driver.Driver                        { return c.d }

type wConnector struct {
	c driver.Connector
	d *wDriver
}

func (w *wConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c, err := w.c.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &wConn{c: c, system: w.d.system}, nil
}

func (w *wConnector) Driver() driver.Driver { return w.d }

func (w *wConnector) Close() error {
	if c, ok := w.c.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type wConn struct {
	c      driver.Conn
	system string
}

var (
	_ driver.ConnPrepareContext = (*wConn)(nil)
	_ driver.ConnBeginTx        = (*wConn)(nil)
	_ driver.ExecerContext      = (*wConn)(nil)
	_ driver.QueryerContext     = (*wConn)(nil)
	_ driver.Pinger             = (*wConn)(nil)
	_ driver.SessionResetter    = (*wConn)(nil)
	_ driver.Validator          = (*wConn)(nil)
	_ driver.NamedValueChecker  = (*wConn)(nil)
)

func (w *wConn) Prepare(q string) (driver.Stmt, error) {
	s, err := w.c.Prepare(q)
	if err != nil {
		return nil, err
	}
	return &wStmt{s: s, query: q, system: w.system}, nil
}

func (w *wConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	var (
		s   driver.Stmt
		err error
	)
	if pc, ok := w.c.(driver.ConnPrepareContext); ok {
		s, err = pc.PrepareContext(ctx, q)
	} else {
		if err = ctx.Err(); err == nil {
			s, err = w.c.Prepare(q)
		}
	}
	if err != nil {
		return nil, err
	}
	return &wStmt{s: s, query: q, system: w.system}, nil
}

func (w *wConn) Close() error { return w.c.Close() }

// Begin is required by driver.Conn; database/sql uses BeginTx.
func (w *wConn) Begin() (driver.Tx, error) { return w.c.Begin() } //lint:ignore SA1019 required by driver.Conn

func (w *wConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := w.c.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	if opts.Isolation != 0 || opts.ReadOnly {
		return nil, errors.New("agent: driver does not support transaction options")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return w.c.Begin() //lint:ignore SA1019 fallback for old drivers
}

func (w *wConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	switch c := w.c.(type) {
	case driver.ExecerContext:
		finish := dbSpan(ctx, w.system, q)
		r, err := c.ExecContext(ctx, q, args)
		finish(err)
		return r, err
	case driver.Execer: //lint:ignore SA1019 fallback for old drivers
		vals, err := namedToValues(args)
		if err != nil {
			return nil, err
		}
		finish := dbSpan(ctx, w.system, q)
		r, err := c.Exec(q, vals)
		finish(err)
		return r, err
	}
	return nil, driver.ErrSkip // database/sql prepares the statement instead
}

func (w *wConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	switch c := w.c.(type) {
	case driver.QueryerContext:
		finish := dbSpan(ctx, w.system, q)
		r, err := c.QueryContext(ctx, q, args)
		finish(err)
		return r, err
	case driver.Queryer: //lint:ignore SA1019 fallback for old drivers
		vals, err := namedToValues(args)
		if err != nil {
			return nil, err
		}
		finish := dbSpan(ctx, w.system, q)
		r, err := c.Query(q, vals)
		finish(err)
		return r, err
	}
	return nil, driver.ErrSkip
}

func (w *wConn) Ping(ctx context.Context) error {
	if p, ok := w.c.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (w *wConn) ResetSession(ctx context.Context) error {
	if r, ok := w.c.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (w *wConn) IsValid() bool {
	if v, ok := w.c.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (w *wConn) CheckNamedValue(nv *driver.NamedValue) error {
	if c, ok := w.c.(driver.NamedValueChecker); ok {
		return c.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type wStmt struct {
	s      driver.Stmt
	query  string
	system string
}

var (
	_ driver.StmtExecContext   = (*wStmt)(nil)
	_ driver.StmtQueryContext  = (*wStmt)(nil)
	_ driver.NamedValueChecker = (*wStmt)(nil)
)

func (w *wStmt) Close() error  { return w.s.Close() }
func (w *wStmt) NumInput() int { return w.s.NumInput() }

func (w *wStmt) Exec(args []driver.Value) (driver.Result, error) {
	finish := dbSpan(context.Background(), w.system, w.query)
	r, err := w.s.Exec(args) //lint:ignore SA1019 required by driver.Stmt
	finish(err)
	return r, err
}

func (w *wStmt) Query(args []driver.Value) (driver.Rows, error) {
	finish := dbSpan(context.Background(), w.system, w.query)
	r, err := w.s.Query(args) //lint:ignore SA1019 required by driver.Stmt
	finish(err)
	return r, err
}

func (w *wStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	finish := dbSpan(ctx, w.system, w.query)
	var (
		r   driver.Result
		err error
	)
	if ec, ok := w.s.(driver.StmtExecContext); ok {
		r, err = ec.ExecContext(ctx, args)
	} else {
		var vals []driver.Value
		if vals, err = namedToValues(args); err == nil {
			if err = ctx.Err(); err == nil {
				r, err = w.s.Exec(vals) //lint:ignore SA1019 fallback for old drivers
			}
		}
	}
	finish(err)
	return r, err
}

func (w *wStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	finish := dbSpan(ctx, w.system, w.query)
	var (
		r   driver.Rows
		err error
	)
	if qc, ok := w.s.(driver.StmtQueryContext); ok {
		r, err = qc.QueryContext(ctx, args)
	} else {
		var vals []driver.Value
		if vals, err = namedToValues(args); err == nil {
			if err = ctx.Err(); err == nil {
				r, err = w.s.Query(vals) //lint:ignore SA1019 fallback for old drivers
			}
		}
	}
	finish(err)
	return r, err
}

func (w *wStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if c, ok := w.s.(driver.NamedValueChecker); ok {
		return c.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

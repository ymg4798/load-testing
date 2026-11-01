package monitor

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymg4798/load-testing/internal/run"
)

const (
	healthTimeout = 900 * time.Millisecond // one scrape must end before the next tick
	stmtTimeout   = 5 * time.Second        // statement statistics at start and finish
	topPerSource  = 20
	maxQueryLen   = 1000
)

// dbSource is a direct database connection.
type dbSource interface {
	// health reads the server's status counters and reports them through c.
	health(ctx context.Context, c *calc) error
	// statements returns the server's cumulative statement statistics,
	// keyed by a stable id. errNoStatements means the server has none to
	// give (extension missing, feature off): warn once and stop asking.
	statements(ctx context.Context) (map[string]stmtStat, error)
	close()
}

// stmtStat is one statement's totals since the server's stats were reset.
type stmtStat struct {
	query   string
	calls   int64
	totalMs float64
	rows    int64
}

type noStatementsError struct{ msg string }

func (e noStatementsError) Error() string { return e.msg }

// topDelta subtracts the statistics at the start of the test from the
// current ones and returns the busiest statements by total time.
func topDelta(source string, base, cur map[string]stmtStat, limit int) []run.TopQuery {
	var out []run.TopQuery
	for k, c := range cur {
		d := c
		if b, ok := base[k]; ok && c.calls >= b.calls {
			d.calls -= b.calls
			d.totalMs -= b.totalMs
			d.rows -= b.rows
		} // else: new since the start, or stats were reset: count it all
		if d.calls <= 0 {
			continue
		}
		if d.totalMs < 0 {
			d.totalMs = 0
		}
		if d.rows < 0 {
			d.rows = 0
		}
		out = append(out, run.TopQuery{
			Source:  source,
			Query:   cleanQuery(d.query),
			Calls:   d.calls,
			TotalMs: d.totalMs,
			MeanMs:  d.totalMs / float64(d.calls),
			Rows:    d.rows,
		})
	}
	sortTop(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortTop(q []run.TopQuery) {
	sort.Slice(q, func(i, j int) bool {
		if q[i].TotalMs != q[j].TotalMs {
			return q[i].TotalMs > q[j].TotalMs
		}
		if q[i].Calls != q[j].Calls {
			return q[i].Calls > q[j].Calls
		}
		return q[i].Query < q[j].Query
	})
}

// cleanQuery collapses whitespace and shortens very long statements.
func cleanQuery(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxQueryLen {
		cut := maxQueryLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

package apm

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/ymg4798/lt/internal/run"
)

// Service map and error groups, both summed at ingest (traces are evicted,
// these are kept for the whole run).
//
// Map node IDs (run.ServiceMap):
//
//	service:<service.name>                  a service that sent server spans
//	db:<db.system>[@<server.address>][/<db.name>]  a database its client spans called
//	http:<host>[:<port>]                    an outside host its HTTP client spans called
//	client:lt                               callers outside the traced services (lt's requests)
//
// An HTTP call answered by a traced service (its server span's parent is
// the client span) is drawn as an edge to that service, not to the host.

const (
	MaxMapEdges    = 500
	MaxErrorGroups = 500
	maxStacks      = 2_000 // distinct stack texts kept on stored spans
)

// mapTarget is the callee of an edge.
type mapTarget struct {
	kind, name, addr, db string // kind: client, service, db, http
}

func (t mapTarget) id() string {
	switch t.kind {
	case "db":
		return "db:" + t.label()
	case "http":
		return "http:" + t.addr
	}
	return t.kind + ":" + t.name
}

// label is the node's display name.
func (t mapTarget) label() string {
	switch t.kind {
	case "db":
		s := t.name
		if t.addr != "" {
			s += "@" + t.addr
		}
		if t.db != "" {
			s += "/" + t.db
		}
		return s
	case "http":
		return t.addr
	}
	return t.name
}

var ltClient = mapTarget{kind: "client", name: "lt"}

type edgeKey struct {
	from, to mapTarget
}

type edgeStat struct {
	count, errors, totalUs int64
	hist                   *hdr.Histogram
}

type nodeStat struct{ count, errors, totalUs int64 }

type aliasKey struct{ service, addr string }

type errKey struct{ service, source, message string }

type errGroup struct {
	count       int64
	first, last int64 // unix nanos
	traceID     string
	stack       string
}

// callTarget is what a db or http client span called.
func callTarget(s *span) (mapTarget, bool) {
	a := s.attrs
	switch s.kind {
	case KindDB:
		sys := a["db.system"]
		if sys == "" {
			sys = "sql"
		}
		return mapTarget{kind: "db", name: sys, addr: first(a["server.address"], a["net.peer.name"]), db: first(a["db.name"], a["db.namespace"])}, true
	case KindHTTP:
		host := first(a["server.address"], a["net.peer.name"])
		port := first(a["server.port"], a["net.peer.port"])
		if host == "" {
			if u, err := url.Parse(first(a["url.full"], a["http.url"])); err == nil {
				host, port = u.Hostname(), first(port, u.Port())
			}
		}
		if host == "" {
			host = first(a["peer.service"], a["http.host"])
		}
		if host == "" {
			return mapTarget{}, false
		}
		addr := host
		if port != "" {
			addr = net.JoinHostPort(host, port)
		}
		return mapTarget{kind: "http", addr: addr}, true
	}
	return mapTarget{}, false
}

func serviceTarget(name string) mapTarget { return mapTarget{kind: "service", name: name} }

// mapSpan counts a span on the map at ingest; t is callTarget(s) for db
// and http spans. c.mu held.
func (c *Collector) mapSpan(s *span, t mapTarget) {
	switch s.kind {
	case KindServer:
		n := c.nodes[s.service]
		if n == nil {
			n = &nodeStat{}
			c.nodes[s.service] = n
		}
		n.count++
		if s.err {
			n.errors++
		}
		n.totalUs += s.durUs()
	case KindDB, KindHTTP:
		if t.kind != "" {
			c.addEdge(edgeKey{serviceTarget(s.service), t}, s)
		}
	}
}

func (c *Collector) addEdge(k edgeKey, s *span) {
	e := c.edges[k]
	if e == nil {
		if len(c.edges) >= MaxMapEdges {
			c.edgesDropped++
			return
		}
		e = &edgeStat{hist: newOpHist()}
		c.edges[k] = e
	}
	d := s.durUs()
	e.count++
	if s.err {
		e.errors++
	}
	e.totalUs += d
	_ = e.hist.RecordValue(max(d, 1))
}

// resolved is called once per server span when its caller is known: par
// is the parent span, or nil when it is not in the traces (lt, or an
// untraced caller). Entry points go to the hitmap queue.
func (c *Collector) resolved(s, par *span) {
	if par != nil && par.kind == KindHTTP {
		if t, ok := callTarget(par); ok {
			c.aliases[aliasKey{par.service, t.addr}] = s.service
		}
	}
	if par != nil && par.service == s.service {
		return // a call within the service
	}
	switch {
	case par == nil:
		c.addEdge(edgeKey{ltClient, serviceTarget(s.service)}, s)
	case par.kind != KindHTTP: // no client span in between
		c.addEdge(edgeKey{serviceTarget(par.service), serviceTarget(s.service)}, s)
	}
	if len(c.done) < maxQueued {
		c.done = append(c.done, s)
	}
}

// Map returns the service map so far.
func (c *Collector) Map() *run.ServiceMap {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expirePending(c.now())
	return c.mapLocked()
}

func (c *Collector) mapLocked() *run.ServiceMap {
	type merged struct {
		from, to mapTarget
		parts    []*edgeStat
	}
	byIDs := map[[2]string]*merged{}
	for k, e := range c.edges {
		to := k.to
		if to.kind == "http" && k.from.kind == "service" {
			if svc, ok := c.aliases[aliasKey{k.from.name, to.addr}]; ok {
				to = serviceTarget(svc)
			}
		}
		key := [2]string{k.from.id(), to.id()}
		m := byIDs[key]
		if m == nil {
			m = &merged{from: k.from, to: to}
			byIDs[key] = m
		}
		m.parts = append(m.parts, e)
	}
	nodes := map[string]*run.MapNode{}
	node := func(t mapTarget) *run.MapNode {
		id := t.id()
		n := nodes[id]
		if n == nil {
			n = &run.MapNode{ID: id, Name: t.label(), Kind: t.kind}
			nodes[id] = n
		}
		return n
	}
	sum := map[string]int64{} // totalUs per non-service node, for the mean
	out := &run.ServiceMap{}
	for _, m := range byIDs {
		e := run.MapEdge{From: m.from.id(), To: m.to.id()}
		var totalUs int64
		h := m.parts[0].hist
		if len(m.parts) > 1 {
			h = newOpHist()
		}
		for _, p := range m.parts {
			e.Count += p.count
			e.Errors += p.errors
			totalUs += p.totalUs
			if len(m.parts) > 1 {
				h.Merge(p.hist)
			}
		}
		if e.Count > 0 {
			e.MeanMs = float64(totalUs) / float64(e.Count) / 1000
		}
		e.P95Ms = float64(h.ValueAtQuantile(95)) / 1000
		out.Edges = append(out.Edges, e)
		// Services count their own server spans (below); client:lt counts
		// what it sent, databases and hosts what they received.
		for _, t := range [2]mapTarget{m.from, m.to} {
			n := node(t)
			if t.kind == "service" || (t == m.to) == (t.kind == "client") {
				continue
			}
			n.Count += e.Count
			n.Errors += e.Errors
			sum[n.ID] += totalUs
		}
	}
	for svc, st := range c.nodes {
		n := node(serviceTarget(svc))
		n.Count, n.Errors = st.count, st.errors
		if st.count > 0 {
			n.MeanMs = float64(st.totalUs) / float64(st.count) / 1000
		}
	}
	for id, us := range sum {
		if n := nodes[id]; n.Count > 0 {
			n.MeanMs = float64(us) / float64(n.Count) / 1000
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	rank := map[string]int{"client": 0, "service": 1, "db": 2, "http": 3}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, *n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		a, b := out.Nodes[i], out.Nodes[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		return a.ID < b.ID
	})
	sort.Slice(out.Edges, func(i, j int) bool {
		a, b := out.Edges[i], out.Edges[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	return out
}

// ---- errors ----

// failure is what ingest learned about a failed span before taking the lock.
type failure struct {
	source, message, stack string
}

// spanFailure says whether a span failed (error status, a server 5xx or an
// exception event) and how it is grouped.
func spanFailure(s *span, r *rawSpan) (failure, bool) {
	if !s.err && r.exc == nil {
		return failure{}, false
	}
	f := failure{source: "span", stack: s.attrs["exception.stacktrace"]}
	msg := s.attrs["exception.message"] // the exception event's, else the status message
	switch {
	case s.kind == KindServer && s.status >= 500 && (msg == "" || msg == statusText(s.status)):
		f.message = "HTTP " + strconv.Itoa(s.status)
	case msg != "":
		f.message = NormalizeError(msg)
	default:
		f.message = s.name
	}
	if e := r.exc; e != nil && (e.escaped || e.typ == "panic" || strings.HasPrefix(e.message, "panic")) {
		f.source = "panic"
	}
	return f, true
}

// statusText is http.StatusText without importing net/http here.
func statusText(code int) string {
	switch code {
	case 500:
		return "Internal Server Error"
	case 501:
		return "Not Implemented"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	case 504:
		return "Gateway Timeout"
	}
	return ""
}

func (c *Collector) countError(s *span, f failure) {
	k := errKey{s.service, f.source, f.message}
	g := c.errs[k]
	if g == nil {
		if len(c.errs) >= MaxErrorGroups {
			k.message = "(other errors)"
			g = c.errs[k]
		}
		if g == nil {
			g = &errGroup{first: s.end}
			c.errs[k] = g
		}
	}
	g.count++
	g.first = min(g.first, s.end)
	if s.end >= g.last {
		g.last = s.end
		g.traceID = s.traceID
	}
	if g.stack == "" {
		g.stack = f.stack
	}
}

// Errors returns the failures seen so far, grouped by service and
// normalized message, most frequent first.
func (c *Collector) Errors() []run.ErrorGroup {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errorsLocked()
}

func (c *Collector) errorsLocked() []run.ErrorGroup {
	out := make([]run.ErrorGroup, 0, len(c.errs))
	for k, g := range c.errs {
		out = append(out, run.ErrorGroup{
			Service: k.service, Source: k.source, Message: k.message,
			Count: g.count, First: time.Unix(0, g.first).UTC(), Last: time.Unix(0, g.last).UTC(),
			TraceID: g.traceID, Stack: g.stack,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Service+out[i].Message < out[j].Service+out[j].Message
	})
	return out
}

var (
	reUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reHex0x  = regexp.MustCompile(`0[xX][0-9a-fA-F]+`)
	reLongHx = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	reSQuote = regexp.MustCompile(`'[^']*'`)
	reDQuote = regexp.MustCompile(`"[^"]*"`)
	reNumber = regexp.MustCompile(`\d+(?:[.:]\d+)*`)
)

const maxGroupMessage = 300

// NormalizeError turns an error message into its group key: the first
// line, with IDs, quoted values and numbers replaced by "?", so
// "order 42 not found" and "order 7 not found" are one error.
func NormalizeError(msg string) string {
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.Join(strings.Fields(msg), " ")
	msg = reUUID.ReplaceAllString(msg, "?")
	msg = reHex0x.ReplaceAllString(msg, "?")
	msg = reLongHx.ReplaceAllStringFunc(msg, func(s string) string {
		if strings.ContainsAny(s, "0123456789") {
			return "?"
		}
		return s
	})
	msg = reSQuote.ReplaceAllString(msg, "'?'")
	msg = reDQuote.ReplaceAllString(msg, `"?"`)
	msg = reNumber.ReplaceAllString(msg, "?")
	if msg == "" {
		return "error"
	}
	return truncUTF8(msg, maxGroupMessage)
}

// internStacks makes spans carrying the same stack text share one string,
// and drops stacks from stored spans once maxStacks distinct ones are held
// (error groups keep theirs). c.mu held.
func (c *Collector) internStacks(attrs map[string]string) {
	for _, k := range [...]string{"exception.stacktrace", "lt.stack"} {
		v, ok := attrs[k]
		if !ok {
			continue
		}
		if s, ok := c.stacks[v]; ok {
			attrs[k] = s
		} else if len(c.stacks) < maxStacks {
			c.stacks[v] = v
		} else {
			delete(attrs, k)
		}
	}
}

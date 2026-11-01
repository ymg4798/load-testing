// Command testserver is a target for trying lt and for checking that lt
// measures correctly. It can add delay, jitter, errors and stalls on demand.
//
//	GET  /fast                    "ok"
//	GET  /delay?ms=50             answer after a fixed delay
//	GET  /jitter?min=5&max=50     answer after a random delay
//	GET  /status?code=503         answer with a status code
//	GET  /flaky?rate=0.1          500 with the given probability
//	POST /login {"id","pw"}       {"token": "tok-<id>"}
//	GET  /items                   needs Authorization: Bearer tok-*
//	GET  /items/{id}              one item
//	POST /admin/stall?ms=1000     every request waits until the stall ends
//	GET  /work?ms=5               burn CPU for that long (makes server CPU visible)
//	GET  /metrics                 Prometheus metrics (CPU, heap, goroutines, requests)
//	GET  /order/{id}              3 fake SQL queries (one is 200ms ~5% of the time) + a call to its own /fast
//	GET  /report                  one slow fake SQL query (300–800ms)
//	GET  /lock?ms=400             chargeCard: waits for a mutex held ms by each caller in turn
//	                              (concurrent calls queue up; their active stacks show sync.(*Mutex).Lock)
//	GET  /panic?rate=0.3          panics with that probability (an index out of range in applyDiscount)
//
// Logs are JSON (log/slog) on stdout and, with -log-file, appended to that
// file too. With -apm they carry trace_id/span_id and are also sent to the
// collector as OTLP logs. /flaky failures and /order's slow query log an
// error with the request's trace.
//
// Requests carrying a W3C traceparent header get its trace ID back in X-Trace-Id.
//
//	GET  /ws/echo                 WebSocket echo
//	GET  /ws/chat                 WebSocket: {"type":"join"} → {"type":"joined"}; others echoed as {"type":"msg"}
//
// gRPC (default :50051): grpc.health.v1.Health/Check, with server reflection.
//
// With -apm URL (or LT_APM=URL) the server traces itself with lt's Go agent
// (service "testserver") and reports to an lt APM collector or any OTLP/HTTP
// endpoint: requests, the fake database's queries (see fakedb.go) and the
// outgoing call of /order, active stacks of requests running over 1s,
// and panics. /metrics is not traced.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ymg4798/lt/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var stallUntil atomic.Int64 // unix nanos

func stall() {
	if until := stallUntil.Load(); until > 0 {
		if d := time.Until(time.Unix(0, until)); d > 0 {
			time.Sleep(d)
		}
	}
}

func intParam(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func main() {
	httpAddr := flag.String("http", ":8080", "HTTP listen address")
	grpcAddr := flag.String("grpc", ":50051", "gRPC listen address (empty to disable)")
	apmURL := flag.String("apm", os.Getenv("LT_APM"), "report traces to this APM collector (lt --apm, or any OTLP/HTTP endpoint); env LT_APM")
	logFile := flag.String("log-file", "", "also append the JSON logs to this file")
	flag.Parse()

	var out io.Writer = os.Stdout
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		out = io.MultiWriter(os.Stdout, f)
	}
	// Also routes the log package (and so this file's log.Printf) through slog.
	slog.SetDefault(slog.New(agent.SlogHandler(slog.NewJSONHandler(out, nil))))

	if *apmURL != "" {
		a := agent.Start(agent.Config{Service: "testserver", Endpoint: *apmURL})
		log.Printf("APM: reporting to %s", *apmURL)
		go func() { // send the last spans on Ctrl-C
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			<-sig
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = a.Shutdown(ctx)
			cancel()
			os.Exit(0)
		}()
	}
	// The agent's wrappers pass straight through while no agent runs.
	db, err := agent.OpenDB("fakedb", "")
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{Transport: agent.Transport(nil), Timeout: 10 * time.Second}
	self := selfURL(*httpAddr)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", writeMetrics)
	mux.HandleFunc("GET /work", func(w http.ResponseWriter, r *http.Request) {
		stall()
		end := time.Now().Add(time.Duration(intParam(r, "ms", 5)) * time.Millisecond)
		x := 0
		for time.Now().Before(end) {
			for i := 0; i < 1000; i++ {
				x += i * i
			}
		}
		_, _ = fmt.Fprintf(w, "ok %d", x%7)
	})
	mux.HandleFunc("GET /fast", func(w http.ResponseWriter, r *http.Request) {
		stall()
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /delay", func(w http.ResponseWriter, r *http.Request) {
		stall()
		time.Sleep(time.Duration(intParam(r, "ms", 50)) * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /jitter", func(w http.ResponseWriter, r *http.Request) {
		stall()
		lo, hi := intParam(r, "min", 5), intParam(r, "max", 50)
		if hi < lo {
			hi = lo
		}
		time.Sleep(time.Duration(lo+rand.IntN(hi-lo+1)) * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		stall()
		w.WriteHeader(intParam(r, "code", 200))
	})
	mux.HandleFunc("GET /flaky", func(w http.ResponseWriter, r *http.Request) {
		stall()
		rate, _ := strconv.ParseFloat(r.URL.Query().Get("rate"), 64)
		if rand.Float64() < rate {
			slog.ErrorContext(r.Context(), "flaky failure", "path", r.URL.Path, "rate", rate)
			http.Error(w, "flaky", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		stall()
		var in struct {
			ID string `json:"id"`
			PW string `json:"pw"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"token": "tok-" + in.ID})
	})
	authed := func(r *http.Request) bool {
		return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-")
	}
	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		stall()
		if !authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no token"})
			return
		}
		items := make([]map[string]any, 10)
		for i := range items {
			items[i] = map[string]any{"id": i + 1, "name": fmt.Sprintf("item-%d", i+1)}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		stall()
		if !authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no token"})
			return
		}
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id < 1 || id > 10 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		time.Sleep(time.Duration(1+rand.IntN(5)) * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": fmt.Sprintf("item-%d", id), "price": id * 1000})
	})
	mux.HandleFunc("GET /order/{id}", func(w http.ResponseWriter, r *http.Request) {
		stall()
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id < 1 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		ctx := r.Context()
		var n int64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT /* %dms */ id, status FROM orders WHERE id = %d", 1+rand.IntN(3), id)).Scan(&n); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		itemsMs := 2 + rand.IntN(4)
		if rand.Float64() < 0.05 {
			itemsMs = 200
		}
		q := fmt.Sprintf("SELECT /* %dms */ sku, qty, price FROM order_items WHERE order_id = %d AND status IN ('open', 'paid')", itemsMs, id)
		if itemsMs >= 200 {
			slog.ErrorContext(ctx, "slow query", "sql", q, "ms", itemsMs, "order", id)
		}
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		items := 0
		for rows.Next() {
			items++
		}
		rows.Close()
		if _, err := db.ExecContext(ctx, fmt.Sprintf("UPDATE /* 1ms */ orders SET viewed_at = NOW() WHERE id = %d", id)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, self+"/fast", nil)
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		resp.Body.Close()
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "items": items, "stock": resp.StatusCode == http.StatusOK})
	})
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		stall()
		var n int64
		q := fmt.Sprintf("SELECT /* %dms */ region, SUM(amount) FROM sales WHERE year = 2026 GROUP BY region", 300+rand.IntN(501))
		if err := db.QueryRowContext(r.Context(), q).Scan(&n); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"regions": n})
	})
	mux.HandleFunc("GET /lock", func(w http.ResponseWriter, r *http.Request) {
		stall()
		hold := time.Duration(intParam(r, "ms", 400)) * time.Millisecond
		waited := chargeCard(r.Context(), hold)
		writeJSON(w, http.StatusOK, map[string]any{"charged": true, "waitedMs": waited.Milliseconds()})
	})
	mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) {
		stall()
		rate := 0.3
		if v, err := strconv.ParseFloat(r.URL.Query().Get("rate"), 64); err == nil {
			rate = v
		}
		defer func() { // log it with the trace, then let the agent record it
			if rec := recover(); rec != nil {
				slog.ErrorContext(r.Context(), "panic", "path", r.URL.Path, "error", fmt.Sprint(rec))
				panic(rec)
			}
		}()
		price := applyDiscount(1000, rand.Float64() < rate)
		writeJSON(w, http.StatusOK, map[string]any{"price": price})
	})
	mux.HandleFunc("POST /admin/stall", func(w http.ResponseWriter, r *http.Request) {
		d := time.Duration(intParam(r, "ms", 1000)) * time.Millisecond
		stallUntil.Store(time.Now().Add(d).UnixNano())
		fmt.Fprintf(w, "stalling for %s\n", d)
	})
	mux.HandleFunc("GET /ws/echo", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			t, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(t, msg); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("GET /ws/chat", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var in map[string]any
			_ = json.Unmarshal(msg, &in)
			out := map[string]any{"type": "msg", "echo": in}
			if in["type"] == "join" {
				out = map[string]any{"type": "joined", "room": in["room"]}
			}
			b, _ := json.Marshal(out)
			if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		}
	})

	if *grpcAddr != "" {
		lis, err := net.Listen("tcp", *grpcAddr)
		if err != nil {
			log.Fatal(err)
		}
		gs := grpc.NewServer()
		healthpb.RegisterHealthServer(gs, health.NewServer())
		reflection.Register(gs)
		go func() { log.Fatal(gs.Serve(lis)) }()
		log.Printf("gRPC on %s", *grpcAddr)
	}
	log.Printf("HTTP on %s", *httpAddr)
	srv := &http.Server{Addr: *httpAddr, Handler: count(traced(mux)), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// cardMu is the payment gateway's single connection: one charge at a time.
var cardMu sync.Mutex

// chargeCard holds cardMu for hold (a slow critical section), so
// concurrent callers wait in sync.(*Mutex).Lock here. It returns how long
// it waited for the lock.
func chargeCard(ctx context.Context, hold time.Duration) time.Duration {
	start := time.Now()
	cardMu.Lock()
	defer cardMu.Unlock()
	waited := time.Since(start)
	if waited > time.Second {
		slog.WarnContext(ctx, "card gateway busy", "waitedMs", waited.Milliseconds())
	}
	time.Sleep(hold)
	return waited
}

var discounts = []int{0, 5, 10}

// applyDiscount reads past the discount table when broken: a real
// runtime panic (index out of range) for the agent to record.
func applyDiscount(price int, broken bool) int {
	i := rand.IntN(len(discounts))
	if broken {
		i += len(discounts) + rand.IntN(5)
	}
	return price * (100 - discounts[i]) / 100
}

var (
	requests atomic.Int64
	inFlight atomic.Int64
)

// count tracks requests for /metrics and echoes the trace ID of a W3C
// traceparent header, so tracing can be checked end to end. It also stops
// handler panics here (500 if nothing was sent yet; the agent middleware
// inside has already recorded them and answered 500) instead of letting
// net/http print each one and drop the connection.
func count(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			requests.Add(1)
			inFlight.Add(1)
			defer inFlight.Add(-1)
		}
		if tp := r.Header.Get("traceparent"); len(tp) == 55 {
			w.Header().Set("X-Trace-Id", tp[3:35])
		}
		ww := &wroteWriter{ResponseWriter: w}
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			if !ww.wrote {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(ww, r)
	})
}

// wroteWriter remembers whether a response was started.
type wroteWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *wroteWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *wroteWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *wroteWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		w.wrote = true
		f.Flush()
	}
}

// Hijack is for the WebSocket endpoints.
func (w *wroteWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("testserver: cannot hijack %T", w.ResponseWriter)
	}
	w.wrote = true
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the connection.
func (w *wroteWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// traced runs requests through the APM agent, except /metrics scrapes.
func traced(mux *http.ServeMux) http.Handler {
	t := agent.Middleware(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			mux.ServeHTTP(w, r)
			return
		}
		t.ServeHTTP(w, r)
	})
}

// selfURL is how the server reaches itself (for /order's outgoing call).
func selfURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func writeMetrics(w http.ResponseWriter, _ *http.Request) {
	cpu := processCPU()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE process_cpu_seconds_total counter\nprocess_cpu_seconds_total %g\n", cpu)
	fmt.Fprintf(w, "# TYPE go_memstats_heap_inuse_bytes gauge\ngo_memstats_heap_inuse_bytes %d\n", m.HeapInuse)
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# TYPE testserver_requests_total counter\ntestserver_requests_total %d\n", requests.Load())
	fmt.Fprintf(w, "# TYPE testserver_in_flight gauge\ntestserver_in_flight %d\n", inFlight.Load())
}

package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// BenchmarkMiddleware is the agent's cost per request: no agent, an agent
// with active stacks off, the default (pprof label per request), and the
// default while a slow request is in flight the whole time, so the agent
// takes a goroutine profile every second.
func BenchmarkMiddleware(b *testing.B) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) }))
	defer sink.Close()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	run := func(b *testing.B, cfg *Config, slow bool) {
		if cfg != nil {
			cfg.Service, cfg.Endpoint, cfg.QueueSize = "bench", sink.URL, 1<<20
			a := Start(*cfg)
			defer func() { _ = a.Shutdown(context.Background()) }()
		}
		h := Middleware(ok)
		if slow {
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/slow", nil).WithContext(ctx))
			}()
			defer func() { cancel(); wg.Wait() }()
			time.Sleep(1200 * time.Millisecond) // over StackAfter: captures from now on
		}
		req := httptest.NewRequest("GET", "/items/7", nil)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
	b.Run("no-agent", func(b *testing.B) { run(b, nil, false) })
	b.Run("stacks-off", func(b *testing.B) { run(b, &Config{StackAfter: -1}, false) })
	b.Run("default", func(b *testing.B) { run(b, &Config{}, false) })
	b.Run("default+slow-tx", func(b *testing.B) { run(b, &Config{}, true) })
}

// BenchmarkCaptureStacks is one active-stack capture with 1000 other
// goroutines parked (taken at most once a second, only while a request
// runs over StackAfter).
func BenchmarkCaptureStacks(b *testing.B) {
	stop := make(chan struct{})
	for i := 0; i < 1000; i++ {
		go func() { <-stop }()
	}
	defer close(stop)
	var buf bytes.Buffer
	want := map[string]bool{"0123456789abcdef": true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		captureStacks(&buf, want)
	}
}

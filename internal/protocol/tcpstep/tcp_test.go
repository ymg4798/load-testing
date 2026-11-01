package tcpstep

import (
	"bufio"
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/protocol"
)

// lineServer answers each line "PING n" with "PONG n\r\n" and "QUIT" by closing.
func lineServer(t *testing.T) (string, *atomic.Int64) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var conns atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					l := strings.TrimSpace(sc.Text())
					switch {
					case l == "QUIT":
						return
					case l == "SLOW":
						time.Sleep(300 * time.Millisecond)
						c.Write([]byte("late\r\n"))
					case strings.HasPrefix(l, "PING"):
						// two replies in one write: the second must stay buffered
						c.Write([]byte("PONG" + strings.TrimPrefix(l, "PING") + "\r\nEXTRA\r\n"))
					default:
						c.Write([]byte("ERR " + l + "\r\n"))
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), &conns
}

func TestTCP(t *testing.T) {
	addr, conns := lineServer(t)
	env := &protocol.Env{Options: protocol.Options{Timeout: 2 * time.Second}}
	var results []protocol.Result
	vu := protocol.NewVU(4, "t", map[string]any{"addr": addr}, func(r protocol.Result) { results = append(results, r) })
	mk := func(m map[string]any) protocol.Step {
		t.Helper()
		m["addr"] = "{{ addr }}"
		st, err := compile(m, env)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	ctx := context.Background()
	ping := mk(map[string]any{"name": "ping", "send": `PING {{ vu }}\n`, "readUntil": `\r\n`, "expect": "PONG {{ vu }}", "keep": true,
		"extract": map[string]any{"n": `regex:PONG (\d+)`}})
	extra := mk(map[string]any{"name": "extra", "conn": "ping", "readUntil": `\r\n`, "expect": "/^EXTRA/", "keep": true})
	for range 3 {
		if err := ping.Run(ctx, vu); err != nil {
			t.Fatal(err)
		}
		if err := extra.Run(ctx, vu); err != nil {
			t.Fatal(err)
		}
	}
	if vu.Vars["n"] != "4" {
		t.Errorf("extract n = %v", vu.Vars["n"])
	}
	if conns.Load() != 1 {
		t.Errorf("keep: %d connections, want 1", conns.Load())
	}
	var connect, rt int
	for _, r := range results {
		switch r.Step {
		case "ping:connect":
			connect++
		case "ping":
			rt++
		}
	}
	if connect != 1 || rt != 3 {
		t.Errorf("connect=%d roundtrip=%d", connect, rt)
	}
	vu.Close()

	// No keep: one connection per run; expect without readUntil reads until match.
	vu2 := protocol.NewVU(9, "t", map[string]any{"addr": addr}, func(protocol.Result) {})
	once := mk(map[string]any{"send": "PING 1\n", "expect": "PONG 1"})
	for range 2 {
		if err := once.Run(ctx, vu2); err != nil {
			t.Fatal(err)
		}
	}
	if conns.Load() != 3 {
		t.Errorf("no keep: %d connections, want 3", conns.Load())
	}
	if once.(*step).name != "TCP {{ addr }}" {
		t.Errorf("name %q", once.(*step).name)
	}
	// Mismatch, EOF before the reply, timeout.
	wrong := mk(map[string]any{"send": "HELLO\n", "readUntil": "\r\n", "expect": "PONG"})
	if err := wrong.Run(ctx, vu2); protocol.ClassifyErr(err) != "check:expect" {
		t.Fatalf("want check:expect, got %v", err)
	}
	quit := mk(map[string]any{"send": "QUIT\n", "expect": "bye"})
	if err := quit.Run(ctx, vu2); protocol.ClassifyErr(err) != "check:expect" {
		t.Fatalf("want check:expect on EOF, got %v", err)
	}
	slow := mk(map[string]any{"send": "SLOW\n", "expect": "late", "timeout": "100ms"})
	if err := slow.Run(ctx, vu2); protocol.ClassifyErr(err) != "timeout" {
		t.Fatalf("want timeout, got %v", err)
	}
	fixed := mk(map[string]any{"send": "PING 22\n", "readBytes": 7, "expect": "PONG 22"})
	if err := fixed.Run(ctx, vu2); err != nil {
		t.Fatal(err)
	}
}

func TestTCPTLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS() // any TLS server: we send an HTTP request by hand
	defer srv.Close()
	env := &protocol.Env{Options: protocol.Options{Timeout: 2 * time.Second, Insecure: true}}
	st, err := compile(map[string]any{
		"addr": srv.Listener.Addr().String(), "tls": true,
		"send": `GET /x HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n`, "expect": "/^HTTP/1.1 404/",
	}, env)
	if err != nil {
		t.Fatal(err)
	}
	vu := protocol.NewVU(1, "t", nil, func(protocol.Result) {})
	if err := st.Run(context.Background(), vu); err != nil {
		t.Fatal(err)
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		`a\r\nb`: "a\r\nb", `\x41\t`: "A\t", `\\n`: `\n`, `plain`: "plain", `\q`: `\q`,
	} {
		got, err := unescape(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := unescape(`\x4`); err == nil {
		t.Error("want error")
	}
}

func TestCompileErrors(t *testing.T) {
	for _, raw := range []map[string]any{
		{"send": "x"},
		{"addr": "x:1"},
		{"addr": "x:1", "send": "x", "readUntil": "\n", "readBytes": 3},
		{"addr": "x:1", "send": "x", "expect": "/[/"},
		{"addr": "x:1", "send": "x", "extract": map[string]any{"a": "$.x"}},
	} {
		if _, err := compile(raw, &protocol.Env{}); err == nil {
			t.Errorf("%v: want error", raw)
		}
	}
}

package redisstep

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/protocol"
)

// fakeRedis speaks enough RESP for the tests: AUTH, SELECT, SET, GET, INCR,
// LRANGE (fixed), and errors for anything else.
type fakeRedis struct {
	ln    net.Listener
	mu    sync.Mutex
	data  map[string]string
	conns int
	auth  []string
}

func startFake(t *testing.T) *fakeRedis {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{ln: ln, data: map[string]string{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns++
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeRedis) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	for {
		v, err := readReply(rd)
		if err != nil {
			return
		}
		args := v.([]any)
		s := make([]string, len(args))
		for i, a := range args {
			s[i] = a.(string)
		}
		f.mu.Lock()
		var out string
		switch strings.ToUpper(s[0]) {
		case "AUTH":
			f.auth = append(f.auth, strings.Join(s[1:], ":"))
			out = "+OK\r\n"
		case "SELECT":
			out = "+OK\r\n"
		case "SET":
			f.data[s[1]] = s[2]
			out = "+OK\r\n"
		case "GET":
			if v, ok := f.data[s[1]]; ok {
				out = fmt.Sprintf("$%d\r\n%s\r\n", len(v), v)
			} else {
				out = "$-1\r\n"
			}
		case "INCR":
			n, _ := strconv.Atoi(f.data[s[1]])
			n++
			f.data[s[1]] = strconv.Itoa(n)
			out = fmt.Sprintf(":%d\r\n", n)
		case "LRANGE":
			out = "*2\r\n$1\r\na\r\n$1\r\nb\r\n"
		default:
			out = "-ERR unknown command '" + s[0] + "'\r\n"
		}
		f.mu.Unlock()
		if _, err := c.Write([]byte(out)); err != nil {
			return
		}
	}
}

func TestRedisStep(t *testing.T) {
	f := startFake(t)
	env := &protocol.Env{Options: protocol.Options{Timeout: 2 * time.Second}}
	var results []protocol.Result
	vu := protocol.NewVU(3, "t", map[string]any{"url": "redis://:secret@" + f.ln.Addr().String() + "/2"}, func(r protocol.Result) { results = append(results, r) })
	mk := func(m map[string]any) protocol.Step {
		t.Helper()
		if _, ok := m["addr"]; !ok {
			m["url"] = "{{ url }}"
		}
		st, err := compile(m, env)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	ctx := context.Background()
	set := mk(map[string]any{"command": []any{"SET", "k:{{ vu }}", "v{{ vu }}"}, "check": map[string]any{"reply": "OK"}})
	if err := set.Run(ctx, vu); err != nil {
		t.Fatal(err)
	}
	if set.(*step).name != "REDIS SET" {
		t.Errorf("name %q", set.(*step).name)
	}
	pipe := mk(map[string]any{
		"commands": []any{[]any{"INCR", "n"}, []any{"INCR", "n"}, []any{"GET", "k:{{ vu }}"}},
		"check":    map[string]any{"reply": "v{{ vu }}", "exists": true},
		"extract":  map[string]any{"v": "reply", "first": "replies.0"},
	})
	if err := pipe.Run(ctx, vu); err != nil {
		t.Fatal(err)
	}
	if vu.Vars["v"] != "v3" || vu.Vars["first"] != int64(1) {
		t.Fatalf("extract: %v", vu.Vars)
	}
	lr := mk(map[string]any{"command": []any{"LRANGE", "l", 0, -1}, "extract": map[string]any{"second": "reply.1"}})
	if err := lr.Run(ctx, vu); err != nil || vu.Vars["second"] != "b" {
		t.Fatalf("lrange: %v %v", err, vu.Vars["second"])
	}
	miss := mk(map[string]any{"command": []any{"GET", "nope"}, "check": map[string]any{"exists": true}})
	if err := miss.Run(ctx, vu); protocol.ClassifyErr(err) != "check:exists" {
		t.Fatalf("want check:exists, got %v", err)
	}
	bad := mk(map[string]any{"command": []any{"BOGUS"}})
	if err := bad.Run(ctx, vu); protocol.ClassifyErr(err) != "redis:ERR" {
		t.Fatalf("want redis:ERR, got %v", err)
	}
	vu.Close()
	f.mu.Lock()
	if f.conns != 1 || len(f.auth) != 1 || f.auth[0] != "secret" {
		t.Errorf("conns=%d auth=%v (want one connection reused, AUTH secret)", f.conns, f.auth)
	}
	f.mu.Unlock()
	connects := 0
	for _, r := range results {
		if r.Step == "REDIS SET:connect" {
			connects++
		}
	}
	if connects != 1 {
		t.Errorf("connect results: %d", connects)
	}

	// Connection refused is classified.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	vu2 := protocol.NewVU(1, "t", nil, func(protocol.Result) {})
	st := mk(map[string]any{"addr": addr, "command": []any{"PING"}})
	if err := st.Run(ctx, vu2); protocol.ClassifyErr(err) != "conn_refused" {
		t.Fatalf("want conn_refused, got %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	env := &protocol.Env{}
	for _, raw := range []map[string]any{
		{"command": []any{"PING"}},
		{"addr": "x:1"},
		{"addr": "x:1", "url": "redis://x", "command": []any{"PING"}},
		{"addr": "x:1", "command": []any{"GET", "a"}, "extract": map[string]any{"v": "$.x"}},
	} {
		if _, err := compile(raw, env); err == nil {
			t.Errorf("%v: want error", raw)
		}
	}
}

// Integration: LT_TEST_REDIS=redis://:lt@localhost:16379/0
func TestRealRedis(t *testing.T) {
	u := os.Getenv("LT_TEST_REDIS")
	if u == "" {
		t.Skip("LT_TEST_REDIS not set")
	}
	env := &protocol.Env{Options: protocol.Options{Timeout: 2 * time.Second}}
	vu := protocol.NewVU(1, "t", nil, func(protocol.Result) {})
	defer vu.Close()
	st, err := compile(map[string]any{"url": u, "commands": []any{
		[]any{"SET", "lt:test:{{ vu }}", "hello", "EX", 10},
		[]any{"GET", "lt:test:{{ vu }}"},
	}, "check": map[string]any{"reply": "hello"}, "extract": map[string]any{"ok": "replies.0"}}, env)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := st.Run(context.Background(), vu); err != nil {
			t.Fatal(err)
		}
	}
	if vu.Vars["ok"] != "OK" {
		t.Fatalf("got %v", vu.Vars["ok"])
	}
	wt, _ := compile(map[string]any{"url": u, "command": []any{"LPUSH", "lt:test:1", "x"}}, env)
	if err := wt.Run(context.Background(), vu); protocol.ClassifyErr(err) != "redis:WRONGTYPE" {
		t.Fatalf("want redis:WRONGTYPE, got %v", err)
	}
}

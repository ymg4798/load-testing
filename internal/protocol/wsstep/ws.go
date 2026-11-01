// Package wsstep is the "ws" step: open (or reuse) a WebSocket connection
// and run a list of actions on it.
//
//   - ws:
//     name: chat
//     url: "ws://localhost:8080/ws/chat"   # omit to reuse the connection named by conn
//     conn: chat                           # connection name, default: name
//     keep: true                           # keep it open across iterations
//     headers: { Authorization: "Bearer {{ token }}" }
//     actions:
//   - send: '{"type":"join","room":"{{ room }}"}'
//   - sendJson: { type: ping }
//   - expect: { name: joined, contains: joined, timeout: 2s }
//   - expect: { json: { "$.type": "msg" }, extract: { id: $.id } }
//   - sleep: 1s
//   - close: true
//
// Measurements: "<name>:connect" for the handshake and "<name>:<expect name>"
// for each expect, timed from the most recent send.
package wsstep

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"crypto/tls"

	"github.com/gorilla/websocket"

	"github.com/ymg4798/load-testing/internal/protocol"
	"github.com/ymg4798/load-testing/internal/tmpl"
	"github.com/ymg4798/load-testing/internal/yamlx"
)

func init() { protocol.Register("ws", compile) }

type spec struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Conn    string            `yaml:"conn"`
	Keep    bool              `yaml:"keep"`
	Headers map[string]string `yaml:"headers"`
	Timeout yamlx.Duration    `yaml:"timeout"`
	Actions []actionSpec      `yaml:"actions"`
}

type actionSpec struct {
	Send     *string         `yaml:"send"`
	SendJSON any             `yaml:"sendJson"`
	Expect   *expectSpec     `yaml:"expect"`
	Sleep    *yamlx.Duration `yaml:"sleep"`
	Close    bool            `yaml:"close"`
}

type expectSpec struct {
	Name       string            `yaml:"name"`
	Contains   string            `yaml:"contains"`
	JSON       map[string]any    `yaml:"json"`
	Timeout    yamlx.Duration    `yaml:"timeout"`
	Extract    map[string]string `yaml:"extract"`
	SkipOthers *bool             `yaml:"skipOthers"` // default true: ignore messages that do not match
}

type action struct {
	send     *tmpl.String
	sendJSON tmpl.Value
	expect   *expect
	sleep    time.Duration
	close    bool
}

type expect struct {
	name       string
	check      *protocol.Check
	extract    *protocol.Extract
	timeout    time.Duration
	skipOthers bool
}

type step struct {
	name    string
	conn    string
	keep    bool
	url     *tmpl.String
	headers map[string]*tmpl.String
	timeout time.Duration
	actions []action
	dialer  *websocket.Dialer
}

func compile(raw any, env *protocol.Env) (protocol.Step, error) {
	var sp spec
	if err := yamlx.Decode(raw, &sp); err != nil {
		return nil, fmt.Errorf("ws: %w", err)
	}
	s := &step{name: sp.Name, conn: sp.Conn, keep: sp.Keep, timeout: sp.Timeout.D()}
	if s.name == "" {
		s.name = "ws"
	}
	if s.conn == "" {
		s.conn = s.name
	}
	if s.timeout == 0 {
		s.timeout = env.Options.Timeout
	}
	var err error
	if sp.URL != "" {
		if s.url, err = tmpl.Compile(sp.URL); err != nil {
			return nil, err
		}
	}
	s.headers = map[string]*tmpl.String{}
	for k, v := range sp.Headers {
		if s.headers[k], err = tmpl.Compile(v); err != nil {
			return nil, err
		}
	}
	for i, a := range sp.Actions {
		var act action
		n := 0
		if a.Send != nil {
			n++
			if act.send, err = tmpl.Compile(*a.Send); err != nil {
				return nil, err
			}
		}
		if a.SendJSON != nil {
			n++
			if act.sendJSON, err = tmpl.CompileValue(a.SendJSON); err != nil {
				return nil, err
			}
		}
		if a.Expect != nil {
			n++
			e := a.Expect
			ex := &expect{name: e.Name, timeout: e.Timeout.D(), skipOthers: e.SkipOthers == nil || *e.SkipOthers}
			if ex.name == "" {
				ex.name = "recv" + strconv.Itoa(i+1)
			}
			if ex.timeout == 0 {
				ex.timeout = s.timeout
			}
			if ex.check, err = protocol.CompileCheck(&protocol.CheckSpec{BodyContains: e.Contains, JSON: e.JSON}); err != nil {
				return nil, err
			}
			if ex.extract, err = protocol.CompileExtract(e.Extract); err != nil {
				return nil, err
			}
			act.expect = ex
		}
		if a.Sleep != nil {
			n++
			act.sleep = a.Sleep.D()
		}
		if a.Close {
			n++
			act.close = true
		}
		if n != 1 {
			return nil, fmt.Errorf("ws %s: action %d needs exactly one of send, sendJson, expect, sleep, close", s.name, i+1)
		}
		s.actions = append(s.actions, act)
	}
	s.dialer = &websocket.Dialer{
		HandshakeTimeout: s.timeout,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: env.Options.Insecure}, //nolint:gosec // opt-in
		Proxy:            http.ProxyFromEnvironment,
	}
	return s, nil
}

type wsConn struct {
	c        *websocket.Conn
	lastSend time.Time
}

func (s *step) key() string { return "ws:" + s.conn }

func (s *step) Run(ctx context.Context, vu *protocol.VU) error {
	var conn *wsConn
	if s.url != nil {
		if old, ok := vu.GetState(s.key()); ok {
			old.(*wsConn).c.Close()
			vu.SetState(s.key(), nil)
		}
		c, err := s.dial(ctx, vu)
		if err != nil {
			return err
		}
		conn = c
		vu.SetState(s.key(), conn)
		cleanup := func() {
			if cur, ok := vu.GetState(s.key()); ok && cur == conn {
				conn.c.Close()
				vu.SetState(s.key(), nil)
			}
		}
		if s.keep {
			vu.AtClose(cleanup)
		} else {
			vu.AtIterationEnd(cleanup)
		}
	} else {
		v, ok := vu.GetState(s.key())
		if !ok {
			return protocol.Fail(s.name, "ws:no_conn", "no open connection named "+s.conn)
		}
		conn = v.(*wsConn)
	}

	for _, a := range s.actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case a.send != nil || a.sendJSON != nil:
			var msg []byte
			if a.send != nil {
				t, err := a.send.RenderString(vu)
				if err != nil {
					return protocol.Fail(s.name, "template", err.Error())
				}
				msg = []byte(t)
			} else {
				v, err := a.sendJSON.Render(vu)
				if err != nil {
					return protocol.Fail(s.name, "template", err.Error())
				}
				if msg, err = json.Marshal(v); err != nil {
					return protocol.Fail(s.name, "template", err.Error())
				}
			}
			_ = conn.c.SetWriteDeadline(time.Now().Add(s.timeout))
			conn.lastSend = time.Now()
			if err := conn.c.WriteMessage(websocket.TextMessage, msg); err != nil {
				kind := protocol.ClassifyErr(err)
				vu.Record(protocol.Result{Step: s.name + ":send", Err: kind})
				return protocol.Fail(s.name, kind, err.Error())
			}
		case a.expect != nil:
			if err := s.expect(ctx, vu, conn, a.expect); err != nil {
				return err
			}
		case a.sleep > 0:
			t := time.NewTimer(a.sleep)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
		case a.close:
			_ = conn.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			conn.c.Close()
			vu.SetState(s.key(), nil)
		}
	}
	return nil
}

func (s *step) dial(ctx context.Context, vu *protocol.VU) (*wsConn, error) {
	u, err := s.url.RenderString(vu)
	if err != nil {
		return nil, protocol.Fail(s.name, "template", err.Error())
	}
	h := http.Header{}
	for k, t := range s.headers {
		v, err := t.RenderString(vu)
		if err != nil {
			return nil, protocol.Fail(s.name, "template", err.Error())
		}
		h.Set(k, v)
	}
	dctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	start := time.Now()
	c, resp, err := s.dialer.DialContext(dctx, u, h)
	lat := time.Since(start)
	if err != nil {
		kind := protocol.ClassifyErr(err)
		if resp != nil {
			kind = "status:" + strconv.Itoa(resp.StatusCode)
		}
		vu.Record(protocol.Result{Step: s.name + ":connect", Latency: lat, Err: kind})
		return nil, protocol.Fail(s.name, kind, err.Error())
	}
	vu.Record(protocol.Result{Step: s.name + ":connect", Latency: lat})
	return &wsConn{c: c, lastSend: time.Now()}, nil
}

func (s *step) expect(ctx context.Context, vu *protocol.VU, conn *wsConn, e *expect) error {
	key := s.name + ":" + e.name
	deadline := time.Now().Add(e.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.c.SetReadDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.c.SetReadDeadline(time.Now()) })
	defer stop()
	for {
		_, msg, err := conn.c.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			kind := protocol.ClassifyErr(err)
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				kind = "ws:closed"
			}
			vu.Record(protocol.Result{Step: key, Latency: time.Since(conn.lastSend), Err: kind})
			// A failed read leaves the connection unusable.
			conn.c.Close()
			vu.SetState(s.key(), nil)
			return protocol.Fail(key, kind, err.Error())
		}
		lat := time.Since(conn.lastSend)
		r := &protocol.Response{Body: msg, Latency: lat}
		kind, m := e.check.Verify(r, vu, false)
		if kind != "" && e.skipOthers {
			continue
		}
		if kind == "" {
			kind, m = e.extract.Apply(r, vu.Vars)
		}
		vu.Last = r
		vu.Record(protocol.Result{Step: key, Latency: lat, Bytes: int64(len(msg)), Err: kind})
		if kind != "" {
			return protocol.Fail(key, kind, m)
		}
		return nil
	}
}

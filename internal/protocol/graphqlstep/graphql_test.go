package graphqlstep

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/protocol"
)

func server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "bad", 400)
			return
		}
		var req struct {
			Query         string         `json:"query"`
			Variables     map[string]any `json:"variables"`
			OperationName string         `json:"operationName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Seen-Traceparent", r.Header.Get("traceparent"))
		w.Header().Set("X-Seen-Auth", r.Header.Get("Authorization"))
		switch {
		case strings.Contains(req.Query, "boom"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []any{map[string]any{"message": "boom happened"}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"user": map[string]any{
				"id": req.Variables["id"], "op": req.OperationName,
			}}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGraphQL(t *testing.T) {
	srv := server(t)
	env := &protocol.Env{Options: protocol.Options{Timeout: 2 * time.Second, Tracing: protocol.Tracing{Enabled: true}}}
	defer env.Close()
	var results []protocol.Result
	vu := protocol.NewVU(5, "t", map[string]any{"base": srv.URL, "token": "abc"}, func(r protocol.Result) { results = append(results, r) })
	st, err := compile(map[string]any{
		"url":       "{{ base }}/graphql",
		"query":     "# get a user\nquery User($id: ID!) { user(id: $id) { id } }",
		"variables": map[string]any{"id": "u{{ vu }}"},
		"headers":   map[string]any{"Authorization": "Bearer {{ token }}"},
		"check":     map[string]any{"json": map[string]any{"$.data.user.id": "u{{ vu }}"}, "header": map[string]any{"X-Seen-Auth": "Bearer abc"}},
		"extract":   map[string]any{"uid": "$.data.user.id"},
	}, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.(*step).name; got != "GraphQL User" {
		t.Errorf("name %q", got)
	}
	if err := st.Run(context.Background(), vu); err != nil {
		t.Fatal(err)
	}
	if vu.Vars["uid"] != "u5" {
		t.Fatalf("uid = %v", vu.Vars["uid"])
	}
	if tp, _ := vu.Last.Header("X-Seen-Traceparent"); tp == "" || results[0].TraceID == "" || !strings.Contains(tp, results[0].TraceID) {
		t.Errorf("trace header %q, id %q", tp, results[0].TraceID)
	}

	bad := map[string]any{"url": srv.URL, "query": "mutation { boom }"}
	st2, err := compile(bad, env)
	if err != nil {
		t.Fatal(err)
	}
	if st2.(*step).name != "GraphQL mutation" {
		t.Errorf("name %q", st2.(*step).name)
	}
	err = st2.Run(context.Background(), vu)
	if protocol.ClassifyErr(err) != "graphql:error" || !strings.Contains(err.Error(), "boom happened") {
		t.Fatalf("want graphql:error, got %v", err)
	}
	if results[len(results)-1].Err != "graphql:error" {
		t.Errorf("recorded %q", results[len(results)-1].Err)
	}
	bad["check"] = map[string]any{"allowErrors": true}
	st3, _ := compile(bad, env)
	if err := st3.Run(context.Background(), vu); err != nil {
		t.Fatalf("allowErrors: %v", err)
	}
	// Shares the http transport with other steps.
	tr, _ := env.Shared("http.transport", func() (any, error) { return nil, nil })
	if tr.(*http.Transport) != st.(*step).tr {
		t.Error("transport not shared")
	}
	// HTTP errors are status errors.
	st4, _ := compile(map[string]any{"url": srv.URL, "query": "{ x }", "headers": map[string]any{"Content-Type": "text/plain"}}, env)
	if err := st4.Run(context.Background(), vu); protocol.ClassifyErr(err) != "status:400" {
		t.Fatalf("want status:400, got %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	if _, err := compile(map[string]any{"url": "http://x"}, &protocol.Env{}); err == nil {
		t.Error("want error without query")
	}
	if _, err := compile(map[string]any{"query": "{ a }"}, &protocol.Env{}); err == nil {
		t.Error("want error without url")
	}
}

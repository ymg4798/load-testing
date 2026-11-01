package convert

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommands(t *testing.T) {
	cases := []struct {
		in   string
		want [][]string
	}{
		{`curl http://a/x`, [][]string{{"curl", "http://a/x"}}},
		{"curl 'http://a/x?a=1&b=2' \\\n  -H 'Accept: json'", [][]string{{"curl", "http://a/x?a=1&b=2", "-H", "Accept: json"}}},
		{`curl -d "say \"hi\" \$x" a`, [][]string{{"curl", "-d", `say "hi" $x`, "a"}}},
		{`curl --data-raw $'{"a":"it\'s\\n\u00e9"}' a`, [][]string{{"curl", "--data-raw", "{\"a\":\"it's\\n\u00e9\"}", "a"}}},
		{`curl $'line\nnext\x41' a`, [][]string{{"curl", "line\nnextA", "a"}}},
		{"curl a\ncurl b\n\n# comment\necho no\ncurl c | jq .", [][]string{{"curl", "a"}, {"curl", "b"}, {"curl", "c"}}},
		{`curl a && curl b; curl c`, [][]string{{"curl", "a"}, {"curl", "b"}, {"curl", "c"}}},
		{`curl 'a'"b"c`, [][]string{{"curl", "abc"}}},
	}
	for _, c := range cases {
		got, err := splitCommands(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{`curl 'a`, `curl "a`, `curl $'a`} {
		if _, err := splitCommands(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestCurl(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, h map[string]any, y string)
	}{
		{"get", `curl https://api.test/v1/users?page=2`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["url"], "{{ baseUrl }}/v1/users")
			eq(t, h["method"], nil)
			eq(t, h["query"].(map[string]any)["page"], "2")
		}},
		{"json post", `curl https://api.test/login -H 'Content-Type: application/json' --data-raw '{"id":"a","n":1}'`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["method"], "POST")
			j := h["json"].(map[string]any)
			eq(t, j["id"], "a")
			eq(t, j["n"], 1)
			eq(t, h["headers"], nil) // default content type is dropped
		}},
		{"--json", `curl --json '{"a":true}' https://api.test/x`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["json"].(map[string]any)["a"], true)
			eq(t, h["headers"].(map[string]any)["Accept"], "application/json")
		}},
		{"form default", `curl -d a=1 -d b=x%20y https://api.test/f`, func(t *testing.T, h map[string]any, y string) {
			f := h["form"].(map[string]any)
			eq(t, f["a"], "1")
			eq(t, f["b"], "x y")
		}},
		{"raw body", `curl -X PUT -H 'Content-Type: text/plain' -d 'hello world' https://api.test/t`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["method"], "PUT")
			eq(t, h["body"], "hello world")
			eq(t, h["headers"].(map[string]any)["Content-Type"], "text/plain")
		}},
		{"-XPOST no data", `curl -XPOST https://api.test/p`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["method"], "POST")
		}},
		{"basic auth and cookie", `curl -u bob:pw -b 'sid=1; x=2' https://api.test/me`, func(t *testing.T, h map[string]any, y string) {
			hd := h["headers"].(map[string]any)
			eq(t, hd["Authorization"], "Basic Ym9iOnB3")
			eq(t, hd["Cookie"], "sid=1; x=2")
		}},
		{"bundled flags", `curl -sSLkH 'X-Api-Key: k' https://api.test/b --compressed -o out.txt -m 5`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["headers"].(map[string]any)["X-Api-Key"], "k")
			if !strings.Contains(y, "insecure: true") {
				t.Error("expected insecure option")
			}
		}},
		{"-G", `curl -G https://api.test/s -d q=go --data-urlencode 'lang=a b'`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["method"], nil)
			q := h["query"].(map[string]any)
			eq(t, q["q"], "go")
			eq(t, q["lang"], "a b")
		}},
		{"devtools noise", `curl 'https://api.test/n' -H 'accept: */*' -H 'user-agent: Mozilla' -H 'sec-ch-ua: x' -H 'x-csrf: t' -H 'referer: https://api.test/'`, func(t *testing.T, h map[string]any, y string) {
			hd := h["headers"].(map[string]any)
			if len(hd) != 1 || hd["X-Csrf"] != "t" {
				t.Errorf("headers = %v", hd)
			}
		}},
		{"head", `curl -I --url https://api.test/h`, func(t *testing.T, h map[string]any, y string) {
			eq(t, h["method"], "HEAD")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y, f := convertOK(t, FormatCurl, c.in, Options{})
			hs := flowHTTP(t, f)
			if len(hs) != 1 {
				t.Fatalf("want 1 step:\n%s", y)
			}
			c.check(t, hs[0], y)
			if t.Failed() {
				t.Log(y)
			}
		})
	}
}

func TestCurlMultiOrigins(t *testing.T) {
	in := "curl https://a.test/1\ncurl https://b.test/2\ncurl https://a.test/3\ncurl https://a.test/1"
	y, f := convertOK(t, FormatCurl, in, Options{})
	eq(t, f.Vars["baseUrl"], "https://a.test")
	hs := flowHTTP(t, f)
	eq(t, hs[1]["url"], "https://b.test/2")
	eq(t, hs[3]["name"], "GET /1 #2")
	_ = y

	_, f = convertOK(t, FormatCurl, in, Options{Hosts: []string{"b.test"}, BaseURL: "http://staging:9000/"})
	eq(t, len(flowHTTP(t, f)), 1)
	eq(t, f.Vars["baseUrl"], "http://staging:9000")
}

func TestCurlErrors(t *testing.T) {
	for _, in := range []string{"echo hi", "curl -H", "curl -s"} {
		if _, err := Curl([]byte(in), Options{}); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

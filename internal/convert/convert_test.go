package convert

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ymg4798/lt/internal/engine"
	"github.com/ymg4798/lt/internal/scenario"
)

// load checks that generated YAML loads and compiles like a hand-written file.
func load(t *testing.T, y []byte) *scenario.File {
	t.Helper()
	f, err := scenario.Parse(y, t.TempDir(), "out.yaml", nil)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, y)
	}
	if err := engine.Validate(f); err != nil {
		t.Fatalf("validate: %v\n%s", err, y)
	}
	return f
}

func convertOK(t *testing.T, format, input string, opt Options) (string, *scenario.File) {
	t.Helper()
	y, err := Convert(format, "in", []byte(input), opt)
	if err != nil {
		t.Fatal(err)
	}
	return string(y), load(t, y)
}

func flowHTTP(t *testing.T, f *scenario.File) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, st := range f.Scenarios["main"].Flow {
		if h, ok := st["http"].(map[string]any); ok {
			out = append(out, h)
		}
	}
	return out
}

func TestExamples(t *testing.T) {
	files, _ := filepath.Glob("../../examples/convert/*")
	if len(files) < 3 {
		t.Fatalf("expected example inputs, got %v", files)
	}
	for _, p := range files {
		t.Run(filepath.Base(p), func(t *testing.T) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			y, err := Convert("", p, b, Options{})
			if err != nil {
				t.Fatal(err)
			}
			f := load(t, y)
			if len(f.Scenarios["main"].Flow) < 5 {
				t.Errorf("too few steps:\n%s", y)
			}
			if f.Vars["baseUrl"] != "http://localhost:8080" {
				t.Errorf("baseUrl = %v", f.Vars["baseUrl"])
			}
		})
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name, data, want string
	}{
		{"x.har", "{}", FormatHAR},
		{"x.json", `{"log":{"entries":[]}}`, FormatHAR},
		{"x.json", `{"openapi":"3.0.0","paths":{}}`, FormatOpenAPI},
		{"x.yaml", "swagger: '2.0'\npaths: {}\n", FormatOpenAPI},
		{"x.txt", "curl http://a/", FormatCurl},
		{"-", "# copied\ncurl 'http://a/'", FormatCurl},
	}
	for _, c := range cases {
		got, err := Detect(c.name, []byte(c.data))
		if err != nil || got != c.want {
			t.Errorf("Detect(%s, %q) = %q, %v; want %q", c.name, c.data, got, err, c.want)
		}
	}
	if _, err := Detect("x.txt", []byte("hello")); err == nil {
		t.Error("expected an error for unknown input")
	}
}

func TestOptionsInOutput(t *testing.T) {
	y, f := convertOK(t, FormatCurl, "curl -k https://a.test/x", Options{Name: "n", Rate: 2.5, Duration: "1m"})
	if f.Name != "n" || !f.Options.Insecure {
		t.Errorf("name/insecure wrong:\n%s", y)
	}
	sc := f.Scenarios["main"]
	if sc.Rate != 2.5 || sc.Duration.D().String() != "1m0s" {
		t.Errorf("rate/duration wrong:\n%s", y)
	}
}

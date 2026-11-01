package tmpl

import "testing"

func TestRender(t *testing.T) {
	s := MapScope{"user": map[string]any{"id": "kim", "n": 3}, "list": []any{"a", "b"}}
	cases := map[string]any{
		"plain":                 "plain",
		"hi {{ user.id }}!":     "hi kim!",
		"{{ user.n }}":          3, // a lone placeholder keeps its type
		"{{user.n}}-{{list.1}}": "3-b",
	}
	for src, want := range cases {
		c, err := Compile(src)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Render(s)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%q → %v (%T), want %v", src, got, got, want)
		}
	}
}

func TestErrors(t *testing.T) {
	for _, src := range []string{"{{ x", "{{ }}", "{{ nope() }}"} {
		if _, err := Compile(src); err == nil {
			t.Errorf("%q: expected error", src)
		}
	}
	c := MustCompile("{{ missing }}")
	if _, err := c.Render(MapScope{}); err == nil {
		t.Error("unknown variable should fail")
	}
}

func TestFunctions(t *testing.T) {
	c := MustCompile("{{ randInt(5, 5) }}")
	v, _ := c.Render(MapScope{})
	if v != int64(5) {
		t.Fatalf("randInt(5,5) = %v", v)
	}
	u, _ := MustCompile("{{ uuid() }}").RenderString(MapScope{})
	if len(u) != 36 {
		t.Fatalf("uuid %q", u)
	}
}

func TestCompileValue(t *testing.T) {
	v, err := CompileValue(map[string]any{"a": "{{ x }}", "b": []any{"{{ x }}-y", 1}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Render(MapScope{"x": 7})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["a"] != 7 || m["b"].([]any)[0] != "7-y" || m["b"].([]any)[1] != 1 {
		t.Fatalf("got %#v", got)
	}
}

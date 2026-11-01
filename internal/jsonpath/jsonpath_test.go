package jsonpath

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestGet(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"a":{"b":[{"id":1},{"id":2}]},"odd key":true}`), &doc)
	cases := map[string]any{
		"$.a.b[0].id":  1.0,
		"$.a.b[-1].id": 2.0,
		"a.b[1].id":    2.0,
		"$['odd key']": true,
		"$.a.b.length": 2.0,
		"$.a.b[*].id":  []any{1.0, 2.0},
	}
	for p, want := range cases {
		c, err := Compile(p)
		if err != nil {
			t.Fatal(p, err)
		}
		got, ok := c.Get(doc)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v (%v), want %v", p, got, ok, want)
		}
	}
	c, _ := Compile("$.a.missing")
	if _, ok := c.Get(doc); ok {
		t.Error("missing key should not match")
	}
}

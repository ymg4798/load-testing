package yamlx

import "testing"

func TestDecodeKeepsWhitespaceStrings(t *testing.T) {
	for _, in := range []string{"\n", "\r\n", " ", "a\n", "\n\n", "\t"} {
		var out struct {
			S string `yaml:"s"`
		}
		if err := Decode(map[string]any{"s": in}, &out); err != nil {
			t.Fatal(err)
		}
		if out.S != in {
			t.Errorf("%q came back as %q", in, out.S)
		}
	}
}

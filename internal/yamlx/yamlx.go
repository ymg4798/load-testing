// Package yamlx holds YAML helpers shared by the scenario loader and the
// protocol plugins.
package yamlx

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration accepts "300ms", "2m", "1h30m" or a bare number of milliseconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// ParseDuration parses a duration; bare numbers are milliseconds.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if v, err := time.ParseDuration(s); err == nil {
		return v, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Millisecond)), nil
	}
	return 0, fmt.Errorf("bad duration %q (use 300ms, 2s, 1m)", s)
}

// Decode converts a generic YAML value (as decoded into any) into a struct,
// rejecting unknown keys so typos surface as errors.
func Decode(raw any, out any) error {
	n, err := toNode(raw)
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return cleanErr(err)
	}
	return nil
}

// cleanErr drops yaml.v3's line numbers, which point into the re-encoded
// fragment rather than the user's file.
func cleanErr(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "yaml: unmarshal errors:\n")
	var lines []string
	for _, l := range strings.Split(msg, "\n") {
		l = strings.TrimSpace(l)
		if i := strings.Index(l, ": "); strings.HasPrefix(l, "line ") && i > 0 {
			l = l[i+2:]
		}
		if l != "" {
			lines = append(lines, l)
		}
	}
	return fmt.Errorf("%s", strings.Join(lines, "; "))
}

// toNode builds a YAML node from decoded YAML values. Strings are always
// double-quoted: yaml.v3 would write "\n" as a block scalar that reads
// back as "".
func toNode(v any) (*yaml.Node, error) {
	switch x := v.(type) {
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x, Style: yaml.DoubleQuotedStyle}, nil
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c, err := toNode(x[k])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, c)
		}
		return n, nil
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = val
		}
		return toNode(m)
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range x {
			c, err := toNode(e)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, c)
		}
		return n, nil
	}
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

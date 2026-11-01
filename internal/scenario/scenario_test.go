package scenario

import (
	"strings"
	"testing"
)

const sample = `
name: t
scenarios:
  a:
    rate: 100
    duration: 10s
    flow:
      - http: http://x/
  b:
    executor: ramping-users
    stages:
      - { duration: 5s, target: 7 }
    flow:
      - sleep: 1s
`

func TestParseAndDefaults(t *testing.T) {
	f, err := Parse([]byte(sample), ".", "t.yaml", map[string]string{"host": "h"})
	if err != nil {
		t.Fatal(err)
	}
	a := f.Scenarios["a"]
	if a.Executor != ArrivalRate || a.StartRate != 100 || len(a.Stages) != 1 || a.MaxInFlight != 1100 {
		t.Fatalf("defaults not applied: %+v", a)
	}
	if f.Vars["host"] != "h" {
		t.Fatal("override not applied")
	}
	if f.Options.Timeout == 0 || f.Options.HTTP2 == nil {
		t.Fatal("options defaults missing")
	}
}

func TestPartition(t *testing.T) {
	f, _ := Parse([]byte(sample), ".", "t.yaml", nil)
	f.Partition(0, 3)
	a, b := f.Scenarios["a"], f.Scenarios["b"]
	if a.Stages[0].Target < 33.3 || a.Stages[0].Target > 33.4 {
		t.Fatalf("rate share %v", a.Stages[0].Target)
	}
	if b.Stages[0].Target != 3 { // 7 users over 3 agents: 3, 2, 2
		t.Fatalf("users share %v", b.Stages[0].Target)
	}
	g, _ := Parse([]byte(sample), ".", "t.yaml", nil)
	g.Partition(2, 3)
	if g.Scenarios["b"].Stages[0].Target != 2 {
		t.Fatalf("last agent share %v", g.Scenarios["b"].Stages[0].Target)
	}
}

func TestErrors(t *testing.T) {
	bad := map[string]string{
		"unknown key":   "scenarios: {a: {rate: 1, duration: 1s, flow: [{http: x}], bogus: 1}}",
		"no flow":       "scenarios: {a: {rate: 1, duration: 1s}}",
		"no scenarios":  "name: x",
		"two kinds":     "scenarios: {a: {rate: 1, duration: 1s, flow: [{http: x, sleep: 1s}]}}",
		"bad executor":  "scenarios: {a: {executor: nope, flow: [{sleep: 1s}]}}",
		"rate no dur":   "scenarios: {a: {rate: 5, flow: [{sleep: 1s}]}}",
		"bad data mode": "data: {u: {rows: [{a: 1}], mode: x}}\nscenarios: {a: {rate: 1, duration: 1s, flow: [{sleep: 1s}]}}",
	}
	for name, src := range bad {
		if _, err := Parse([]byte(src), ".", "t.yaml", nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDataModes(t *testing.T) {
	src := "data: {u: {rows: [{id: 1}, {id: 2}, {id: 3}, {id: 4}], mode: unique}}\n" +
		"scenarios: {a: {rate: 1, duration: 1s, flow: [{sleep: 1s}]}}"
	f, err := Parse([]byte(src), ".", "t.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Partition(1, 2)
	d := f.Data["u"]
	if err := d.Open("."); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for {
		row, ok := d.Next(nil)
		if !ok {
			break
		}
		ids = append(ids, strings.TrimSpace(strings.Repeat(" ", 0)+toS(row["id"])))
	}
	if strings.Join(ids, ",") != "2,4" {
		t.Fatalf("agent 2 of 2 got rows %v, want 2,4", ids)
	}
}

func toS(v any) string {
	switch x := v.(type) {
	case int:
		return string(rune('0' + x))
	}
	return ""
}

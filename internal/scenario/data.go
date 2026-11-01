package scenario

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
)

// DataSpec feeds rows from a file into each iteration as a variable.
//
//	data:
//	  users: { csv: users.csv, mode: sequential }   # sequential | random | unique
//
// In a flow, {{ users.id }} reads the "id" column of the row for this
// iteration. "unique" hands out every row at most once and fails the
// iteration once rows run out. With several agents, sequential and unique
// rows are split between agents so no two agents share a row.
type DataSpec struct {
	CSV  string           `yaml:"csv"`
	JSON string           `yaml:"json"` // a JSON array of objects
	Rows []map[string]any `yaml:"rows"` // rows written inline
	Mode string           `yaml:"mode"`

	partIndex, partCount int
	rows                 []map[string]any
	next                 atomic.Int64
}

func (d *DataSpec) validate(name string) error {
	n := 0
	if d.CSV != "" {
		n++
	}
	if d.JSON != "" {
		n++
	}
	if d.Rows != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("data %s: give exactly one of csv, json, rows", name)
	}
	if d.Mode == "" {
		d.Mode = "sequential"
	}
	switch d.Mode {
	case "sequential", "random", "unique":
	default:
		return fmt.Errorf("data %s: mode must be sequential, random or unique", name)
	}
	return nil
}

// Open reads the rows. Call once per run before Next.
func (d *DataSpec) Open(dir string) error {
	var rows []map[string]any
	switch {
	case d.Rows != nil:
		rows = d.Rows
	case d.CSV != "":
		f, err := os.Open(resolve(dir, d.CSV))
		if err != nil {
			return err
		}
		defer f.Close()
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		recs, err := r.ReadAll()
		if err != nil {
			return fmt.Errorf("%s: %w", d.CSV, err)
		}
		if len(recs) < 2 {
			return fmt.Errorf("%s: needs a header row and at least one data row", d.CSV)
		}
		head := recs[0]
		for _, rec := range recs[1:] {
			row := make(map[string]any, len(head))
			for i, h := range head {
				if i < len(rec) {
					row[h] = rec[i]
				} else {
					row[h] = ""
				}
			}
			rows = append(rows, row)
		}
	case d.JSON != "":
		b, err := os.ReadFile(resolve(dir, d.JSON))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &rows); err != nil {
			return fmt.Errorf("%s: expected a JSON array of objects: %w", d.JSON, err)
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("data has no rows")
	}
	if d.partCount > 1 && d.Mode != "random" {
		var mine []map[string]any
		for i, r := range rows {
			if i%d.partCount == d.partIndex {
				mine = append(mine, r)
			}
		}
		if len(mine) == 0 {
			return fmt.Errorf("data: %d rows is fewer than %d agents", len(rows), d.partCount)
		}
		rows = mine
	}
	d.rows = rows
	d.next.Store(0)
	return nil
}

// Next returns the row for the next iteration. false means unique rows ran out.
func (d *DataSpec) Next(r *rand.Rand) (map[string]any, bool) {
	switch d.Mode {
	case "random":
		return d.rows[r.IntN(len(d.rows))], true
	case "unique":
		k := d.next.Add(1) - 1
		if k >= int64(len(d.rows)) {
			return nil, false
		}
		return d.rows[k], true
	default:
		k := d.next.Add(1) - 1
		return d.rows[k%int64(len(d.rows))], true
	}
}

// Len is the number of rows this run can use.
func (d *DataSpec) Len() int { return len(d.rows) }

func resolve(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

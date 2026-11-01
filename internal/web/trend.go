package web

import (
	_ "embed"
	"html/template"
	"io"

	"github.com/ymg4798/lt/internal/history"
)

var (
	//go:embed assets/trend.html
	trendHTML string
	//go:embed assets/trend.js
	trendJS string
)

var trendPage = template.Must(template.New("trend").Parse(trendHTML))

type trendRun struct {
	history.Entry
	Pinned bool `json:"pinned"`
}

// WriteTrend writes a self-contained page charting a test's stored runs.
func WriteTrend(w io.Writer, test string, runs []history.Entry) error {
	list := make([]trendRun, len(runs))
	for i, e := range runs {
		list[i] = trendRun{Entry: e, Pinned: e.Pinned}
	}
	data, err := marshal(map[string]any{"test": test, "runs": list})
	if err != nil {
		return err
	}
	return trendPage.Execute(w, pageData{
		Title:    test + " · lt trend",
		UplotCSS: template.CSS(uplotCSS),
		AppCSS:   template.CSS(appCSS),
		UplotJS:  template.JS(uplotJS),
		AppJS:    template.JS(trendJS),
		Data:     template.JS(data),
	})
}

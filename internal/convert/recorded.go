package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Recorded is one request/response pair captured live (lt record). It
// carries what the HAR converter reads, so recordings go through the same
// filtering (static assets, hosts), header cleanup, body handling and
// pause detection as HAR files.
type Recorded struct {
	Started         time.Time
	Duration        time.Duration // until the response body was read
	Method          string
	URL             string // absolute
	Headers         []KV   // request headers, string values, in order
	Body            string
	ContentType     string // request body type
	Status          int    // 0 when the request got no response
	ResponseHeaders []KV
	ResponseType    string // response Content-Type
}

// FromRecording converts captured traffic into a scenario.
func FromRecording(entries []Recorded, opt Options) (*Scenario, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("record: no requests were recorded")
	}
	type pair struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	pairs := func(kv []KV) []pair {
		out := make([]pair, 0, len(kv))
		for _, h := range kv {
			out = append(out, pair{h.Key, fmt.Sprint(h.Value)})
		}
		return out
	}
	type entry struct {
		StartedDateTime string  `json:"startedDateTime"`
		Time            float64 `json:"time"`
		Request         struct {
			Method   string `json:"method"`
			URL      string `json:"url"`
			Headers  []pair `json:"headers"`
			PostData *struct {
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"postData,omitempty"`
		} `json:"request"`
		Response struct {
			Status  int    `json:"status"`
			Headers []pair `json:"headers"`
			Content struct {
				MimeType string `json:"mimeType"`
			} `json:"content"`
		} `json:"response"`
	}
	var har struct {
		Log struct {
			Entries []entry `json:"entries"`
		} `json:"log"`
	}
	for _, r := range entries {
		var e entry
		e.StartedDateTime = r.Started.UTC().Format(time.RFC3339Nano)
		e.Time = float64(r.Duration) / float64(time.Millisecond)
		e.Request.Method, e.Request.URL, e.Request.Headers = r.Method, r.URL, pairs(r.Headers)
		if r.Body != "" {
			e.Request.PostData = &struct {
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
			}{r.ContentType, r.Body}
		}
		e.Response.Status, e.Response.Headers = r.Status, pairs(r.ResponseHeaders)
		e.Response.Content.MimeType = r.ResponseType
		har.Log.Entries = append(har.Log.Entries, e)
	}
	data, err := json.Marshal(har)
	if err != nil {
		return nil, err
	}
	s, err := HAR(data, opt)
	if err != nil {
		return nil, fmt.Errorf("record: %s", strings.TrimPrefix(err.Error(), "har: "))
	}
	for i, c := range s.Comments {
		if strings.HasPrefix(c, "Converted from HAR by lt convert") {
			s.Comments[i] = strings.Replace(c, "Converted from HAR by lt convert", "Recorded by lt record", 1)
		}
	}
	return s, nil
}

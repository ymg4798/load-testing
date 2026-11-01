package convert

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path"
	"strings"
	"time"
)

type harFile struct {
	Log struct {
		Entries []harEntry `json:"entries"`
	} `json:"log"`
}

type harEntry struct {
	StartedDateTime string  `json:"startedDateTime"`
	Time            float64 `json:"time"` // ms
	ResourceType    string  `json:"_resourceType"`
	Request         struct {
		Method   string    `json:"method"`
		URL      string    `json:"url"`
		Headers  []harPair `json:"headers"`
		Cookies  []harPair `json:"cookies"`
		PostData *struct {
			MimeType string    `json:"mimeType"`
			Text     string    `json:"text"`
			Params   []harPair `json:"params"`
		} `json:"postData"`
	} `json:"request"`
	Response struct {
		Status  int       `json:"status"`
		Headers []harPair `json:"headers"`
		Content struct {
			MimeType string `json:"mimeType"`
		} `json:"content"`
	} `json:"response"`
}

type harPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

var staticTypes = map[string]bool{
	"image": true, "stylesheet": true, "script": true, "font": true, "media": true,
	"manifest": true, "texttrack": true,
}

var staticExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".ico": true,
	".webp": true, ".avif": true, ".bmp": true, ".css": true, ".js": true, ".mjs": true,
	".map": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp4": true, ".webm": true, ".mp3": true, ".wav": true, ".ogg": true, ".m4a": true,
	".wasm": true,
}

func isStatic(e *harEntry, u *url.URL) bool {
	if staticTypes[strings.ToLower(e.ResourceType)] {
		return true
	}
	if staticExts[strings.ToLower(path.Ext(u.Path))] {
		return true
	}
	mt := strings.ToLower(e.Response.Content.MimeType)
	mt, _, _ = strings.Cut(mt, ";")
	for _, p := range []string{"image/", "font/", "audio/", "video/", "application/font"} {
		if strings.HasPrefix(mt, p) {
			return true
		}
	}
	switch strings.TrimSpace(mt) {
	case "text/css", "text/javascript", "application/javascript", "application/x-javascript",
		"application/wasm", "application/vnd.ms-fontobject":
		return true
	}
	return false
}

// HAR converts a browser HAR recording.
func HAR(data []byte, opt Options) (*Scenario, error) {
	var h harFile
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("har: %w", err)
	}
	if len(h.Log.Entries) == 0 {
		return nil, fmt.Errorf("har: no entries")
	}
	s := newScenario(opt)
	var (
		reqs                            []request
		starts                          []time.Time
		ends                            []time.Time
		nStatic, nHost, nOther, nFailed int
		cookies                         bool
	)
	for i := range h.Log.Entries {
		e := &h.Log.Entries[i]
		u, err := url.Parse(e.Request.URL)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || e.ResourceType == "websocket" || e.Response.Status == 101 {
			nOther++
			continue
		}
		if !hostAllowed(u, opt.Hosts) {
			nHost++
			continue
		}
		if !opt.All && isStatic(e, u) {
			nStatic++
			continue
		}
		if e.Response.Status == 0 {
			nFailed++
			continue
		}
		r := request{Method: strings.ToUpper(e.Request.Method), URL: u, Status: e.Response.Status}
		if len(e.Request.Cookies) > 0 {
			cookies = true
		}
		for _, hd := range e.Request.Headers {
			if strings.EqualFold(hd.Name, "cookie") {
				cookies = true
			}
			r.Headers = append(r.Headers, KV{hd.Name, hd.Value})
		}
		for _, hd := range e.Response.Headers {
			if strings.EqualFold(hd.Name, "set-cookie") {
				cookies = true
			}
		}
		if pd := e.Request.PostData; pd != nil {
			r.Body = pd.Text
			r.contentType = pd.MimeType
			if pd.Text == "" {
				for _, p := range pd.Params {
					r.FormParams = append(r.FormParams, KV{p.Name, p.Value})
				}
			}
		}
		t, _ := time.Parse(time.RFC3339Nano, e.StartedDateTime)
		reqs = append(reqs, r)
		starts = append(starts, t)
		ends = append(ends, t.Add(time.Duration(e.Time*float64(time.Millisecond))))
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("har: no requests left after filtering (%d static, %d other hosts, %d non-HTTP, %d failed); try --all or --host", nStatic, nHost, nOther, nFailed)
	}

	s.Comments = append(s.Comments, fmt.Sprintf("Converted from HAR by lt convert: %d request(s).", len(reqs)))
	if nStatic > 0 {
		s.Comments = append(s.Comments, fmt.Sprintf("skipped %d static asset request(s) (use --all to keep)", nStatic))
	}
	if nHost > 0 {
		s.Comments = append(s.Comments, fmt.Sprintf("skipped %d request(s) to other hosts (--host)", nHost))
	}
	if nOther > 0 {
		s.Comments = append(s.Comments, fmt.Sprintf("skipped %d non-HTTP request(s) (websocket, data:, extensions)", nOther))
	}
	if nFailed > 0 {
		s.Comments = append(s.Comments, fmt.Sprintf("skipped %d request(s) that got no response", nFailed))
	}
	if cookies {
		s.Cookies = true
		s.Comments = append(s.Comments, "cookies were recorded: options.cookies keeps a per-VU cookie jar instead of replaying them")
	}
	s.Comments = append(s.Comments, "Review hard-coded tokens and ids; replace them with vars, data or extract.")

	buildSteps(s, reqs, false, opt.BaseURL)
	if opt.NoSleep {
		return s, nil
	}
	// Insert sleeps for pauses of a second or more between requests.
	var out []Step
	var lastEnd time.Time
	for i, st := range s.Steps {
		if i > 0 && !starts[i].IsZero() && !lastEnd.IsZero() {
			if gap := starts[i].Sub(lastEnd); gap >= time.Second {
				secs := math.Round(gap.Seconds())
				out = append(out, Step{Sleep: fmt.Sprintf("%gs", secs)})
			}
		}
		if ends[i].After(lastEnd) {
			lastEnd = ends[i]
		}
		out = append(out, st)
	}
	s.Steps = out
	return s, nil
}

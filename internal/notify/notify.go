// Package notify tells other tools about a run: chat webhooks (Slack,
// Discord, or any URL that takes JSON) when it ends, and Grafana
// annotations that mark the test window on every dashboard.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ymg4798/lt/internal/run"
)

var client = &http.Client{Timeout: 10 * time.Second}

func post(ctx context.Context, method, u string, body any, header http.Header) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("%s: status %d: %s", redact(u), resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// redact hides the secret part of webhook URLs in error messages.
func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "webhook"
	}
	return p.Scheme + "://" + p.Host + "/…"
}

// Webhook posts the run's outcome. Slack and Discord webhooks get their
// message format; other URLs get {text, result} where result is the
// result without per-second snapshots.
func Webhook(ctx context.Context, u, text string, r *run.Result) error {
	var body any
	switch host := hostOf(u); {
	case strings.HasSuffix(host, "hooks.slack.com"):
		body = map[string]string{"text": text}
	case strings.HasSuffix(host, "discord.com") || strings.HasSuffix(host, "discordapp.com"):
		body = map[string]string{"content": text}
	default:
		slim := *r
		slim.Snapshots = nil
		body = map[string]any{"text": text, "result": &slim}
	}
	_, err := post(ctx, http.MethodPost, u, body, nil)
	return err
}

// Text posts a plain message: Slack and Discord in their format, other
// URLs as {"text": ...}.
func Text(ctx context.Context, u, text string) error {
	var body any = map[string]string{"text": text}
	if h := hostOf(u); strings.HasSuffix(h, "discord.com") || strings.HasSuffix(h, "discordapp.com") {
		body = map[string]string{"content": text}
	}
	_, err := post(ctx, http.MethodPost, u, body, nil)
	return err
}

func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Hostname()
}

// Grafana marks a test on Grafana dashboards as a time region.
type Grafana struct {
	URL   string // e.g. http://localhost:3000
	Token string // service account token; empty for anonymous access
	id    int64
	tags  []string
}

func (g *Grafana) header() http.Header {
	h := http.Header{}
	if g.Token != "" {
		h.Set("Authorization", "Bearer "+g.Token)
	}
	return h
}

// Start creates the annotation at the test's start.
func (g *Grafana) Start(ctx context.Context, name string, at time.Time) error {
	g.tags = []string{"lt", name}
	out, err := post(ctx, http.MethodPost, strings.TrimRight(g.URL, "/")+"/api/annotations", map[string]any{
		"time": at.UnixMilli(),
		"tags": g.tags,
		"text": "lt: " + name + " running",
	}, g.header())
	if err != nil {
		return err
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || resp.ID == 0 {
		return fmt.Errorf("grafana: unexpected reply %q", strings.TrimSpace(string(out)))
	}
	g.id = resp.ID
	return nil
}

// End turns the annotation into a region ending now, with the outcome.
func (g *Grafana) End(ctx context.Context, text string, verdict string) error {
	if g.id == 0 {
		return nil
	}
	_, err := post(ctx, http.MethodPatch, fmt.Sprintf("%s/api/annotations/%d", strings.TrimRight(g.URL, "/"), g.id), map[string]any{
		"timeEnd": time.Now().UnixMilli(),
		"text":    text,
		"tags":    append(g.tags, strings.ToLower(verdict)),
	}, g.header())
	return err
}

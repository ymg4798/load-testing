package logs

import (
	"regexp"
	"sort"
	"strings"

	"github.com/ymg4798/load-testing/internal/run"
)

const (
	maxGroups       = 1000 // distinct error groups tracked
	maxReportGroups = 100  // log groups added to the report
	maxGroupMessage = 300
)

var (
	reUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reQuoted = regexp.MustCompile(`"[^"]*"|'[^']*'|` + "`[^`]*`")
	reHex    = regexp.MustCompile(`(?i)\b(?:0x[0-9a-f]+|[0-9a-f]*[0-9][0-9a-f]*[a-f][0-9a-f]*|[0-9a-f]*[a-f][0-9a-f]*[0-9][0-9a-f]*)\b`)
	reNum    = regexp.MustCompile(`[-+]?\b\d+(?:[.,:]\d+)*(?:[eE][-+]?\d+)?(?:ms|us|µs|ns|s|m|h|kb|mb|gb|b|%)?\b`)
	reSpaces = regexp.MustCompile(`\s+`)
)

// Normalize turns a message into its pattern: UUIDs, quoted strings, hex
// IDs and numbers become placeholders, so the same failure groups together.
func Normalize(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = truncUTF8(msg, 1024)
	msg = reUUID.ReplaceAllString(msg, "<uuid>")
	msg = reQuoted.ReplaceAllString(msg, `"…"`)
	msg = reHex.ReplaceAllStringFunc(msg, func(s string) string {
		if len(s) < 8 && !strings.HasPrefix(strings.ToLower(s), "0x") {
			return s // short words like "a1b" or "cafe" stay
		}
		return "<id>"
	})
	msg = reNum.ReplaceAllString(msg, "<n>")
	msg = reSpaces.ReplaceAllString(strings.TrimSpace(msg), " ")
	return truncUTF8(msg, maxGroupMessage)
}

type group struct {
	run.ErrorGroup
}

type groupKey struct{ service, msg string }

// addGroup counts an error line; called with the store lock held.
func (s *Store) addGroup(l *run.LogLine, norm string) {
	k := groupKey{l.Source, norm}
	g := s.groups[k]
	if g == nil {
		if len(s.groups) >= maxGroups {
			s.groupsDropped++
			return
		}
		g = &group{run.ErrorGroup{Service: l.Source, Source: "log", Message: norm, First: l.Time}}
		s.groups[k] = g
	}
	g.Count++
	if l.Time.Before(g.First) {
		g.First = l.Time
	}
	if l.Time.After(g.Last) {
		g.Last = l.Time
	}
	if g.TraceID == "" && l.TraceID != "" {
		g.TraceID = l.TraceID
	}
	if g.Stack == "" {
		if st := l.Attrs["stack"]; st != "" {
			g.Stack = st
		}
	}
}

// groupMessage is the text an error line is grouped by: the message, or
// the error attribute when the message is empty.
func groupMessage(l *run.LogLine) string {
	m := l.Message
	if strings.TrimSpace(m) == "" {
		m = l.Attrs["error"]
	}
	return Normalize(m)
}

// ErrorGroups returns the error groups seen so far, most frequent first.
func (s *Store) ErrorGroups() []run.ErrorGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]run.ErrorGroup, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, g.ErrorGroup)
	}
	sortGroups(out)
	return out
}

func sortGroups(gs []run.ErrorGroup) {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Count != gs[j].Count {
			return gs[i].Count > gs[j].Count
		}
		if !gs[i].Last.Equal(gs[j].Last) {
			return gs[i].Last.After(gs[j].Last)
		}
		return gs[i].Message < gs[j].Message
	})
}

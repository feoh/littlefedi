// Package httpneg handles the small piece of HTTP content negotiation shared
// by the human-facing and ActivityPub endpoints.
package httpneg

import (
	"mime"
	"strconv"
	"strings"
)

const activityStreamsProfile = "https://www.w3.org/ns/activitystreams"

type preference struct {
	q           float64
	specificity int
	parameters  int
	order       int
	ok          bool
}

// ActivityPubPreferred reports whether Accept prefers an ActivityPub
// representation over HTML. A tie leaves the choice to the route.
func ActivityPubPreferred(accept string) bool {
	ap, html := preferences(accept)
	return preferred(ap, html)
}

// HTMLPreferred reports whether Accept prefers HTML over an ActivityPub
// representation. A tie leaves the choice to the route.
func HTMLPreferred(accept string) bool {
	ap, html := preferences(accept)
	return preferred(html, ap)
}

func preferences(accept string) (preference, preference) {
	ranges := parseAccept(accept)
	ap := better(
		quality(ranges, "application/activity+json", nil),
		quality(ranges, "application/ld+json", map[string]string{"profile": activityStreamsProfile}),
	)
	return ap, quality(ranges, "text/html", nil)
}

func preferred(a, b preference) bool {
	if !a.ok || a.q <= 0 {
		return false
	}
	if !b.ok || b.q <= 0 {
		return true
	}
	if a.q != b.q {
		return a.q > b.q
	}
	if a.specificity != b.specificity {
		return a.specificity > b.specificity
	}
	return a.order < b.order
}

func better(a, b preference) preference {
	if preferred(a, b) {
		return a
	}
	return b
}

type mediaRange struct {
	typeName, subtype string
	parameters        map[string]string
	q                 float64
	order             int
}

func parseAccept(accept string) []mediaRange {
	parts := splitMediaRanges(accept)
	ranges := make([]mediaRange, 0, len(parts))
	for order, part := range parts {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		typeName, subtype, ok := strings.Cut(strings.ToLower(mediaType), "/")
		if !ok || typeName == "" || subtype == "" {
			continue
		}
		q := 1.0
		if raw, ok := parameters["q"]; ok {
			q, err = strconv.ParseFloat(raw, 64)
			if err != nil || q < 0 || q > 1 {
				continue
			}
			delete(parameters, "q")
		}
		ranges = append(ranges, mediaRange{typeName: typeName, subtype: subtype, parameters: parameters, q: q, order: order})
	}
	return ranges
}

func quality(ranges []mediaRange, mediaType string, parameters map[string]string) preference {
	typeName, subtype, _ := strings.Cut(mediaType, "/")
	best := preference{order: len(ranges)}
	for _, candidate := range ranges {
		specificity := -1
		switch {
		case candidate.typeName == typeName && candidate.subtype == subtype:
			specificity = 2
		case candidate.typeName == typeName && candidate.subtype == "*":
			specificity = 1
		case candidate.typeName == "*" && candidate.subtype == "*":
			specificity = 0
		}
		if specificity < 0 || !parametersMatch(candidate.parameters, parameters) {
			continue
		}
		p := preference{q: candidate.q, specificity: specificity, parameters: len(candidate.parameters), order: candidate.order, ok: true}
		if !best.ok || p.specificity > best.specificity ||
			p.specificity == best.specificity && p.parameters > best.parameters ||
			p.specificity == best.specificity && p.parameters == best.parameters && p.order < best.order {
			best = p
		}
	}
	return best
}

func parametersMatch(want, have map[string]string) bool {
	for key, value := range want {
		if !strings.EqualFold(have[key], value) {
			return false
		}
	}
	return true
}

func splitMediaRanges(header string) []string {
	var parts []string
	start := 0
	quoted, escaped := false, false
	for i, r := range header {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			parts = append(parts, header[start:i])
			start = i + 1
		}
	}
	return append(parts, header[start:])
}

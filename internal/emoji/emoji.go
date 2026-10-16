package emoji

import "strings"

var known = func() map[string]bool {
	set := make(map[string]bool, len(UnicodeMap))
	for _, value := range UnicodeMap {
		set[base(value)] = true
	}
	return set
}()

func Known(value string) bool {
	return known[base(value)]
}

func base(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\ufe0f' || r >= '\U0001f3fb' && r <= '\U0001f3ff' {
			return -1
		}
		return r
	}, value)
}

func ParseCustom(value string) (shortcode, domain string, ok bool) {
	if len(value) < 5 || value[0] != ':' || value[len(value)-1] != ':' {
		return "", "", false
	}
	shortcode, domain, ok = strings.Cut(value[1:len(value)-1], "@")
	if !ok || shortcode == "" || domain == "" || strings.ContainsAny(shortcode+domain, ":/\\\r\n\t ") {
		return "", "", false
	}
	return shortcode, strings.ToLower(domain), true
}

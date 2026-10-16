package federation

import (
	"sort"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
)

func naturalLanguageFields(object map[string]interface{}) (content, summary, lang string) {
	content, _ = object["content"].(string)
	if content == "" {
		content, _ = object["name"].(string)
	}
	summary, _ = object["summary"].(string)

	contentValue, contentLang, contentPresent, contentValid := languageMapValue(object["contentMap"])
	summaryValue, summaryLang, summaryPresent, summaryValid := languageMapValue(object["summaryMap"])
	if contentValid {
		content = contentValue
	} else if content == "" && contentValue != "" {
		content = contentValue
	}
	if summaryValid {
		summary = summaryValue
	} else if summary == "" && summaryValue != "" {
		summary = summaryValue
	}
	if (contentPresent && !contentValid) || (summaryPresent && !summaryValid) {
		return content, summary, ""
	}
	if contentValid && summaryValid && contentLang != summaryLang {
		return content, summary, ""
	}
	if contentValid {
		return content, summary, contentLang
	}
	if summaryValid {
		return content, summary, summaryLang
	}
	if raw, ok := object["language"].(string); ok {
		lang = statusfilter.PrimaryLanguage(raw)
	}
	return content, summary, lang
}

func languageMapValue(raw interface{}) (value, lang string, present, valid bool) {
	if raw == nil {
		return "", "", false, false
	}
	present = true
	values, ok := raw.(map[string]interface{})
	if !ok {
		return "", "", true, false
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var fallback string
	for _, rawLang := range keys {
		rawValue := values[rawLang]
		lang = statusfilter.PrimaryLanguage(rawLang)
		switch v := rawValue.(type) {
		case string:
			if len(values) == 1 {
				return v, lang, true, true
			}
			if fallback == "" {
				fallback = v
			}
		case []interface{}:
			for _, item := range v {
				if text, ok := item.(string); ok {
					if len(values) == 1 && len(v) == 1 {
						return text, lang, true, true
					}
					if fallback == "" {
						fallback = text
					}
					break
				}
			}
		}
	}
	return fallback, "", true, false
}

func languageMap(lang, value string) map[string]interface{} {
	lang = statusfilter.PrimaryLanguage(strings.TrimSpace(lang))
	if lang == "" {
		return nil
	}
	return map[string]interface{}{lang: value}
}

package statusfilter

import (
	"html"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/language"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

var htmlTagPattern = regexp.MustCompile(`<[^>]+>`)

// PrimaryLanguage reduces a BCP 47 tag to its primary language. Invalid,
// private-use-only, and undetermined tags have no usable language.
func PrimaryLanguage(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(strings.SplitN(raw, "-", 2)[0], "und") {
		return ""
	}
	if len(raw) == 2 || len(raw) == 3 {
		letters := true
		for i := range len(raw) {
			if (raw[i] < 'a' || raw[i] > 'z') && (raw[i] < 'A' || raw[i] > 'Z') {
				letters = false
				break
			}
		}
		if letters {
			return strings.ToLower(raw)
		}
	}
	tag, err := language.Parse(raw)
	if err != nil {
		return ""
	}
	if tag.String() == "und" || strings.HasPrefix(tag.String(), "x-") {
		return ""
	}
	base, confidence := tag.Base()
	if confidence == language.No || base.String() == "und" {
		return ""
	}
	return strings.ToLower(base.String())
}

func NormalizeLanguages(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		lang := PrimaryLanguage(raw)
		if lang != "" && !seen[lang] {
			seen[lang] = true
			out = append(out, lang)
		}
	}
	return out
}

// Apply evaluates active filters and returns top-level status copies. A hide
// match removes the status; a warn match annotates it with a FilterResult.
func Apply(filters []*models.Filter, scope string, statuses []*models.Status, now time.Time) []*models.Status {
	type compiledKeyword struct {
		original, needle string
		wholeWord        bool
	}
	type compiledFilter struct {
		filter    *models.Filter
		keywords  []compiledKeyword
		languages []string
	}
	active := make([]compiledFilter, 0, len(filters))
	for _, f := range filters {
		if f != nil && (f.ExpiresAt == nil || !now.After(*f.ExpiresAt)) && contains(f.Context, scope) {
			compiled := compiledFilter{filter: f, keywords: make([]compiledKeyword, 0, len(f.Keywords)), languages: NormalizeLanguages(f.Languages)}
			seenKeywords := make(map[string]bool, len(f.Keywords))
			for _, keyword := range f.Keywords {
				if needle := strings.ToLower(strings.TrimSpace(keyword.Keyword)); needle != "" {
					key := "part:" + needle
					if keyword.WholeWord {
						key = "word:" + key
					}
					if seenKeywords[key] {
						continue
					}
					seenKeywords[key] = true
					compiled.keywords = append(compiled.keywords, compiledKeyword{original: keyword.Keyword, needle: needle, wholeWord: keyword.WholeWord})
				}
			}
			if len(compiled.keywords) > 0 || len(compiled.languages) > 0 {
				active = append(active, compiled)
			}
		}
	}
	if len(active) == 0 {
		return statuses
	}

	out := make([]*models.Status, 0, len(statuses))
	languageCache := make(map[string]string)
	primaryLanguage := func(raw string) string {
		if language, ok := languageCache[raw]; ok {
			return language
		}
		language := PrimaryLanguage(raw)
		languageCache[raw] = language
		return language
	}
	for _, original := range statuses {
		if original == nil {
			continue
		}
		st := *original
		st.FilterMatches = nil
		text, languages := displayedContent(&st, primaryLanguage)
		hide := false
		for _, compiled := range active {
			keywordMatches := make([]string, 0, len(compiled.keywords))
			for _, keyword := range compiled.keywords {
				if (!keyword.wholeWord && strings.Contains(text, keyword.needle)) || (keyword.wholeWord && wholeWordContains(text, keyword.needle)) {
					keywordMatches = append(keywordMatches, keyword.original)
				}
			}
			languageMatches := intersect(compiled.languages, languages)
			languageMatched := len(languageMatches) > 0
			if compiled.filter.InvertLanguages && len(compiled.languages) > 0 {
				languageMatches = languageMatches[:0]
				for _, language := range languages {
					if !contains(compiled.languages, language) {
						languageMatches = append(languageMatches, language)
					}
				}
				languageMatched = (len(languages) == 0 && !compiled.filter.ShowUnknownLanguages) || len(languageMatches) > 0
			}
			if len(keywordMatches) == 0 && !languageMatched {
				continue
			}
			if compiled.filter.FilterAction == "hide" {
				hide = true
				break
			}
			st.FilterMatches = append(st.FilterMatches, models.FilterMatch{
				Filter: compiled.filter, KeywordMatches: keywordMatches, StatusMatches: []string{}, LanguageMatches: languageMatches,
			})
		}
		if !hide {
			out = append(out, &st)
		}
	}
	return out
}

func displayedContent(st *models.Status, primaryLanguage func(string) string) (string, []string) {
	if st.BoostOf != nil {
		st = st.BoostOf
	}
	var texts, languages []string
	seen := map[*models.Status]bool{}
	var add func(*models.Status)
	add = func(current *models.Status) {
		if current == nil || seen[current] {
			return
		}
		seen[current] = true
		texts = append(texts, current.ContentHTML, current.Summary)
		if current.Poll != nil {
			for _, option := range current.Poll.Options {
				texts = append(texts, option.Title)
			}
		}
		if lang := primaryLanguage(current.Language); lang != "" && !contains(languages, lang) {
			languages = append(languages, lang)
		}
		add(current.QuoteOf)
	}
	add(st)
	plain := html.UnescapeString(htmlTagPattern.ReplaceAllString(strings.Join(texts, " "), " "))
	return strings.ToLower(plain), languages
}

func wholeWordContains(text, needle string) bool {
	for offset := 0; ; {
		i := strings.Index(text[offset:], needle)
		if i < 0 {
			return false
		}
		i += offset
		beforeOK := i == 0
		if !beforeOK {
			r, _ := utf8.DecodeLastRuneInString(text[:i])
			beforeOK = !wordRune(r)
		}
		after := i + len(needle)
		afterOK := after == len(text)
		if !afterOK {
			r, _ := utf8.DecodeRuneInString(text[after:])
			afterOK = !wordRune(r)
		}
		if beforeOK && afterOK {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		offset = i + size
	}
}

func wordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)
}

func intersect(want, have []string) []string {
	out := []string{}
	for _, value := range want {
		if contains(have, value) {
			out = append(out, value)
		}
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

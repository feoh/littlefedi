package statusfilter

import (
	"reflect"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func TestPrimaryLanguage(t *testing.T) {
	tests := map[string]string{"en": "en", "EN": "en", "pt-BR": "pt", "zh-Hant-TW": "zh", "und": "", "": "", "x-klingon": ""}
	for in, want := range tests {
		if got := PrimaryLanguage(in); got != want {
			t.Errorf("PrimaryLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeLanguages(t *testing.T) {
	got := NormalizeLanguages([]string{"en-US", "en", "fr", "bogus!!", "FR"})
	if want := []string{"en", "fr"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeLanguages = %v, want %v", got, want)
	}
}

func TestApplyKeywordFilters(t *testing.T) {
	now := time.Now()
	statuses := []*models.Status{
		{ID: "1", ContentHTML: "<p>I love <b>Spoilers</b> for the finale</p>"},
		{ID: "2", ContentHTML: "<p>A post about cats</p>"},
		{ID: "3", ContentHTML: "<p>catsup on fries</p>"},
	}
	filters := []*models.Filter{
		{Title: "warn", Context: []string{"home"}, FilterAction: "warn", Keywords: []models.FilterKeyword{{Keyword: "spoilers", WholeWord: true}}},
		{Title: "hide", Context: []string{"home"}, FilterAction: "hide", Keywords: []models.FilterKeyword{{Keyword: "cats", WholeWord: true}}},
	}

	out := Apply(filters, "home", statuses, now)
	if len(out) != 2 || out[0].ID != "1" || out[1].ID != "3" {
		t.Fatalf("Apply kept %v, want statuses 1 and 3", ids(out))
	}
	if len(out[0].FilterMatches) != 1 || out[0].FilterMatches[0].Filter.Title != "warn" {
		t.Errorf("status 1 should carry the warn match, got %+v", out[0].FilterMatches)
	}
	if len(out[1].FilterMatches) != 0 {
		t.Error("whole-word 'cats' should not match 'catsup'")
	}
	if statuses[0].FilterMatches != nil {
		t.Error("Apply mutated its input")
	}
}

func TestApplyIgnoresOtherContextsAndExpiredFilters(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	statuses := []*models.Status{{ID: "1", ContentHTML: "cats"}}
	filters := []*models.Filter{
		{Context: []string{"public"}, FilterAction: "hide", Keywords: []models.FilterKeyword{{Keyword: "cats"}}},
		{Context: []string{"home"}, FilterAction: "hide", ExpiresAt: &past, Keywords: []models.FilterKeyword{{Keyword: "cats"}}},
	}
	if out := Apply(filters, "home", statuses, time.Now()); len(out) != 1 {
		t.Errorf("Apply hid a status with an out-of-scope or expired filter")
	}
}

func TestApplyLanguageFilter(t *testing.T) {
	statuses := []*models.Status{{ID: "en", Language: "en"}, {ID: "de", Language: "de-AT"}}
	filters := []*models.Filter{{Context: []string{"home"}, FilterAction: "hide", Languages: []string{"de"}}}
	out := Apply(filters, "home", statuses, time.Now())
	if len(out) != 1 || out[0].ID != "en" {
		t.Errorf("language filter kept %v, want only en", ids(out))
	}
}

func ids(statuses []*models.Status) []string {
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = s.ID
	}
	return out
}

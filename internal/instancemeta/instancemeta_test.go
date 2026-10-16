package instancemeta

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

type fakeStore struct {
	meta *models.InstanceMeta
	err  error
}

func (f fakeStore) GetInstanceMeta(context.Context) (*models.InstanceMeta, error) {
	return f.meta, f.err
}

func TestLoadAppliesDefaults(t *testing.T) {
	meta := Load(context.Background(), nil, Meta{ContactEmail: "admin@example.com"})
	if meta.ShortDescription != DefaultShortDescription || meta.Description != DefaultDescription {
		t.Errorf("defaults not applied: %+v", meta)
	}
	if meta.ContactEmail != "admin@example.com" {
		t.Errorf("configured value lost: %q", meta.ContactEmail)
	}
}

func TestLoadPrefersStoredValues(t *testing.T) {
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := fakeStore{meta: &models.InstanceMeta{
		ShortDescription: "  Stored short  ",
		Description:      "Stored long",
		ContactEmail:     "",
		Rules:            "Be kind\n\n  No spam ",
		UpdatedAt:        updated,
	}}
	meta := Load(context.Background(), store, Meta{ContactEmail: "config@example.com"})
	if meta.ShortDescription != "Stored short" || meta.Description != "Stored long" {
		t.Errorf("stored descriptions not used: %+v", meta)
	}
	if meta.ContactEmail != "config@example.com" {
		t.Error("a blank stored value should not override configuration")
	}
	if !reflect.DeepEqual(meta.Rules, []string{"Be kind", "No spam"}) {
		t.Errorf("rules = %q", meta.Rules)
	}
	if !meta.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt = %v", meta.UpdatedAt)
	}
}

func TestLoadFallsBackOnStoreError(t *testing.T) {
	meta := Load(context.Background(), fakeStore{err: errors.New("boom")}, Meta{Description: "configured"})
	if meta.Description != "configured" {
		t.Errorf("Description = %q, want the configured value", meta.Description)
	}
}

func TestRulesRoundTrip(t *testing.T) {
	rules := []string{" Be kind ", "", "No spam"}
	joined := JoinRules(rules)
	if joined != "Be kind\nNo spam" {
		t.Errorf("JoinRules = %q", joined)
	}
	if got := SplitRules(joined); !reflect.DeepEqual(got, []string{"Be kind", "No spam"}) {
		t.Errorf("SplitRules = %q", got)
	}
}

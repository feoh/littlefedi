package storage

import (
	"context"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Filter caching on cachingStorage.
//
// ListFilters runs on every timeline page build in both front ends, but
// filters change only when the user edits them in settings. The full per-user
// filter set is cached here, keyed by owner, with copies on read so the
// compiled-regex consumers can never mutate the cached slice. All filter
// writes flow through CreateFilter/UpdateFilter/DeleteFilter (keyword rows are
// rewritten inside those), so those three overrides are the complete
// invalidation surface; the entity TTL is the backstop.

func keyFilters(accountID string) string { return "filters:" + accountID }

func copyFilters(in []*models.Filter) []*models.Filter {
	out := make([]*models.Filter, len(in))
	for i, f := range in {
		if f == nil {
			continue
		}
		cp := *f
		if f.ExpiresAt != nil {
			t := *f.ExpiresAt
			cp.ExpiresAt = &t
		}
		cp.Context = append([]string(nil), f.Context...)
		cp.Languages = append([]string(nil), f.Languages...)
		cp.Keywords = append([]models.FilterKeyword(nil), f.Keywords...)
		cp.Statuses = append([]any(nil), f.Statuses...)
		out[i] = &cp
	}
	return out
}

func filtersSize(in []*models.Filter) int {
	n := 32
	for _, f := range in {
		if f == nil {
			continue
		}
		n += 160 + len(f.ID) + len(f.Title) + len(f.ContextJSON) + len(f.LanguagesJSON)
		for _, language := range f.Languages {
			n += len(language)
		}
		for _, k := range f.Keywords {
			n += 64 + len(k.Keyword)
		}
	}
	return n
}

func (cs *cachingStorage) ListFilters(ctx context.Context, accountID string) ([]*models.Filter, error) {
	key := keyFilters(accountID)
	if v, ok := cs.c.Get(key); ok {
		return copyFilters(v.([]*models.Filter)), nil
	}
	version := cs.c.Version()
	filters, err := cs.Storage.ListFilters(ctx, accountID)
	if err != nil {
		return nil, err
	}
	pristine := copyFilters(filters)
	cs.c.SetIfVersion(version, key, pristine, filtersSize(pristine)+len(key), cs.ttl)
	return filters, nil
}

func (cs *cachingStorage) CreateFilter(ctx context.Context, filter *models.Filter) error {
	err := cs.Storage.CreateFilter(ctx, filter)
	if err == nil && filter != nil {
		cs.c.Delete(keyFilters(filter.AccountID))
	}
	return err
}

func (cs *cachingStorage) UpdateFilter(ctx context.Context, filter *models.Filter) error {
	err := cs.Storage.UpdateFilter(ctx, filter)
	if err == nil && filter != nil {
		cs.c.Delete(keyFilters(filter.AccountID))
	}
	return err
}

func (cs *cachingStorage) DeleteFilter(ctx context.Context, id, accountID string) error {
	err := cs.Storage.DeleteFilter(ctx, id, accountID)
	if err == nil {
		cs.c.Delete(keyFilters(accountID))
	}
	return err
}

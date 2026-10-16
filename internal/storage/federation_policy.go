package storage

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

const federationPolicyTTL = time.Minute

const federationPolicyInvalidatePayload = `{"kind":"federation.policy.invalidate"}`

type federationPolicySnapshot struct {
	mode       string
	commercial bool
	allows     map[string]struct{}
	bigTech    map[string]struct{}
	suspended  map[string]struct{}
}

type federationPolicyCache struct {
	mu       sync.Mutex
	loadedAt time.Time
	snapshot *federationPolicySnapshot
}

func (c *federationPolicyCache) invalidate() {
	c.mu.Lock()
	c.loadedAt = time.Time{}
	c.mu.Unlock()
}

func (s *bunStorage) InvalidateFederationPolicy() { s.policy.invalidate() }

func (s *bunStorage) policyChanged(ctx context.Context) {
	s.policy.invalidate()
	if s.dialect == "pg" {
		if _, err := s.sqldb.ExecContext(ctx, "SELECT pg_notify($1, $2)", "littlefedi_streaming", federationPolicyInvalidatePayload); err != nil {
			slog.Warn("federation policy invalidation notification failed", "error", err)
		}
	}
}

func (s *bunStorage) federationPolicySnapshot(ctx context.Context) (*federationPolicySnapshot, error) {
	s.policy.mu.Lock()
	defer s.policy.mu.Unlock()
	if s.policy.snapshot != nil && time.Since(s.policy.loadedAt) < federationPolicyTTL {
		return s.policy.snapshot, nil
	}
	state := new(models.FederationPolicyState)
	if err := s.db.NewSelect().Model(state).Where("id = 1").Scan(ctx); err != nil {
		return nil, err
	}
	var allows []*models.DomainAllow
	var commercial []*models.CommercialDomainBlock
	var blocks []*models.DomainBlock
	var instances []*models.Instance
	queries := []func() error{
		func() error { return s.db.NewSelect().Model(&allows).Scan(ctx) },
		func() error { return s.db.NewSelect().Model(&commercial).Scan(ctx) },
		func() error { return s.db.NewSelect().Model(&blocks).Where("severity = 'suspend'").Scan(ctx) },
		func() error { return s.db.NewSelect().Model(&instances).Where("suspended = TRUE").Scan(ctx) },
	}
	for _, query := range queries {
		if err := query(); err != nil {
			return nil, err
		}
	}
	snapshot := &federationPolicySnapshot{
		mode: state.Mode, commercial: state.CommercialBigTechEnabled,
		allows: make(map[string]struct{}, len(allows)), bigTech: make(map[string]struct{}, len(commercial)),
		suspended: make(map[string]struct{}, len(blocks)+len(instances)),
	}
	for _, allow := range allows {
		if domain, err := normalizeInstanceDomain(allow.Domain); err == nil {
			snapshot.allows[domain] = struct{}{}
		}
	}
	for _, block := range commercial {
		if domain, err := normalizeInstanceDomain(block.Domain); err == nil {
			snapshot.bigTech[domain] = struct{}{}
		}
	}
	for _, block := range blocks {
		if domain, err := normalizeInstanceDomain(block.Domain); err == nil {
			snapshot.suspended[domain] = struct{}{}
		}
	}
	for _, instance := range instances {
		if domain, err := normalizeInstanceDomain(instance.Domain); err == nil {
			snapshot.suspended[domain] = struct{}{}
		}
	}
	s.policy.snapshot, s.policy.loadedAt = snapshot, time.Now()
	return snapshot, nil
}

// FederationDomainAllowed evaluates manual suspension, explicit allows,
// commercial blocks, and allowlist mode in precedence order.
func (s *bunStorage) FederationDomainAllowed(ctx context.Context, domain string) (bool, string, error) {
	domain, err := normalizeDomain(domain)
	if err != nil {
		return false, "invalid_domain", err
	}
	p, err := s.federationPolicySnapshot(ctx)
	if err != nil {
		return false, "policy_unavailable", err
	}
	if _, ok := p.suspended[domain]; ok {
		return false, "manual_suspend", nil
	}
	_, explicitlyAllowed := p.allows[domain]
	if p.commercial && !explicitlyAllowed {
		if _, ok := p.bigTech[domain]; ok {
			return false, "commercial_bigtech", nil
		}
	}
	if p.mode == "allowlist" && !explicitlyAllowed {
		return false, "not_allowlisted", nil
	}
	return true, "allowed", nil
}

func (s *bunStorage) FederationPolicyMode(ctx context.Context) (string, error) {
	p, err := s.federationPolicySnapshot(ctx)
	if err != nil {
		return "", err
	}
	return p.mode, nil
}

func (s *bunStorage) CreateDomainAllow(ctx context.Context, allow *models.DomainAllow) error {
	domain, err := normalizeDomain(allow.Domain)
	if err != nil {
		return err
	}
	allow.Domain = domain
	if allow.ID == "" {
		allow.ID = newID()
	}
	if allow.CreatedAt.IsZero() {
		allow.CreatedAt = time.Now()
	}
	_, err = s.db.NewInsert().Model(allow).Exec(ctx)
	if err == nil {
		s.policyChanged(ctx)
	}
	return err
}

func (s *bunStorage) ImportDomainAllows(ctx context.Context, allows []*models.DomainAllow) (int, error) {
	seen := make(map[string]struct{}, len(allows))
	for _, allow := range allows {
		domain, err := normalizeDomain(allow.Domain)
		if err != nil {
			return 0, err
		}
		if _, ok := seen[domain]; ok {
			continue
		}
		seen[domain], allow.Domain = struct{}{}, domain
		if allow.ID == "" {
			allow.ID = newID()
		}
		if allow.CreatedAt.IsZero() {
			allow.CreatedAt = time.Now()
		}
	}
	inserted := 0
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for _, allow := range allows {
			result, err := tx.NewInsert().Model(allow).On("CONFLICT (domain) DO NOTHING").Exec(ctx)
			if err != nil {
				return err
			}
			n, _ := result.RowsAffected()
			inserted += int(n)
		}
		return nil
	})
	if err == nil {
		s.policyChanged(ctx)
	}
	return inserted, err
}

func (s *bunStorage) DeleteDomainAllow(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.DomainAllow)(nil)).Where("id = ?", id).Exec(ctx)
	if err == nil {
		s.policyChanged(ctx)
	}
	return err
}

func (s *bunStorage) GetDomainAllow(ctx context.Context, id string) (*models.DomainAllow, error) {
	allow := new(models.DomainAllow)
	err := s.db.NewSelect().Model(allow).Where("id = ?", id).Scan(ctx)
	return allow, err
}

func (s *bunStorage) ListDomainAllows(ctx context.Context) ([]*models.DomainAllow, error) {
	var allows []*models.DomainAllow
	err := s.db.NewSelect().Model(&allows).Order("domain ASC").Scan(ctx)
	return allows, err
}

func (s *bunStorage) GetFederationPolicyState(ctx context.Context) (*models.FederationPolicyState, error) {
	state := new(models.FederationPolicyState)
	err := s.db.NewSelect().Model(state).Where("id = 1").Scan(ctx)
	return state, err
}

func (s *bunStorage) SetFederationMode(ctx context.Context, mode string) error {
	if mode != "open" && mode != "allowlist" {
		return fmt.Errorf("invalid federation mode %q", mode)
	}
	_, err := s.db.NewUpdate().Model((*models.FederationPolicyState)(nil)).Set("mode = ?", mode).Where("id = 1").Exec(ctx)
	if err == nil {
		s.policyChanged(ctx)
	}
	return err
}

func (s *bunStorage) SetCommercialBigTechEnabled(ctx context.Context, enabled bool) error {
	_, err := s.db.NewUpdate().Model((*models.FederationPolicyState)(nil)).Set("commercial_bigtech_enabled = ?", enabled).Where("id = 1").Exec(ctx)
	if err == nil {
		s.policyChanged(ctx)
	}
	return err
}

func (s *bunStorage) AcquireFederationPolicyLease(ctx context.Context, until time.Time) (bool, error) {
	result, err := s.db.NewUpdate().Model((*models.FederationPolicyState)(nil)).
		Set("lease_until = ?", until).Where("id = 1").
		Where("lease_until IS NULL OR lease_until < ?", time.Now()).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *bunStorage) ReleaseFederationPolicyLease(ctx context.Context) error {
	_, err := s.db.NewUpdate().Model((*models.FederationPolicyState)(nil)).
		Set("lease_until = NULL").Where("id = 1").Exec(ctx)
	return err
}

func (s *bunStorage) UpdateFederationPolicySync(ctx context.Context, state *models.FederationPolicyState) error {
	_, err := s.db.NewUpdate().Model((*models.FederationPolicyState)(nil)).Where("id = 1").
		Set("source_url = ?", state.SourceURL).Set("etag = ?", state.ETag).
		Set("last_modified = ?", state.LastModified).Set("last_attempt = ?", state.LastAttempt).
		Set("last_success = ?", state.LastSuccess).Set("last_error = ?", state.LastError).Exec(ctx)
	return err
}

func (s *bunStorage) ListCommercialDomainBlocks(ctx context.Context) ([]*models.CommercialDomainBlock, error) {
	var blocks []*models.CommercialDomainBlock
	err := s.db.NewSelect().Model(&blocks).Order("domain ASC").Scan(ctx)
	return blocks, err
}

func (s *bunStorage) ReplaceCommercialDomainBlocks(ctx context.Context, blocks []*models.CommercialDomainBlock, state *models.FederationPolicyState) error {
	if len(blocks) == 0 {
		return fmt.Errorf("commercial domain list is empty")
	}
	seen := make(map[string]struct{}, len(blocks))
	for _, block := range blocks {
		domain, err := normalizeDomain(block.Domain)
		if err != nil {
			return err
		}
		if _, ok := seen[domain]; ok {
			return fmt.Errorf("duplicate commercial domain %q", domain)
		}
		seen[domain], block.Domain = struct{}{}, domain
	}
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*models.CommercialDomainBlock)(nil)).Where("1 = 1").Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(&blocks).Exec(ctx); err != nil {
			return err
		}
		q := tx.NewUpdate().Model((*models.FederationPolicyState)(nil)).Where("id = 1").
			Set("source_url = ?", state.SourceURL).Set("etag = ?", state.ETag).
			Set("last_modified = ?", state.LastModified).Set("last_attempt = ?", state.LastAttempt).
			Set("last_success = ?", state.LastSuccess).Set("last_error = ?", state.LastError)
		_, err := q.Exec(ctx)
		return err
	})
	if err == nil {
		s.policyChanged(ctx)
	}
	return err
}

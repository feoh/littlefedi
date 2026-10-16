package storage

// The relationship cache memoises the per-viewer and instance-wide ID sets
// that every timeline query needs (blocks, mutes, follows, followed tags,
// domain blocks, moderation flags). Without it each cold timeline build
// re-runs five pre-queries and attaches every moderation subquery even when
// the sets are empty - the common case. Entries are invalidated by the write
// methods in bun.go and bounded by a short TTL so a missed path (or another
// process in PostgreSQL mode) can only cause brief staleness.

import (
	"context"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

const (
	relCacheTTL        = 5 * time.Minute
	relCacheMaxViewers = 64
)

// viewerRelationships holds one viewer's timeline-filtering sets. All slices
// are read-only once stored; callers must copy before mutating.
type viewerRelationships struct {
	hiddenAuthorIDs []string // blocks (both directions) + active mutes
	// notifHiddenIDs is the notification-list variant: blocks (both
	// directions) + active mutes with hide_notifications only. A plain mute
	// hides timelines but not notifications, so it can't reuse
	// hiddenAuthorIDs.
	notifHiddenIDs  []string
	nextMuteExpiry  time.Time
	hasDomainBlocks bool
	followingIDs    []string // accepted follows (viewer's own ID not included)
	followedTagIDs  []string
	// exclusiveListMemberIDs are members of the viewer's exclusive lists,
	// hidden from the home timeline read-time. Empty for almost everyone,
	// which skips the home predicate entirely.
	exclusiveListMemberIDs []string
	fetchedAt              time.Time
}

// instanceRelationships holds the instance-wide moderation flags that decide
// whether the shared moderation subqueries need to run at all.
type instanceRelationships struct {
	anySuspended        bool
	anySilencedDomains  bool
	anySilencedAccounts bool
	// Inbound-gate flags: whether any suspend-severity domain block or
	// suspended instance exists, so hostInboundBlocked can skip its per-host
	// lookups on unmoderated instances.
	anySuspendDomainBlocks bool
	anySuspendedInstances  bool
	fetchedAt              time.Time
}

type relationshipCache struct {
	mu       sync.RWMutex
	viewers  map[string]*viewerRelationships
	instance *instanceRelationships
}

func (c *relationshipCache) viewer(id string, now time.Time) *viewerRelationships {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry := c.viewers[id]
	if entry == nil || now.Sub(entry.fetchedAt) > relCacheTTL {
		return nil
	}
	// A timed mute elapsing must show the muted account again without any
	// write happening, so expiry acts like an invalidation deadline.
	if !entry.nextMuteExpiry.IsZero() && now.After(entry.nextMuteExpiry) {
		return nil
	}
	return entry
}

func (c *relationshipCache) storeViewer(id string, entry *viewerRelationships) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.viewers == nil {
		c.viewers = make(map[string]*viewerRelationships)
	}
	if len(c.viewers) >= relCacheMaxViewers {
		oldestID := ""
		var oldest time.Time
		for vid, v := range c.viewers {
			if oldestID == "" || v.fetchedAt.Before(oldest) {
				oldestID, oldest = vid, v.fetchedAt
			}
		}
		delete(c.viewers, oldestID)
	}
	c.viewers[id] = entry
}

func (c *relationshipCache) invalidateViewers(ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.viewers, id)
	}
}

func (c *relationshipCache) invalidateAllViewers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.viewers = nil
}

func (c *relationshipCache) getInstance(now time.Time) *instanceRelationships {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.instance == nil || now.Sub(c.instance.fetchedAt) > relCacheTTL {
		return nil
	}
	return c.instance
}

func (c *relationshipCache) storeInstance(entry *instanceRelationships) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instance = entry
}

func (c *relationshipCache) invalidateInstance() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instance = nil
}

func (c *relationshipCache) instanceFlagsSet() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.instance != nil && (c.instance.anySuspended || c.instance.anySilencedDomains || c.instance.anySilencedAccounts)
}

// InvalidateRelationshipCaches drops every cached relationship set. In
// PostgreSQL multi-process mode the broker consumer calls this on remote
// cache-invalidation events; the TTL bounds anything a lost event could miss.
func (s *bunStorage) InvalidateRelationshipCaches() {
	s.rels.invalidateAllViewers()
	s.rels.invalidateInstance()
}

// viewerRelsFor returns the viewer's cached relationship sets, fetching them
// with a constant handful of indexed queries on miss.
func (s *bunStorage) viewerRelsFor(ctx context.Context, viewerID string) (*viewerRelationships, error) {
	now := time.Now()
	if entry := s.rels.viewer(viewerID, now); entry != nil {
		return entry, nil
	}

	blocked, err := s.BlockedAccountIDs(ctx, viewerID)
	if err != nil {
		return nil, err
	}

	var mutes []*models.Mute
	if err := s.db.NewSelect().Model(&mutes).
		Where("account_id = ?", viewerID).
		Scan(ctx); err != nil {
		return nil, err
	}
	hidden := blocked
	// Separate backing array: hidden aliases blocked, so the notification set
	// must not append to the same slice.
	notifHidden := append([]string(nil), blocked...)
	var nextExpiry time.Time
	for _, m := range mutes {
		if muteExpired(m, now) {
			continue
		}
		hidden = append(hidden, m.TargetAccountID)
		if m.HideNotifications {
			notifHidden = append(notifHidden, m.TargetAccountID)
		}
		if m.Duration != nil {
			expiry := m.CreatedAt.Add(time.Duration(*m.Duration) * time.Minute)
			if nextExpiry.IsZero() || expiry.Before(nextExpiry) {
				nextExpiry = expiry
			}
		}
	}

	var followingIDs []string
	if err := s.db.NewSelect().Model((*models.Follow)(nil)).
		Column("target_account_id").
		Where("account_id = ?", viewerID).
		Where("state = ?", models.FollowAccepted).
		Scan(ctx, &followingIDs); err != nil {
		return nil, err
	}

	followedTagIDs, err := s.GetFollowedTagIDs(ctx, viewerID)
	if err != nil {
		return nil, err
	}

	hasDomainBlocks, err := s.db.NewSelect().Model((*models.AccountDomainBlock)(nil)).
		Where("account_id = ?", viewerID).
		Exists(ctx)
	if err != nil {
		return nil, err
	}

	var exclusiveMemberIDs []string
	if err := s.db.NewSelect().
		TableExpr("list_accounts AS la").
		ColumnExpr("DISTINCT la.account_id").
		Join("JOIN lists AS l ON l.id = la.list_id").
		Where("l.account_id = ?", viewerID).
		Where("l.exclusive = ?", true).
		Scan(ctx, &exclusiveMemberIDs); err != nil {
		return nil, err
	}

	entry := &viewerRelationships{
		hiddenAuthorIDs:        hidden,
		notifHiddenIDs:         notifHidden,
		nextMuteExpiry:         nextExpiry,
		hasDomainBlocks:        hasDomainBlocks,
		followingIDs:           followingIDs,
		followedTagIDs:         followedTagIDs,
		exclusiveListMemberIDs: exclusiveMemberIDs,
		fetchedAt:              now,
	}
	s.rels.storeViewer(viewerID, entry)
	return entry, nil
}

// instanceRelsFor returns the instance-wide moderation flags, refreshed with
// three indexed EXISTS probes on miss.
func (s *bunStorage) instanceRelsFor(ctx context.Context) (*instanceRelationships, error) {
	now := time.Now()
	if entry := s.rels.getInstance(now); entry != nil {
		return entry, nil
	}

	anySuspended, err := s.db.NewSelect().Model((*models.Account)(nil)).
		Where("suspended_at IS NOT NULL").
		Exists(ctx)
	if err != nil {
		return nil, err
	}
	anySilencedDomains, err := s.db.NewSelect().Model((*models.DomainBlock)(nil)).
		Where("severity IN ('silence', 'suspend')").
		Exists(ctx)
	if err != nil {
		return nil, err
	}
	anySilencedAccounts, err := s.db.NewSelect().Model((*models.Account)(nil)).
		Where("moderation_state IN ('silenced', 'quiet')").
		Exists(ctx)
	if err != nil {
		return nil, err
	}
	anySuspendDomainBlocks, err := s.db.NewSelect().Model((*models.DomainBlock)(nil)).
		Where("severity = 'suspend'").
		Exists(ctx)
	if err != nil {
		return nil, err
	}
	anySuspendedInstances, err := s.db.NewSelect().Model((*models.Instance)(nil)).
		Where("suspended").
		Exists(ctx)
	if err != nil {
		return nil, err
	}

	entry := &instanceRelationships{
		anySuspended:           anySuspended,
		anySilencedDomains:     anySilencedDomains,
		anySilencedAccounts:    anySilencedAccounts,
		anySuspendDomainBlocks: anySuspendDomainBlocks,
		anySuspendedInstances:  anySuspendedInstances,
		fetchedAt:              now,
	}
	s.rels.storeInstance(entry)
	return entry, nil
}

// InboundHostBlocksPossible reports whether any suspend-severity domain block
// or suspended instance exists at all, from the cached instance-wide flags.
func (s *bunStorage) InboundHostBlocksPossible(ctx context.Context) (bool, error) {
	entry, err := s.instanceRelsFor(ctx)
	if err != nil {
		return false, err
	}
	return entry.anySuspendDomainBlocks || entry.anySuspendedInstances, nil
}

// relsAccountWritten invalidates the instance flags when an account update
// could flip them: the account is (or is being) moderated, or a flag is
// already set and might now turn off. Routine remote profile merges, the
// bulk of UpdateAccount traffic, skip both branches and keep the cache warm.
func (s *bunStorage) relsAccountWritten(account *models.Account) {
	if account == nil {
		return
	}
	moderated := account.SuspendedAt != nil ||
		account.ModerationState == "silenced" || account.ModerationState == "quiet" ||
		account.ModerationState == "suspended" || account.ModerationState == "disabled"
	if moderated || s.rels.instanceFlagsSet() {
		s.rels.invalidateInstance()
	}
}

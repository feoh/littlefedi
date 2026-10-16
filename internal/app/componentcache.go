package app

import (
	"context"
	"log/slog"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// EnrichStatusesCached composes a status slice from immutable, per-viewer
// enriched components. All misses are enriched together, preserving the
// constant-query cold path.
//
// Contract: cached components are immutable below the top level. Hits are
// served as shallow top-level copies (see shallowReadCopy); callers may append
// to the empty top-level BoostedBy/FilterMatches slices but must never mutate
// the shared nested graph. Use WithInteractionCounts to update counters on a
// request-owned status and boost target.
func (s *Service) EnrichStatusesCached(ctx context.Context, statuses []*models.Status, viewer *models.Account) {
	if len(statuses) == 0 {
		return
	}
	if s.pageCache == nil {
		s.enrichStatusesLive(ctx, statuses, viewer)
		return
	}
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	version := s.pageCache.Version()
	misses := make([]*models.Status, 0, len(statuses))
	indices := make([]int, 0, len(statuses))
	for i, st := range statuses {
		if st == nil {
			continue
		}
		key := componentKey(viewerID, st.ID)
		if v, ok, result := s.pageCache.GetWithResult(key); ok && v.(*models.Status).UpdatedAt.Equal(st.UpdatedAt) && s.pageAudienceCurrent(ctx, []*models.Status{v.(*models.Status)}, viewerID) {
			statuses[i] = shallowReadCopy(v.(*models.Status))
		} else {
			slogComponent("miss", st.ID, viewerID, string(result))
			misses = append(misses, st)
			indices = append(indices, i)
		}
	}
	if len(misses) == 0 {
		return
	}
	s.enrichStatusesLive(ctx, misses, viewer)
	for j, st := range misses {
		if st == nil {
			continue
		}
		// The enriched status graph comes from entity-cache copies, so it can be
		// stored directly as the immutable component - no deep copy. Callers get
		// the same shallow top-level copy hits get. The raw source is dropped:
		// timeline rendering only needs ContentHTML, and every source reader
		// (edit form, /statuses/:id/source, AS2) fetches fresh via GetStatus.
		st.ContentSource = ""
		s.pageCache.SetIfVersionWithDependencies(version, componentKey(viewerID, st.ID), st,
			componentStatusSize(st), s.componentTTL, componentDependencies(st, viewerID))
		statuses[indices[j]] = shallowReadCopy(st)
	}
}

func componentKey(viewerID, statusID string) string {
	return ViewerCacheKeyPrefix(viewerID) + "component:" + statusID
}

func componentDependencies(st *models.Status, viewerID string) []string {
	seen := make(map[string]struct{})
	var deps []string
	add := func(v string) {
		if v != "" {
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				deps = append(deps, v)
			}
		}
	}
	var walk func(*models.Status)
	walk = func(cur *models.Status) {
		if cur == nil {
			return
		}
		key := "status:" + cur.ID
		if _, ok := seen[key]; ok {
			return
		}
		add(key)
		add("account:" + cur.AccountID)
		if cur.Poll != nil {
			add("poll:" + cur.Poll.ID)
		}
		walk(cur.InReplyTo)
		walk(cur.BoostOf)
		walk(cur.QuoteOf)
	}
	walk(st)
	add("viewer:" + viewerID)
	add("viewer-status:" + viewerID + ":" + st.ID)
	return deps
}

func componentStatusSize(st *models.Status) int {
	if st == nil {
		return 1
	}
	n := 768 + len(st.ID) + len(st.ContentHTML) + len(st.Summary)
	n += len(st.MediaAttachments)*256 + len(st.Mentions)*128 + len(st.Tags)*128 + len(st.Emojis)*192
	if st.Account != nil {
		n += 512 + len(st.Account.DisplayName) + len(st.Account.Note) + len(st.Account.Emojis)*192
	}
	if st.Poll != nil {
		n += 256 + len(st.Poll.Options)*128 + len(st.Poll.OwnVotes)*8
	}
	return n
}

func slogComponent(event, status, viewer, reason string) {
	// Kept in a helper so component diagnostics have one stable shape.
	slog.Debug("cache: component "+event, "status", status, "viewer", viewer, "reason", reason)
}

// InvalidateStatusComponents invalidates enriched components and rendered cards
// for a status, including components that reference it.
func (s *Service) InvalidateStatusComponents(statusID string) int {
	if s.pageCache == nil || statusID == "" {
		return 0
	}
	return s.pageCache.DeleteDependencies("status:" + statusID)
}

// InvalidateViewerStatus invalidates only viewer-specific interaction state.
func (s *Service) InvalidateViewerStatus(viewerID, statusID string) int {
	if s.pageCache == nil {
		return 0
	}
	return s.pageCache.DeleteDependencies("viewer-status:" + viewerID + ":" + statusID)
}

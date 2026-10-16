package app

// Shared list operations used by both the Mastodon API and the web UI, so
// validation, the followed-members invariant, and page-cache invalidation
// live in one place.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// ErrListNotFound is returned when the list does not exist or is owned by
// someone else (both render as 404, matching Mastodon).
var ErrListNotFound = errors.New("list not found")

// ErrListMemberNotFollowed is returned when a membership add targets an
// account the owner does not follow; Mastodon rejects with "Record not found".
var ErrListMemberNotFollowed = errors.New("account not followed")

// ErrListInvalid is returned for validation failures (empty title, unknown
// replies_policy); render as 422.
var ErrListInvalid = errors.New("invalid list")

func validRepliesPolicy(p string) bool {
	switch p {
	case models.ListRepliesFollowed, models.ListRepliesList, models.ListRepliesNone:
		return true
	}
	return false
}

// GetOwnedList loads a list and enforces ownership: a miss and a foreign list
// are indistinguishable to the caller (404), so list IDs are not enumerable.
func (s *Service) GetOwnedList(ctx context.Context, owner *models.Account, listID string) (*models.List, error) {
	if listID == "" {
		return nil, ErrListNotFound
	}
	list, err := s.store.GetList(ctx, listID)
	if err != nil || list == nil || list.AccountID != owner.ID {
		return nil, ErrListNotFound
	}
	return list, nil
}

func (s *Service) CreateList(ctx context.Context, owner *models.Account, title, repliesPolicy string, exclusive bool) (*models.List, error) {
	defer s.invalidatePages(owner.ID)
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrListInvalid)
	}
	if repliesPolicy == "" {
		repliesPolicy = models.ListRepliesList
	}
	if !validRepliesPolicy(repliesPolicy) {
		return nil, fmt.Errorf("%w: invalid replies_policy", ErrListInvalid)
	}
	list := &models.List{
		Title:         title,
		AccountID:     owner.ID,
		RepliesPolicy: repliesPolicy,
		Exclusive:     exclusive,
	}
	if err := s.store.CreateList(ctx, list); err != nil {
		return nil, err
	}
	return list, nil
}

// UpdateList applies the non-nil fields to an owned list.
func (s *Service) UpdateList(ctx context.Context, owner *models.Account, listID string, title, repliesPolicy *string, exclusive *bool) (*models.List, error) {
	defer s.invalidatePages(owner.ID)
	list, err := s.GetOwnedList(ctx, owner, listID)
	if err != nil {
		return nil, err
	}
	if title != nil {
		t := strings.TrimSpace(*title)
		if t == "" {
			return nil, fmt.Errorf("%w: title is required", ErrListInvalid)
		}
		list.Title = t
	}
	if repliesPolicy != nil && *repliesPolicy != "" {
		if !validRepliesPolicy(*repliesPolicy) {
			return nil, fmt.Errorf("%w: invalid replies_policy", ErrListInvalid)
		}
		list.RepliesPolicy = *repliesPolicy
	}
	if exclusive != nil {
		list.Exclusive = *exclusive
	}
	if err := s.store.UpdateList(ctx, list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Service) DeleteList(ctx context.Context, owner *models.Account, listID string) error {
	defer s.invalidatePages(owner.ID)
	list, err := s.GetOwnedList(ctx, owner, listID)
	if err != nil {
		return err
	}
	return s.store.DeleteList(ctx, list.ID)
}

// AddListMembers adds accounts to an owned list. Every target is validated
// (exists and is followed by the owner) before any insert, so the operation is
// all-or-nothing like Mastodon's.
func (s *Service) AddListMembers(ctx context.Context, owner *models.Account, listID string, accountIDs []string) error {
	defer s.invalidatePages(owner.ID)
	list, err := s.GetOwnedList(ctx, owner, listID)
	if err != nil {
		return err
	}
	for _, aid := range accountIDs {
		target, err := s.store.GetAccount(ctx, aid)
		if err != nil || target == nil {
			return ErrListMemberNotFollowed
		}
		if target.ID == owner.ID {
			continue // adding yourself is a silent no-op
		}
		following, err := s.store.IsFollowing(ctx, owner.ID, target.ID)
		if err != nil {
			return err
		}
		if !following {
			return ErrListMemberNotFollowed
		}
	}
	for _, aid := range accountIDs {
		if aid == owner.ID {
			continue
		}
		if err := s.store.AddAccountToList(ctx, list.ID, aid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RemoveListMembers(ctx context.Context, owner *models.Account, listID string, accountIDs []string) error {
	defer s.invalidatePages(owner.ID)
	list, err := s.GetOwnedList(ctx, owner, listID)
	if err != nil {
		return err
	}
	for _, aid := range accountIDs {
		if err := s.store.RemoveAccountFromList(ctx, list.ID, aid); err != nil {
			return err
		}
	}
	return nil
}

package blog

import (
	"context"
	"encoding/json"
	"fmt"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

type RebuildRequest struct {
	AccountID string `json:"account_id"`
	PostID    string `json:"post_id,omitempty"`
}

func (r RebuildRequest) Key() string {
	if r.PostID != "" {
		return r.AccountID + ":" + r.PostID
	}
	return r.AccountID
}

// ParseRebuildRequest accepts new JSON payloads and legacy account-ID jobs.
func ParseRebuildRequest(payload string) RebuildRequest {
	var req RebuildRequest
	if json.Unmarshal([]byte(payload), &req) == nil && req.AccountID != "" {
		return req
	}
	return RebuildRequest{AccountID: payload}
}

// Rebuild is the RebuildJobKind job handler body: payload is the account ID
// whose site should be regenerated.
func Rebuild(ctx context.Context, store storage.Storage, cfg *config.Config, payload string) error {
	req := ParseRebuildRequest(payload)
	account, err := store.GetAccount(ctx, req.AccountID)
	if err != nil {
		return fmt.Errorf("blog: load account %q: %w", req.AccountID, err)
	}
	if req.PostID != "" {
		return GeneratePost(ctx, store, cfg, account, req.PostID)
	}
	return Generate(ctx, store, cfg, account)
}

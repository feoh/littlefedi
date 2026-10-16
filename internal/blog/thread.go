package blog

import (
	"context"
	"html/template"
	"sort"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

type blogReply struct {
	Handle      string
	Permalink   string
	Date        time.Time
	Summary     string
	ContentHTML template.HTML
	Attachments []replyAttachment
	Children    []*blogReply
}

type replyAttachment struct{ URL, Label string }

func buildReplies(ctx context.Context, store storage.Storage, post *models.Status) ([]*blogReply, bool, error) {
	_, descendants, err := store.GetStatusContext(ctx, post.ID)
	if err != nil {
		return nil, false, err
	}
	truncated := len(descendants) == 100
	eligible := make([]*models.Status, 0, len(descendants))
	accountIDs, statusIDs := make([]string, 0, len(descendants)), make([]string, 0, len(descendants))
	seenAccounts := make(map[string]bool)
	for _, status := range descendants {
		if status.Visibility != models.VisibilityPublic && status.Visibility != models.VisibilityUnlisted {
			continue
		}
		eligible = append(eligible, status)
		statusIDs = append(statusIDs, status.ID)
		if !seenAccounts[status.AccountID] {
			seenAccounts[status.AccountID] = true
			accountIDs = append(accountIDs, status.AccountID)
		}
	}
	accounts, err := store.GetAccountsByIDs(ctx, accountIDs)
	if err != nil {
		return nil, false, err
	}
	media, err := store.GetMediaForStatuses(ctx, statusIDs)
	if err != nil {
		return nil, false, err
	}
	nodes := make(map[string]*blogReply, len(eligible))
	for _, status := range eligible {
		account := accounts[status.AccountID]
		handle := "unknown"
		if account != nil {
			handle = account.Username
			if account.Domain != nil && *account.Domain != "" {
				handle += "@" + *account.Domain
			}
		}
		permalink := status.URL
		if permalink == "" {
			permalink = status.URI
		}
		node := &blogReply{
			Handle: handle, Permalink: permalink, Date: status.CreatedAt, Summary: status.Summary,
			ContentHTML: template.HTML(content.SanitizeHTMLStr(status.ContentHTML)), //nolint:gosec -- sanitized immediately above
		}
		for _, attachment := range media[status.ID] {
			label := strings.TrimSpace(attachment.Description)
			if label == "" {
				label = attachment.Type + " attachment"
			}
			node.Attachments = append(node.Attachments, replyAttachment{URL: attachment.URL, Label: label})
		}
		nodes[status.ID] = node
	}
	var roots []*blogReply
	for _, status := range eligible {
		node := nodes[status.ID]
		if status.InReplyToID != nil && nodes[*status.InReplyToID] != nil {
			nodes[*status.InReplyToID].Children = append(nodes[*status.InReplyToID].Children, node)
		} else {
			roots = append(roots, node)
		}
	}
	var sortTree func([]*blogReply)
	sortTree = func(replies []*blogReply) {
		sort.SliceStable(replies, func(i, j int) bool {
			if replies[i].Date.Equal(replies[j].Date) {
				return replies[i].Permalink < replies[j].Permalink
			}
			return replies[i].Date.Before(replies[j].Date)
		})
		for _, reply := range replies {
			sortTree(reply.Children)
		}
	}
	sortTree(roots)
	return roots, truncated, nil
}

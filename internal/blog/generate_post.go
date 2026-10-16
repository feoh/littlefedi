package blog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// GeneratePost atomically replaces one already-published post page. Reply
// changes do not rewrite the blog index, tags, archive or Atom feed.
func GeneratePost(ctx context.Context, store storage.Storage, cfg *config.Config, account *models.Account, postID string) (result error) {
	accountDir := filepath.Join(cfg.Blog.ResolvedOutputDir(), account.Username)
	if _, err := os.Stat(filepath.Join(accountDir, "index.html")); err != nil {
		return Generate(ctx, store, cfg, account)
	}
	generateMu.Lock()
	fallbackToFull := false
	defer func() {
		generateMu.Unlock()
		if fallbackToFull {
			result = Generate(ctx, store, cfg, account)
		}
	}()
	statuses, err := collectBlogStatuses(ctx, store, account.ID)
	if err != nil {
		return err
	}
	index := -1
	for i, status := range statuses {
		if status.ID == postID {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	status := statuses[index]
	if strings.TrimSpace(status.BlogSlug) == "" {
		return fmt.Errorf("blog: post %q has no published slug", postID)
	}
	tags, err := store.GetTagsForStatuses(ctx, []string{postID})
	if err != nil {
		return fmt.Errorf("blog: load post tags: %w", err)
	}
	media, err := store.GetMediaForStatuses(ctx, []string{postID})
	if err != nil {
		return fmt.Errorf("blog: load post media: %w", err)
	}
	var replies []*blogReply
	var truncated bool
	if account.BlogRepliesEnabled {
		replies, truncated, err = buildReplies(ctx, store, status)
		if err != nil {
			return fmt.Errorf("blog: load post replies: %w", err)
		}
	}
	tempRoot, err := os.MkdirTemp(accountDir, ".post-build-*")
	if err != nil {
		fallbackToFull = os.IsPermission(err)
		return fmt.Errorf("blog: create post staging dir: %w", err)
	}
	defer os.RemoveAll(tempRoot)

	blogRoot := "/@" + account.Username + "/blog"
	post := buildPost(cfg, tempRoot, blogRoot, status.BlogSlug, status, tags[postID], media[postID], replies, truncated, account.BlogRepliesEnabled, account.DefaultPostLanguage)
	var prev, next *sitePost
	if index > 0 {
		prev = postReference(blogRoot, statuses[index-1])
	}
	if index+1 < len(statuses) {
		next = postReference(blogRoot, statuses[index+1])
	}
	webDomain := strings.TrimSpace(cfg.WebDomain)
	if webDomain == "" {
		webDomain = cfg.LocalDomain
	}
	fallback := ""
	if strings.TrimSpace(account.Avatar) != "" {
		fallback = absoluteBlogURL(webDomain, account.Avatar)
	}
	siteTitle := strings.TrimSpace(account.DisplayName)
	if siteTitle == "" {
		siteTitle = "@" + account.Username
	}
	if err := writePostPage(tempRoot, siteTitle, account.Username, blogRoot, postOGImage(webDomain, post, fallback), post, prev, next); err != nil {
		return err
	}
	dest := filepath.Join(accountDir, filepath.FromSlash(status.BlogSlug))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return publishGeneratedSite(dest, filepath.Join(tempRoot, filepath.FromSlash(status.BlogSlug)))
}

func postReference(blogRoot string, status *models.Status) *sitePost {
	if status == nil || status.BlogSlug == "" {
		return nil
	}
	return &sitePost{
		ID: status.ID, Slug: status.BlogSlug,
		Permalink: blogRoot + "/" + status.BlogSlug + "/",
		Date:      status.CreatedAt, Title: deriveTitle(status.ContentSource, status.CreatedAt),
	}
}

//go:build blog

package webui

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blog"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

func (h *Handler) registerBlogRoutes(r chi.Router) {
	if !h.cfg.Blog.Enabled {
		return
	}
	if h.cfg.Blog.DirectoryEnabled {
		r.With(h.OptionalAuthMiddleware).Get("/blogs", h.BlogsPage)
		r.With(h.OptionalAuthMiddleware).Get("/sitemap.xml", h.BlogSitemapIndex)
	}
	blog.RegisterRoutes(r, h.cfg.Blog)
}

// BlogsPage lists generated local blogs whose authors have not opted out of
// discovery. A direct blog URL remains public when it is omitted here.
func (h *Handler) BlogsPage(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Blog.DirectoryEnabled {
		http.NotFound(w, r)
		return
	}
	if getAuthAccount(r.Context()) == nil && !h.publicBlogDirectoryEnabled() {
		redirectGuest(w, r, "/auth/sign_in")
		return
	}
	accounts, err := h.listedBlogs(r.Context())
	if err != nil {
		slog.Error("webui: list blogs", "error", err)
		http.Error(w, "Error loading blogs", http.StatusInternalServerError)
		return
	}
	h.renderPage(w, r, h.pageTitle(r, "blogs.title"), map[string]any{"Accounts": accounts}, "blogs.html")
}

func (h *Handler) listedBlogs(ctx context.Context) ([]*models.Account, error) {
	// Scan local accounts; add a paginated filtered query only if
	// large-instance directory traffic makes this measurable.
	accounts, err := h.store.GetLocalAccounts(ctx)
	if err != nil {
		return nil, err
	}
	listed := accounts[:0]
	for _, account := range accounts {
		if account.Discovery && account.CanAuthenticate(false) && h.blogAllowed(account) && h.blogSiteGenerated(account) {
			listed = append(listed, account)
		}
	}
	sort.Slice(listed, func(i, j int) bool {
		return strings.ToLower(listed[i].Username) < strings.ToLower(listed[j].Username)
	})
	return listed, nil
}

type sitemapIndex struct {
	XMLName  xml.Name     `xml:"sitemapindex"`
	XMLNS    string       `xml:"xmlns,attr"`
	Sitemaps []sitemapRef `xml:"sitemap"`
}

type sitemapRef struct {
	Loc string `xml:"loc"`
}

// BlogSitemapIndex lets robots.txt advertise every listed blog through one
// stable URL instead of growing one directive per account.
func (h *Handler) BlogSitemapIndex(w http.ResponseWriter, r *http.Request) {
	if getAuthAccount(r.Context()) == nil && !h.publicBlogDirectoryEnabled() {
		redirectGuest(w, r, "/auth/sign_in")
		return
	}
	accounts, err := h.listedBlogs(r.Context())
	if err != nil {
		slog.Error("webui: build blog sitemap index", "error", err)
		http.Error(w, "Error loading sitemap", http.StatusInternalServerError)
		return
	}
	index := sitemapIndex{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9", Sitemaps: make([]sitemapRef, len(accounts))}
	for i, account := range accounts {
		index.Sitemaps[i].Loc = "https://" + h.domain + "/@" + account.Username + "/blog/sitemap.xml"
	}
	out, err := xml.MarshalIndent(index, "", "  ")
	if err != nil {
		http.Error(w, "Error rendering sitemap", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if !h.publicBlogDirectoryEnabled() {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	_, _ = w.Write(append([]byte(xml.Header), out...))
}

// blogCapable reports whether this server can actually serve a static blog
// (compiled with -tags blog and enabled in config), so templates only offer
// the blog-post composer toggle when it will really work.
func (h *Handler) blogCapable() bool { return h.cfg.Blog.Enabled }

func (h *Handler) enqueueBlogSettingsRebuild(ctx context.Context, accountID string) {
	blog.EnqueueRebuild(ctx, h.store, blog.RebuildRequest{AccountID: accountID})
}

// enqueueBlogRebuildAll queues a rebuild job for every local account
// currently allowed to blog, and reports how many were queued.
func (h *Handler) enqueueBlogRebuildAll(ctx context.Context) (int, error) {
	accounts, err := h.store.GetLocalAccounts(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, account := range accounts {
		if !h.cfg.Blog.AccountAllowed(account.BlogAllowed) {
			continue
		}
		blog.EnqueueRebuild(ctx, h.store, blog.RebuildRequest{AccountID: account.ID})
		count++
	}
	return count, nil
}

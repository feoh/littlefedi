package blog

import (
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogslug"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// excerptRunes bounds the plain-text excerpt shown on index/tag/feed pages.
const excerptRunes = 280

type tagRef struct {
	Name string
	URL  string
}

type sitePost struct {
	ID string
	// Slug is the post's path inside the blog, e.g. "2026/07/25/hello". It is
	// persisted on the status row, so it survives edits and rebuilds.
	Slug             string
	Permalink        string
	Lang             string
	Date             time.Time
	ContentHTML      template.HTML
	Excerpt          string
	Title            string
	Tags             []tagRef
	Media            []mediaItem
	Sensitive        bool // gates the media disclosure, mirrors the live UI's CW behavior for media only
	ThreadURL        string
	Replies          []*blogReply
	RepliesTruncated bool
	ShowReplies      bool
}

type mediaItem struct {
	Type   string // image, video, audio, unknown
	URL    string
	Alt    string
	Width  int
	Height int
}

type tagGroup struct {
	Name  string // display name (original casing/text of the first post tagged with it)
	Slug  string
	Posts []sitePost
}

type archiveYear struct {
	Year  int
	Posts []sitePost
}

type tagCount struct {
	Name  string
	URL   string
	Count int
}

// generateMu serializes full-site builds and keeps readers off the short
// staging-directory swap. One global lock is enough at personal-blog
// scale; use per-account locks if rebuild throughput ever matters.
var generateMu sync.RWMutex

// Generate rebuilds one account's entire static blog site from scratch and
// writes it to cfg.Blog.ResolvedOutputDir()/<username>/. Regeneration is
// always full-site (no incremental cache): a personal blog is small enough
// that this stays cheap, and it avoids incremental-invalidation bugs.
func Generate(ctx context.Context, store storage.Storage, cfg *config.Config, account *models.Account) (err error) {
	start := time.Now()
	slog.Info("blog: rebuild starting", "account", account.Username)
	defer func() {
		if err != nil {
			slog.Error("blog: rebuild failed", "account", account.Username, "error", err, "duration", time.Since(start).Round(time.Millisecond))
		}
	}()

	generateMu.Lock()
	defer generateMu.Unlock()

	if account.Domain != nil {
		return fmt.Errorf("blog: account %q is not local", account.Username)
	}
	if err := accounts.ValidateUsername(account.Username); err != nil {
		return fmt.Errorf("blog: invalid local account: %w", err)
	}
	if !cfg.Blog.AccountAllowed(account.BlogAllowed) {
		return fmt.Errorf("blog: account %q is not permitted to blog", account.Username)
	}

	statuses, err := collectBlogStatuses(ctx, store, account.ID)
	if err != nil {
		return err
	}

	ids := make([]string, len(statuses))
	for i, st := range statuses {
		ids[i] = st.ID
	}
	tagsByStatus, err := store.GetTagsForStatuses(ctx, ids)
	if err != nil {
		return fmt.Errorf("blog: load tags: %w", err)
	}
	mediaByStatus, err := store.GetMediaForStatuses(ctx, ids)
	if err != nil {
		return fmt.Errorf("blog: load media: %w", err)
	}

	blogRoot := "/@" + account.Username + "/blog"
	siteTitle := strings.TrimSpace(account.DisplayName)
	if siteTitle == "" {
		siteTitle = "@" + account.Username
	}
	siteDescription := content.PlainText(account.Note)

	outputDir := cfg.Blog.ResolvedOutputDir()
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("blog: create output dir: %w", err)
	}
	root, err := os.MkdirTemp(outputDir, ".blog-build-*")
	if err != nil {
		return fmt.Errorf("blog: create staging dir: %w", err)
	}
	defer os.RemoveAll(root)

	// Slugs are seeded from every slug the account already owns (including
	// posts this build does not render, e.g. one turned non-public), so a
	// freshly assigned slug can never collide with a stored one.
	takenSlugs, err := store.GetBlogSlugs(ctx, account.ID)
	if err != nil {
		return fmt.Errorf("blog: load slugs: %w", err)
	}
	slugFormat := cfg.Blog.ResolvedSlugFormat(account.BlogSlugFormat)

	posts := make([]sitePost, len(statuses))
	for i, st := range statuses {
		slug := resolveSlug(ctx, store, st, slugFormat, takenSlugs)
		var replies []*blogReply
		var truncated bool
		if account.BlogRepliesEnabled {
			replies, truncated, err = buildReplies(ctx, store, st)
			if err != nil {
				return fmt.Errorf("blog: load replies for %s: %w", st.ID, err)
			}
		}
		posts[i] = buildPost(cfg, root, blogRoot, slug, st, tagsByStatus[st.ID], mediaByStatus[st.ID], replies, truncated, account.BlogRepliesEnabled, account.DefaultPostLanguage)
	}

	tagGroups := map[string]*tagGroup{}
	var tagSlugs []string
	for _, p := range posts {
		for _, t := range p.Tags {
			g, ok := tagGroups[t.URL]
			if !ok {
				g = &tagGroup{Name: t.Name, Slug: tagSlug(t.Name)}
				tagGroups[t.URL] = g
				tagSlugs = append(tagSlugs, t.URL)
			}
			g.Posts = append(g.Posts, p)
		}
	}

	// Posts are already sorted newest-first, so grouping by year while
	// iterating (instead of via a map) keeps each year's posts in order and
	// the years themselves descending, with no extra sort pass needed.
	var archiveYears []archiveYear
	for _, p := range posts {
		year := p.Date.Year()
		if len(archiveYears) == 0 || archiveYears[len(archiveYears)-1].Year != year {
			archiveYears = append(archiveYears, archiveYear{Year: year})
		}
		last := &archiveYears[len(archiveYears)-1]
		last.Posts = append(last.Posts, p)
	}

	tagCounts := make([]tagCount, 0, len(tagGroups))
	for _, g := range tagGroups {
		tagCounts = append(tagCounts, tagCount{Name: g.Name, URL: blogRoot + "/tags/" + g.Slug + "/", Count: len(g.Posts)})
	}
	sort.Slice(tagCounts, func(i, j int) bool { return tagCounts[i].Name < tagCounts[j].Name })

	webDomain := strings.TrimSpace(cfg.WebDomain)
	if webDomain == "" {
		webDomain = cfg.LocalDomain
	}
	avatarOGImage := ""
	if strings.TrimSpace(account.Avatar) != "" {
		avatarOGImage = absoluteBlogURL(webDomain, account.Avatar)
	}

	if err := writeThemeAsset(root, account.BlogTheme, account.BlogCustomCSS); err != nil {
		return err
	}
	if err := writeIndexPage(root, siteTitle, siteDescription, account.Username, blogRoot, account.DefaultPostLanguage, avatarOGImage, posts); err != nil {
		return err
	}
	for i, p := range posts {
		var prev, next *sitePost
		if i > 0 {
			prev = &posts[i-1] // newer post
		}
		if i+1 < len(posts) {
			next = &posts[i+1] // older post
		}
		if err := writePostPage(root, siteTitle, account.Username, blogRoot, postOGImage(webDomain, p, avatarOGImage), p, prev, next); err != nil {
			return err
		}
		if err := writeLegacyRedirect(root, blogRoot, p); err != nil {
			return err
		}
	}
	for _, key := range tagSlugs {
		g := tagGroups[key]
		if err := writeTagPage(root, siteTitle, account.Username, blogRoot, account.DefaultPostLanguage, avatarOGImage, g); err != nil {
			return err
		}
	}
	if err := writeTagsIndexPage(root, siteTitle, account.Username, blogRoot, account.DefaultPostLanguage, avatarOGImage, tagCounts); err != nil {
		return err
	}
	if err := writeArchivePage(root, siteTitle, account.Username, blogRoot, account.DefaultPostLanguage, avatarOGImage, archiveYears); err != nil {
		return err
	}
	if err := writeFeed(root, siteTitle, siteDescription, blogRoot, webDomain, posts); err != nil {
		return err
	}
	if err := writeSitemap(root, blogRoot, webDomain, posts, tagCounts); err != nil {
		return err
	}
	if err := publishGeneratedSite(filepath.Join(outputDir, account.Username), root); err != nil {
		return err
	}
	slog.Info("blog: rebuild complete", "account", account.Username, "posts", len(posts), "tags", len(tagCounts), "duration", time.Since(start).Round(time.Millisecond))
	return nil
}

// publishGeneratedSite replaces the served directory only after the complete
// replacement has been rendered. If the publish rename fails, the previous
// site is restored.
func publishGeneratedSite(dest, staging string) error {
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("blog: make generated site readable: %w", err)
	}
	backup, err := os.MkdirTemp(filepath.Dir(dest), ".blog-old-*")
	if err != nil {
		return fmt.Errorf("blog: create backup path: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		return fmt.Errorf("blog: prepare backup path: %w", err)
	}

	hadPrevious := true
	if err := os.Rename(dest, backup); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("blog: preserve previous site: %w", err)
		}
		hadPrevious = false
	}
	if err := os.Rename(staging, dest); err != nil {
		if hadPrevious {
			if restoreErr := os.Rename(backup, dest); restoreErr != nil {
				return fmt.Errorf("blog: publish generated site: %v; restore previous site: %w", err, restoreErr)
			}
		}
		return fmt.Errorf("blog: publish generated site: %w", err)
	}
	if hadPrevious {
		_ = os.Chmod(backup, 0o755)
		if err := os.RemoveAll(backup); err != nil {
			slog.Warn("blog: failed to remove previous site backup", "path", backup, "error", err)
		}
	}
	return nil
}

// collectBlogStatuses uses the storage query backed by the partial blog index;
// public visibility is a hard floor because generated files have no auth.
func collectBlogStatuses(ctx context.Context, store storage.Storage, accountID string) ([]*models.Status, error) {
	statuses, err := store.GetBlogStatuses(ctx, accountID, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("blog: list statuses: %w", err)
	}
	return statuses, nil
}

// resolveSlug returns the permalink slug of one post, assigning and
// persisting one the first time the post is rendered. A slug already stored
// on the status is always kept: that is what makes a permalink survive an
// edit that rewrites the post's first line (and therefore its derived title).
// A failure to persist is logged, not fatal - the slug is deterministic, so
// the next rebuild recomputes the same one and tries again.
func resolveSlug(ctx context.Context, store storage.Storage, st *models.Status, format string, taken map[string]string) string {
	base := blogslug.Clean(st.BlogSlug)
	if base != "" && taken[base] == st.ID {
		return base
	}
	if base == "" {
		base = blogslug.Build(format, st.CreatedAt, deriveTitle(st.ContentSource, st.CreatedAt), st.ID)
	}
	slug := blogslug.Unique(base, st.ID, taken)
	taken[slug] = st.ID
	if err := store.SetBlogSlug(ctx, st.ID, slug); err != nil {
		slog.Warn("blog: failed to persist post slug", "status_id", st.ID, "slug", slug, "error", err)
	}
	return slug
}

func buildPost(cfg *config.Config, root, blogRoot, slug string, st *models.Status, tags []*models.Tag, media []*models.MediaAttachment, replies []*blogReply, repliesTruncated, showReplies bool, defaultLang string) sitePost {
	lang := st.Language
	if lang == "" {
		lang = defaultLang
	}
	title := deriveTitle(st.ContentSource, st.CreatedAt)
	threadURL := st.URL
	if threadURL == "" {
		threadURL = st.URI
	}

	// The excerpt is the plain-text body with the title line (if it repeats
	// at the start, as it does for e.g. a markdown "# Heading" post) stripped
	// so the index/tag/feed listing doesn't show the title twice in a row.
	plain := content.PlainText(st.ContentHTML)
	excerptSource := strings.TrimSpace(strings.TrimPrefix(plain, title))
	if excerptSource == "" {
		excerptSource = plain
	}
	excerpt := truncateRunes(excerptSource, excerptRunes)

	refs := make([]tagRef, 0, len(tags))
	for _, t := range tags {
		if strings.Trim(strings.TrimSpace(t.Name), "#") == "" {
			continue
		}
		refs = append(refs, tagRef{Name: t.Name, URL: blogRoot + "/tags/" + tagSlug(t.Name) + "/"})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })

	return sitePost{
		ID:               st.ID,
		Slug:             slug,
		Permalink:        blogRoot + "/" + slug + "/",
		Lang:             lang,
		Date:             st.CreatedAt,
		ContentHTML:      template.HTML(rewriteHashtagLinks(stripTitleBlock(content.SanitizeHTMLStr(st.ContentHTML), title), blogRoot)),
		Excerpt:          excerpt,
		Title:            title,
		Tags:             refs,
		Media:            buildMediaItems(cfg, root, blogRoot, slug, media),
		Sensitive:        st.Sensitive,
		ThreadURL:        threadURL,
		Replies:          replies,
		RepliesTruncated: repliesTruncated,
		ShowReplies:      showReplies,
	}
}

// buildMediaItems resolves each attachment to a URL the static site can
// serve. Locally-stored attachments are copied into the post's own output
// directory, so the site stays fully self-contained (surviving even if the
// live instance later disappears, which is what the blog is for).
// S3-backed attachments already have their own durable public URL
// independent of the littleFedi process, so those are linked rather than
// copied; a local file that's missing on disk falls back to linking too,
// rather than failing the whole rebuild over one attachment.
func buildMediaItems(cfg *config.Config, root, blogRoot, slug string, media []*models.MediaAttachment) []mediaItem {
	if len(media) == 0 {
		return nil
	}
	items := make([]mediaItem, 0, len(media))
	for _, m := range media {
		url := m.URL
		// StorageBackend is objectstore.Store.ID(), not a bare backend name -
		// "local:/abs/path" for the local backend, "s3:bucket:prefix:base"
		// for S3 (see internal/media/objectstore/{local,s3}.go). Only skip
		// the local-file copy for an actual S3 row; anything else (including
		// the bun default bare "local" and legacy/empty rows) attempts it,
		// and OwnedLocalPath itself safely no-ops on a URL it can't resolve.
		if !strings.HasPrefix(m.StorageBackend, "s3") && strings.TrimSpace(cfg.Media.Path) != "" {
			if copied, ok := copyPostMedia(cfg.Media.Path, root, blogRoot, slug, m); ok {
				url = copied
			}
		}
		width, height := m.Dimensions()
		items = append(items, mediaItem{Type: m.Type, URL: url, Alt: m.Description, Width: width, Height: height})
	}
	return items
}

// copyPostMedia copies one locally-stored attachment's file into
// <root>/<slug>/media/ and returns the blog-relative URL to serve it from.
// Returns ok=false (caller falls back to m.URL) on anything that stops the
// copy - a missing source file, an unresolvable URL, or a write failure -
// since a warning is preferable to failing the entire site rebuild.
func copyPostMedia(mediaPath, root, blogRoot, slug string, m *models.MediaAttachment) (string, bool) {
	src, ok := medialib.OwnedLocalPath(mediaPath, m.URL)
	if !ok {
		return "", false
	}
	source, err := os.Open(src)
	if err != nil {
		slog.Warn("blog: media file unreadable, linking original URL instead", "media_id", m.ID, "path", src, "error", err)
		return "", false
	}
	defer source.Close()
	filename := m.ID + filepath.Ext(src)
	destDir := filepath.Join(root, filepath.FromSlash(slug), "media")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		slog.Warn("blog: failed to create media directory", "media_id", m.ID, "error", err)
		return "", false
	}
	dest := filepath.Join(destDir, filename)
	if err := os.Link(src, dest); err == nil {
		return blogRoot + "/" + slug + "/media/" + filename, true
	}
	target, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		_, err = io.Copy(target, source)
		if closeErr := target.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = os.Remove(dest)
		slog.Warn("blog: failed to copy media file", "media_id", m.ID, "error", err)
		return "", false
	}
	return blogRoot + "/" + slug + "/media/" + filename, true
}

// deriveTitle uses the post's first non-empty source line as its title
// (stripping a leading markdown heading marker, if any), since posts have no
// separate title field. Falls back to the post date when the source is
// empty or was media-only.
func deriveTitle(source string, createdAt time.Time) string {
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return truncateRunes(line, 100)
		}
	}
	return "Post from " + createdAt.Format("2006-01-02")
}

// leadingBlockPattern matches the first heading or paragraph of a rendered
// post body, so buildPost can tell whether it merely repeats the derived
// title.
var leadingBlockPattern = regexp.MustCompile(`(?is)^\s*<(?:h[1-6]|p)\b[^>]*>(.*?)</(?:h[1-6]|p)>\s*`)

// stripTitleBlock drops the post body's opening block when it is the same
// text the page already shows as the title. deriveTitle takes the first
// source line, so a post written as "# Heading" (or with a bare first line)
// otherwise renders that line twice: once as the <h1>, once at the top of
// the body.
func stripTitleBlock(contentHTML, title string) string {
	m := leadingBlockPattern.FindStringSubmatch(contentHTML)
	if m == nil {
		return contentHTML
	}
	if truncateRunes(strings.TrimSpace(content.PlainText(m[1])), 100) != title {
		return contentHTML
	}
	rest := contentHTML[len(m[0]):]
	// A one-line post is all title: keep the body rather than render a page
	// with nothing under the heading.
	if strings.TrimSpace(content.PlainText(rest)) == "" {
		return contentHTML
	}
	return rest
}

// hashtagHrefPattern matches the href of an inline hashtag link as rendered
// into Status.ContentHTML, e.g. href="/tags/golang" or, after
// content.ResolveSocialLinks absolute-ifies it for federation,
// href="https://example.test/tags/golang".
var hashtagHrefPattern = regexp.MustCompile(`href="[^"]*/tags/([^"/?#]+)/?"`)

// rewriteHashtagLinks points inline #hashtag links at the blog's own tag
// archive instead of the live instance's /tags/<name> page, so a static-site
// reader browsing tags never gets pulled back into the fediverse app.
func rewriteHashtagLinks(html, blogRoot string) string {
	return hashtagHrefPattern.ReplaceAllStringFunc(html, func(match string) string {
		sub := hashtagHrefPattern.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		name, err := url.PathUnescape(sub[1])
		if err != nil {
			name = sub[1]
		}
		return `href="` + blogRoot + "/tags/" + tagSlug(name) + `/"`
	})
}

// tagSlug is an injective, filesystem- and URL-safe encoding. Encoding the
// complete lowercased tag avoids collisions such as C++/C# and keeps path
// separators or dot segments from escaping the tag directory.
func tagSlug(name string) string {
	return "t-" + base64.RawURLEncoding.EncodeToString([]byte(strings.ToLower(name)))
}

// absoluteBlogURL turns a site-relative path (as stored for locally-served
// avatars/media) into an absolute URL; an already-absolute URL (S3-backed
// media, unfetched remote avatars) passes through unchanged. og:image needs
// an absolute URL since link-preview scrapers fetch it out of page context.
func absoluteBlogURL(webDomain, pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	return "https://" + webDomain + pathOrURL
}

// postOGImage picks the post's first attached image for its og:image,
// falling back to the account avatar (already absolute) when the post has
// no image media at all.
func postOGImage(webDomain string, p sitePost, fallback string) string {
	for _, m := range p.Media {
		if m.Type == "image" {
			return absoluteBlogURL(webDomain, m.URL)
		}
	}
	return fallback
}

// truncateRunes cuts s to at most n runes, backing up to the previous space
// so words aren't sliced in half, and marks truncation with an ellipsis.
func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	cut := runes[:n]
	if idx := strings.LastIndexAny(string(cut), " \t\n"); idx > 0 {
		cut = []rune(string(cut)[:idx])
	}
	return strings.TrimSpace(string(cut)) + "…"
}

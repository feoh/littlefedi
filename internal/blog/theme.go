package blog

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"os"
	"path/filepath"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/blogtheme"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
)

//go:embed theme/layout.html theme/index.html theme/post.html theme/tag.html theme/tags.html theme/archive.html theme/style.css
var themeFS embed.FS

var pageTemplates = template.Must(template.ParseFS(themeFS,
	"theme/layout.html", "theme/index.html", "theme/post.html", "theme/tag.html", "theme/tags.html", "theme/archive.html"))

type indexPageData struct {
	SiteTitle       string
	SiteDescription string
	Username        string
	BlogRoot        string
	Lang            string
	PageTitle       string
	MetaDescription string
	OGImage         string
	Posts           []sitePost
}

type postPageData struct {
	SiteTitle       string
	Username        string
	BlogRoot        string
	Lang            string
	PageTitle       string
	MetaDescription string
	OGImage         string
	Post            sitePost
	Prev, Next      *sitePost
}

type tagPageData struct {
	SiteTitle       string
	Username        string
	BlogRoot        string
	Lang            string
	PageTitle       string
	MetaDescription string
	OGImage         string
	Tag             string
	Posts           []sitePost
}

type tagsIndexPageData struct {
	SiteTitle       string
	Username        string
	BlogRoot        string
	Lang            string
	PageTitle       string
	MetaDescription string
	OGImage         string
	Tags            []tagCount
}

type archivePageData struct {
	SiteTitle       string
	Username        string
	BlogRoot        string
	Lang            string
	PageTitle       string
	MetaDescription string
	OGImage         string
	Years           []archiveYear
}

// writeThemeAsset renders the site's one stylesheet: the stock sheet, then the
// selected theme's layer, then the author's own CSS. Concatenating instead of
// emitting three files keeps layout.html unchanged and the page at a single
// stylesheet request, and the order is the cascade the author expects - their
// own rules land last and win.
func writeThemeAsset(root, themeID, customCSS string) error {
	base, err := themeFS.ReadFile("theme/style.css")
	if err != nil {
		return err
	}
	var sheet bytes.Buffer
	sheet.Write(base)

	layer, err := blogtheme.CSS(themeID)
	if err != nil {
		return err
	}
	if len(layer) > 0 {
		sheet.WriteString("\n\n/* theme: " + themeID + " */\n")
		sheet.Write(layer)
	}

	// Validated again here rather than trusted from the column: a value stored
	// before a validation change must not reach the published site unchecked.
	if custom, err := content.ValidateCustomCSS(customCSS); err == nil && custom != "" {
		sheet.WriteString("\n\n/* author stylesheet */\n")
		sheet.WriteString(custom)
		sheet.WriteString("\n")
	} else if err != nil {
		slog.Warn("blog: stored author CSS rejected, publishing without it", "error", err)
	}

	return os.WriteFile(filepath.Join(root, "style.css"), sheet.Bytes(), 0o644)
}

// writePage executes the named page template (which wraps itself in the
// shared "header"/"footer" blocks from layout.html) and writes it as
// dir/index.html, so the URL for that directory has no visible file
// extension and directory listing never triggers (every directory the
// generator creates gets an index.html).
func writePage(dir, templateName string, data any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := pageTemplates.ExecuteTemplate(&buf, templateName, data); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.html"), buf.Bytes(), 0o644)
}

func writeIndexPage(root, siteTitle, siteDescription, username, blogRoot, lang, ogImage string, posts []sitePost) error {
	data := indexPageData{
		SiteTitle:       siteTitle,
		SiteDescription: siteDescription,
		Username:        username,
		BlogRoot:        blogRoot,
		Lang:            lang,
		PageTitle:       siteTitle,
		MetaDescription: siteDescription,
		OGImage:         ogImage,
		Posts:           posts,
	}
	return writePage(root, "index", data)
}

func writePostPage(root, siteTitle, username, blogRoot, ogImage string, p sitePost, prev, next *sitePost) error {
	data := postPageData{
		SiteTitle:       siteTitle,
		Username:        username,
		BlogRoot:        blogRoot,
		Lang:            p.Lang,
		PageTitle:       p.Title + " - " + siteTitle,
		MetaDescription: p.Excerpt,
		OGImage:         ogImage,
		Post:            p,
		Prev:            prev,
		Next:            next,
	}
	return writePage(filepath.Join(root, filepath.FromSlash(p.Slug)), "post", data)
}

// legacyRedirectTemplate keeps the pre-slug permalink (/@user/blog/<status
// id>/) working after a post gains a readable slug. It is a plain static
// file, so no reverse-proxy rule is needed, and the canonical link tells
// crawlers which URL is the real one.
var legacyRedirectTemplate = template.Must(template.New("redirect").Parse(
	`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Moved</title>
<link rel="canonical" href="{{.}}">
<meta name="robots" content="noindex">
<meta http-equiv="refresh" content="0; url={{.}}">
</head>
<body><p>This post has moved to <a href="{{.}}">{{.}}</a>.</p></body>
</html>
`))

// writeLegacyRedirect emits that stub for one post, unless the post is still
// served from its status id (a slug of exactly the id, or an unassigned one),
// in which case the redirect would point at itself.
func writeLegacyRedirect(root, blogRoot string, p sitePost) error {
	if p.Slug == p.ID {
		return nil
	}
	dir := filepath.Join(root, p.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := legacyRedirectTemplate.Execute(&buf, blogRoot+"/"+p.Slug+"/"); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.html"), buf.Bytes(), 0o644)
}

func writeTagPage(root, siteTitle, username, blogRoot, lang, ogImage string, g *tagGroup) error {
	data := tagPageData{
		SiteTitle:       siteTitle,
		Username:        username,
		BlogRoot:        blogRoot,
		Lang:            lang,
		PageTitle:       "#" + g.Name + " - " + siteTitle,
		MetaDescription: "Posts tagged #" + g.Name + " on " + siteTitle,
		OGImage:         ogImage,
		Tag:             g.Name,
		Posts:           g.Posts,
	}
	return writePage(filepath.Join(root, "tags", g.Slug), "tag", data)
}

func writeTagsIndexPage(root, siteTitle, username, blogRoot, lang, ogImage string, tags []tagCount) error {
	data := tagsIndexPageData{
		SiteTitle:       siteTitle,
		Username:        username,
		BlogRoot:        blogRoot,
		Lang:            lang,
		PageTitle:       "Tags - " + siteTitle,
		MetaDescription: "All tags on " + siteTitle,
		OGImage:         ogImage,
		Tags:            tags,
	}
	return writePage(filepath.Join(root, "tags"), "tags", data)
}

func writeArchivePage(root, siteTitle, username, blogRoot, lang, ogImage string, years []archiveYear) error {
	data := archivePageData{
		SiteTitle:       siteTitle,
		Username:        username,
		BlogRoot:        blogRoot,
		Lang:            lang,
		PageTitle:       "Archive - " + siteTitle,
		MetaDescription: "All posts on " + siteTitle + ", by year",
		OGImage:         ogImage,
		Years:           years,
	}
	return writePage(filepath.Join(root, "archive"), "archive", data)
}

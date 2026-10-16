package blog

import (
	"encoding/xml"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
	Type string `xml:"type,attr,omitempty"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",cdata"`
}

type atomEntry struct {
	Title     string      `xml:"title"`
	ID        string      `xml:"id"`
	Link      atomLink    `xml:"link"`
	Updated   string      `xml:"updated"`
	Published string      `xml:"published"`
	Content   atomContent `xml:"content"`
}

type atomFeed struct {
	XMLName  xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Title    string      `xml:"title"`
	Subtitle string      `xml:"subtitle,omitempty"`
	ID       string      `xml:"id"`
	Updated  string      `xml:"updated"`
	Links    []atomLink  `xml:"link"`
	Entries  []atomEntry `xml:"entry"`
}

type sitemapURL struct {
	Loc string `xml:"loc"`
}

type sitemap struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

// writeFeed builds an Atom feed (RFC 4287) covering every blog post and
// writes it to root/feed.xml. webDomain is used to build the absolute URLs
// Atom requires for feed/entry ids and links.
func writeFeed(root, siteTitle, siteDescription, blogRoot, webDomain string, posts []sitePost) error {
	base := "https://" + webDomain
	feedURL := base + blogRoot + "/feed.xml"
	siteURL := base + blogRoot + "/"

	updated := time.Now().UTC()
	if len(posts) > 0 {
		updated = posts[0].Date.UTC()
	}

	feed := atomFeed{
		Title:    siteTitle,
		Subtitle: siteDescription,
		ID:       siteURL,
		Updated:  updated.Format(time.RFC3339),
		Links: []atomLink{
			{Href: feedURL, Rel: "self", Type: "application/atom+xml"},
			{Href: siteURL, Rel: "alternate", Type: "text/html"},
		},
		Entries: make([]atomEntry, len(posts)),
	}
	for i, p := range posts {
		entryURL := base + p.Permalink
		feed.Entries[i] = atomEntry{
			Title: p.Title,
			// The entry id stays pinned to the status id, never to the
			// permalink: RFC 4287 wants a permanently stable id, and a reader
			// that saw this post before slugs existed (or before an author
			// changed their slug format) must not resurface it as new.
			ID:        base + blogRoot + "/" + p.ID + "/",
			Link:      atomLink{Href: entryURL, Rel: "alternate", Type: "text/html"},
			Updated:   p.Date.UTC().Format(time.RFC3339),
			Published: p.Date.UTC().Format(time.RFC3339),
			Content:   atomContent{Type: "html", Body: feedContentHTML(p, base)},
		}
	}

	out, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return err
	}
	out = append([]byte(xml.Header), out...)
	return os.WriteFile(filepath.Join(root, "feed.xml"), out, 0o644)
}

// writeSitemap lists every canonical HTML page generated for one blog.
func writeSitemap(root, blogRoot, webDomain string, posts []sitePost, tags []tagCount) error {
	base := "https://" + webDomain
	urls := []sitemapURL{
		{Loc: base + blogRoot + "/"},
		{Loc: base + blogRoot + "/archive/"},
		{Loc: base + blogRoot + "/tags/"},
	}
	for _, post := range posts {
		urls = append(urls, sitemapURL{Loc: base + post.Permalink})
	}
	for _, tag := range tags {
		urls = append(urls, sitemapURL{Loc: base + tag.URL})
	}
	out, err := xml.MarshalIndent(sitemap{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9", URLs: urls}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "sitemap.xml"), append([]byte(xml.Header), out...), 0o644)
}

// feedContentHTML adds media markup (image/video/audio) before the post body,
// absolute-URL'd against base since a feed is consumed externally -
// unlike the HTML pages, a relative /@user/blog/... URL means nothing
// outside this site.
func feedContentHTML(p sitePost, base string) string {
	var b strings.Builder
	for _, m := range p.Media {
		src := m.URL
		if strings.HasPrefix(src, "/") {
			src = base + src
		}
		src = html.EscapeString(src)
		switch m.Type {
		case "video":
			fmt.Fprintf(&b, `<p><video controls src="%s"></video></p>`, src)
		case "audio":
			fmt.Fprintf(&b, `<p><audio controls src="%s"></audio></p>`, src)
		default:
			fmt.Fprintf(&b, `<p><img src="%s" alt="%s"></p>`, src, html.EscapeString(m.Alt))
		}
	}
	b.WriteString(string(p.ContentHTML))
	return b.String()
}

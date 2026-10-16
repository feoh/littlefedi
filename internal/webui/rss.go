package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	LastBuildDate string    `xml:"lastBuildDate,omitempty"`
	Items         []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
	Description string  `xml:"description"`
}

type rssGUID struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type rssDocument struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type cachedRSS struct {
	body     []byte
	etag     string
	modified time.Time
}

// RSSFeed serves the account's explicitly enabled, federatable RSS 2.0 feed.
func (h *Handler) RSSFeed(w http.ResponseWriter, r *http.Request) {
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), chi.URLParam(r, "username"), nil)
	if err != nil || account.Domain != nil || !account.RSSFeedEnabled {
		http.NotFound(w, r)
		return
	}
	if cached, ok := h.rssCache.Get(account.ID); ok {
		h.serveRSS(w, r, cached.(cachedRSS))
		return
	}
	statuses, err := h.store.GetRSSStatuses(r.Context(), account.ID)
	if err != nil {
		http.Error(w, "Feed unavailable", http.StatusInternalServerError)
		return
	}
	ids := make([]string, len(statuses))
	for i, status := range statuses {
		ids[i] = status.ID
	}
	media, err := h.store.GetMediaForStatuses(r.Context(), ids)
	if err != nil {
		http.Error(w, "Feed unavailable", http.StatusInternalServerError)
		return
	}

	base := "https://" + h.domain
	profileURL := base + "/@" + url.PathEscape(account.Username)
	title := strings.TrimSpace(account.DisplayName)
	if title == "" {
		title = "@" + account.Username
	}
	doc := rssDocument{Version: "2.0", Channel: rssChannel{
		Title: title, Link: profileURL, Description: content.PlainText(account.Note),
		Items: make([]rssItem, len(statuses)),
	}}
	modified := account.UpdatedAt.UTC()
	for i, status := range statuses {
		permalink := absoluteRSSURL(base, status.URL)
		if permalink == base {
			permalink = profileURL + "/" + url.PathEscape(status.ID)
		}
		plain := strings.TrimSpace(content.PlainText(status.ContentHTML))
		itemTitle := truncateText(plain, 100)
		if itemTitle == "" {
			itemTitle = "Post by @" + account.Username
		}
		doc.Channel.Items[i] = rssItem{
			Title: itemTitle, Link: permalink,
			GUID:        rssGUID{IsPermaLink: false, Value: status.URI},
			PubDate:     status.CreatedAt.UTC().Format(time.RFC1123Z),
			Description: rssStatusHTML(status, media[status.ID], base),
		}
		if status.UpdatedAt.After(modified) {
			modified = status.UpdatedAt.UTC()
		}
		for _, attachment := range media[status.ID] {
			if attachment.UpdatedAt.After(modified) {
				modified = attachment.UpdatedAt.UTC()
			}
		}
	}
	if !modified.IsZero() {
		doc.Channel.LastBuildDate = modified.Format(time.RFC1123Z)
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		http.Error(w, "Feed unavailable", http.StatusInternalServerError)
		return
	}
	body = append([]byte(xml.Header), body...)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	cached := cachedRSS{body: body, etag: etag, modified: modified}
	h.rssCache.Set(account.ID, cached, len(body)+128, 5*time.Minute)
	h.serveRSS(w, r, cached)
}

func (h *Handler) serveRSS(w http.ResponseWriter, r *http.Request, cached cachedRSS) {
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", cached.etag)
	if !cached.modified.IsZero() {
		w.Header().Set("Last-Modified", cached.modified.Format(http.TimeFormat))
	}
	if rssNotModified(r, cached.etag, cached.modified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(cached.body)
}

func absoluteRSSURL(base, value string) string {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || value == "" {
		return base
	}
	if u.IsAbs() {
		return u.String()
	}
	return base + "/" + strings.TrimLeft(u.String(), "/")
}

func rssStatusHTML(status *models.Status, attachments []*models.MediaAttachment, base string) string {
	var b strings.Builder
	if status.Summary != "" {
		b.WriteString(`<p><strong>Content warning:</strong> `)
		b.WriteString(html.EscapeString(status.Summary))
		b.WriteString(`</p>`)
	}
	b.WriteString(content.SanitizeHTMLStr(status.ContentHTML))
	for _, attachment := range attachments {
		label := strings.TrimSpace(attachment.Description)
		if label == "" {
			label = attachment.Type + " attachment"
		}
		b.WriteString(`<p><a href="`)
		b.WriteString(html.EscapeString(absoluteRSSURL(base, attachment.URL)))
		b.WriteString(`">Attachment: `)
		b.WriteString(html.EscapeString(label))
		b.WriteString(`</a></p>`)
	}
	return b.String()
}

func rssNotModified(r *http.Request, etag string, modified time.Time) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, candidate := range strings.Split(inm, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
				return true
			}
		}
		return false
	}
	if ims, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !modified.IsZero() {
		return !modified.Truncate(time.Second).After(ims)
	}
	return false
}

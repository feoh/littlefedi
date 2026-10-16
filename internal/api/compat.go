package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

type mastodonParams map[string][]string

func decodeMastodonParams(r *http.Request, maxMemory int64) (mastodonParams, string, error) {
	if maxMemory <= 0 {
		maxMemory = 1 << 20
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "/json") || strings.Contains(ct, "+json") {
		r.Body = http.MaxBytesReader(nil, r.Body, maxMemory)
		var body map[string]interface{}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil {
			return nil, "json", err
		}
		var extra interface{}
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("multiple JSON values")
			}
			return nil, "json", err
		}
		return flattenJSONParams(body), "json", nil
	}

	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, maxMemory+(1<<20))
		if err := r.ParseMultipartForm(maxMemory); err != nil {
			return nil, "multipart", err
		}
		return mastodonParams(r.Form), "multipart", nil
	}

	r.Body = http.MaxBytesReader(nil, r.Body, maxMemory)
	if err := r.ParseForm(); err != nil {
		return nil, "form", err
	}
	return mastodonParams(r.Form), "form", nil
}

func flattenJSONParams(body map[string]interface{}) mastodonParams {
	out := mastodonParams{}
	for k, v := range body {
		addJSONParam(out, k, v)
	}
	return out
}

func addJSONParam(out mastodonParams, key string, value interface{}) {
	switch v := value.(type) {
	case nil:
		return
	case string:
		out[key] = append(out[key], v)
	case bool:
		out[key] = append(out[key], strconv.FormatBool(v))
	case float64:
		out[key] = append(out[key], strconv.FormatFloat(v, 'f', -1, 64))
	case []interface{}:
		if len(v) == 0 {
			out[key] = []string{}
		}
		for _, item := range v {
			addJSONParam(out, key, item)
		}
	case map[string]interface{}:
		for childKey, childValue := range v {
			addJSONParam(out, key+"."+childKey, childValue)
		}
	default:
		out[key] = append(out[key], fmt.Sprint(v))
	}
}

func (p mastodonParams) First(keys ...string) string {
	for _, key := range keys {
		if values := p[key]; len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func (p mastodonParams) All(keys ...string) []string {
	var out []string
	for _, key := range keys {
		out = append(out, p[key]...)
	}
	return out
}

func parseAPIBool(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	return v == "true" || v == "1" || v == "yes" || v == "on"
}

func mastodonVisibility(v models.Visibility) string {
	switch v {
	case models.VisibilityPublic, models.VisibilityLocal:
		return "public"
	case models.VisibilityUnlisted, models.VisibilityLocalUnlisted:
		return "unlisted"
	case models.VisibilityPrivate:
		return "private"
	case models.VisibilityDirect:
		return "direct"
	default:
		return "public"
	}
}

func (h *Handler) absoluteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || strings.ContainsAny(raw, "\\\r\n") || strings.HasPrefix(raw, "//") {
		return ""
	}
	if u.IsAbs() {
		if u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return ""
		}
		return u.String()
	}
	if u.Host != "" {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return "https://" + h.domain + raw
}

func (h *Handler) mediaAttachmentToMastodon(m models.MediaAttachment) MastodonAttachment {
	// Mastodon reports missing alt text as null, and some clients show an
	// empty ALT badge when they get "" instead.
	var desc *string
	if m.Description != "" {
		description := m.Description
		desc = &description
	}
	previewURL := h.displayMediaURL(m.PreviewURL)
	if previewURL == "" && (m.Type == "image" || m.Type == "audio") {
		previewURL = h.displayMediaURL(m.URL)
	}
	var preview *string
	if previewURL != "" {
		preview = &previewURL
	}
	// RemoteURL is informational (Mastodon clients display `url`); keep it as the
	// original remote location even when proxying so debugging/attribution works.
	var remoteURLPtr *string
	if m.RemoteURL != "" {
		remoteURL := m.RemoteURL
		remoteURLPtr = &remoteURL
	}
	return MastodonAttachment{
		ID:          m.ID,
		Type:        m.Type,
		URL:         h.displayMediaURL(m.URL),
		PreviewURL:  preview,
		RemoteURL:   remoteURLPtr,
		Blurhash:    m.Blurhash,
		Description: desc,
		Meta:        mediaMetaJSON(m.Meta),
	}
}

// mediaMetaJSON decodes the JSON blob stored in MediaAttachment.Meta (e.g.
// `{"width":W,"height":H,"focus":{"x":X,"y":Y}}`) for the API response.
// Empty or malformed metadata is omitted rather than surfaced as an error,
// since it is optional display information for clients.
func mediaMetaJSON(meta string) interface{} {
	if meta == "" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(meta), &v); err != nil {
		return nil
	}
	return v
}

// mergeMediaFocus merges a Mastodon-style "x,y" focus parameter (each in
// [-1, 1]) into the existing Meta JSON without discarding width/height set
// by image processing. An empty or invalid focus value leaves meta untouched.
func mergeMediaFocus(meta, focus string) string {
	if focus == "" {
		return meta
	}
	parts := strings.SplitN(focus, ",", 2)
	if len(parts) != 2 {
		return meta
	}
	x, errX := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	y, errY := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errX != nil || errY != nil || x < -1 || x > 1 || y < -1 || y > 1 {
		return meta
	}

	fields := map[string]interface{}{}
	if meta != "" {
		_ = json.Unmarshal([]byte(meta), &fields)
	}
	fields["focus"] = map[string]float64{"x": x, "y": y}

	out, err := json.Marshal(fields)
	if err != nil {
		return meta
	}
	return string(out)
}

// emojisToMastodon converts stored custom emojis into the API shape, rewriting
// each image URL through the privacy proxy / on-disk cache like other media.
func (h *Handler) emojisToMastodon(emojis []models.CustomEmoji) []MastodonEmoji {
	out := make([]MastodonEmoji, 0, len(emojis))
	for _, e := range emojis {
		static := e.StaticURL
		if static == "" {
			static = e.URL
		}
		out = append(out, MastodonEmoji{
			Shortcode:       e.Shortcode,
			URL:             h.displayMediaURL(e.URL),
			StaticURL:       h.displayMediaURL(static),
			VisibleInPicker: e.VisibleInPicker,
		})
	}
	return out
}

// displayMediaURL turns a stored media URL into the URL clients should load.
// Local/relative URLs become absolute on our domain; absolute remote URLs are
// rewritten to the signed media proxy when proxying is enabled (so local users
// never connect to the remote instance) and returned unchanged otherwise.
func (h *Handler) displayMediaURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if h.mediaProxy != nil {
		if proxied := h.mediaProxy.URL(raw); proxied != raw {
			return proxied
		}
	}
	return h.absoluteURL(raw)
}

func logAPIValidationError(r *http.Request, status int, description string, attrs ...interface{}) {
	allAttrs := []interface{}{
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"content_type", r.Header.Get("Content-Type"),
		"scopes", getAuthScopes(r.Context()),
		"error", description,
	}
	allAttrs = append(allAttrs, attrs...)
	slog.Warn("api: request failed", allAttrs...)
}

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/instancemeta"
	medialib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/media"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// Instance Endpoints

// InstanceV1 returns public instance information.
// GET /api/v1/instance
func (h *Handler) InstanceV1(w http.ResponseWriter, r *http.Request) {
	userCount, _ := h.store.CountLocalAccounts(r.Context())
	statusCount, _ := h.store.CountStatuses(r.Context())
	domainCount, _ := h.store.CountDomains(r.Context())
	// Get contact account
	contactAccount := h.getContactAccount(r.Context())
	meta := h.instanceMeta(r.Context())

	info := &MastodonInstanceV1{
		URI:              h.localDomain,
		Title:            h.instanceName(),
		ShortDescription: meta.ShortDescription,
		Description:      meta.Description,
		Email:            meta.ContactEmail,
		Version:          "4.4.0", // Mastodon API compatibility version
		URLs: map[string]string{
			"streaming_api": "wss://" + h.domain + "/api/v1/streaming",
		},
		Stats: map[string]int{
			"user_count":   userCount,
			"status_count": statusCount,
			"domain_count": domainCount,
		},
		Languages:        []string{"en"},
		Registrations:    h.Registrations,
		ApprovalRequired: h.RequireApproval,
		InvitesEnabled:   h.InviteRequired,
		ContactAccount:   contactAccount,
		Rules:            rulesToMastodon(meta.Rules),
		Configuration:    h.instanceConfiguration(),
		MaxTootChars:     h.maxStatusChars(),
	}

	writeAPIJSON(w, http.StatusOK, info)
}

// maxStatusChars returns the configured maximum status length advertised to
// clients, falling back to the Mastodon default of 5000 when unset.
func (h *Handler) maxStatusChars() int {
	if h.config.MaxStatusChars > 0 {
		return h.config.MaxStatusChars
	}
	return 5000
}

func (h *Handler) instanceName() string {
	if name := strings.TrimSpace(h.config.InstanceName); name != "" {
		return name
	}
	return "littleFedi"
}

// MastodonRule is the common rule shape returned by both instance APIs.
type MastodonRule struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// instanceMeta resolves the instance profile for this request: the admin-saved
// values where they exist, the configured ones otherwise.
func (h *Handler) instanceMeta(ctx context.Context) instancemeta.Meta {
	return instancemeta.Load(ctx, h.store, instancemeta.Meta{
		ShortDescription:    h.config.InstanceShortDescription,
		Description:         h.config.InstanceDescription,
		ExtendedDescription: h.config.InstanceExtendedDescription,
		TermsOfService:      h.config.InstanceTermsOfService,
		ContactEmail:        h.config.InstanceContactEmail,
		Rules:               h.config.InstanceRules,
	})
}

// rulesToMastodon numbers the rules by their position in the published list,
// not in the input. A blank line in the configured rules would otherwise leave
// a hole in the IDs, and a report filed against "rule 2" could cite a rule no
// client can show.
func rulesToMastodon(rules []string) []MastodonRule {
	out := make([]MastodonRule, 0, len(rules))
	for _, rule := range rules {
		if rule = strings.TrimSpace(rule); rule != "" {
			out = append(out, MastodonRule{ID: strconv.Itoa(len(out) + 1), Text: rule})
		}
	}
	return out
}

func (h *Handler) maxMedia() int {
	if h.config.MaxMedia > 0 {
		return h.config.MaxMedia
	}
	return 4
}

// instanceConfiguration returns the shared "configuration" object advertised by
// both the v1 and v2 instance endpoints.
func (h *Handler) instanceConfiguration() map[string]interface{} {
	mediaSizeLimit := h.config.MaxMediaSize
	if mediaSizeLimit <= 0 {
		mediaSizeLimit = 40 << 20
	}
	return map[string]interface{}{
		"accounts": map[string]interface{}{
			"max_featured_tags": maxFeaturedTags,
		},
		"statuses": map[string]interface{}{
			"max_characters":              h.maxStatusChars(),
			"max_media_attachments":       h.maxMedia(),
			"characters_reserved_per_url": 23,
		},
		"media_attachments": map[string]interface{}{
			"supported_mime_types": []string{"image/jpeg", "image/png", "image/gif", "image/webp", "video/mp4", "video/webm", "audio/mpeg", "audio/ogg", "audio/wav"},
			"image_size_limit":     mediaSizeLimit,
			"video_size_limit":     mediaSizeLimit,
			"image_matrix_limit":   4096,
			"description_limit":    models.MaxMediaDescriptionLength,
		},
		"polls": map[string]interface{}{
			"max_options":               4,
			"max_characters_per_option": 50,
			"min_expiration":            300,
			"max_expiration":            2629746,
		},
	}
}

// instanceConfigurationV2 adds the urls block that only /api/v2/instance
// carries: v1 advertises the streaming endpoint under its own top-level "urls",
// and a client reading only v2 would otherwise never find it.
func (h *Handler) instanceConfigurationV2(ctx context.Context) map[string]interface{} {
	configuration := h.instanceConfiguration()
	mode, _ := h.store.FederationPolicyMode(ctx)
	configuration["limited_federation"] = mode == "allowlist"
	configuration["urls"] = map[string]interface{}{
		"streaming": "wss://" + h.domain + "/api/v1/streaming",
	}
	// Web clients such as Elk and Phanpy look here for the key to subscribe
	// a browser to push notifications.
	configuration["vapid"] = map[string]interface{}{
		"public_key": h.vapidPublicKey,
	}
	return configuration
}

// InstanceV2 returns extended instance information.
// GET /api/v2/instance
func (h *Handler) InstanceV2(w http.ResponseWriter, r *http.Request) {
	userCount, _ := h.store.CountLocalAccounts(r.Context())

	contactAccount := h.getContactAccount(r.Context())
	meta := h.instanceMeta(r.Context())

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"domain":      h.localDomain,
		"title":       h.instanceName(),
		"version":     "4.4.0",
		"source_url":  "https://brew.bsd.cafe/littlecomputerpeople/littlefedi",
		"description": meta.ShortDescription,
		"usage": map[string]interface{}{
			"users": map[string]interface{}{
				"active_month": userCount,
			},
		},
		"thumbnail": map[string]interface{}{
			"url": "https://" + h.domain + "/headers/original/missing.png",
		},
		"languages": []string{"en"},
		// Clients read api_versions to decide which endpoints exist; without it
		// they fall back to probing or to behaviour older than the advertised
		// version string.
		"api_versions":  map[string]interface{}{"mastodon": 2},
		"configuration": h.instanceConfigurationV2(r.Context()),
		"registrations": map[string]interface{}{
			"enabled":           h.Registrations,
			"approval_required": h.RequireApproval,
			"message":           nil,
		},
		"contact": map[string]interface{}{
			"email":   meta.ContactEmail,
			"account": contactAccount,
		},
		"rules": rulesToMastodon(meta.Rules),
	})
}

// InstanceLanguages lists the languages a post can be tagged with. Clients call
// it while setting an account up and treat a 404 as a broken server.
// GET /api/v1/instance/languages
func (h *Handler) InstanceLanguages(w http.ResponseWriter, r *http.Request) {
	writeAPIJSON(w, http.StatusOK, []map[string]interface{}{
		{"code": "en", "name": "English"},
	})
}

// InstanceTranslationLanguages reports which translation pairs the server
// offers. littleFedi runs no translation backend, so the map is empty - which
// is how a client learns to hide the "translate" action instead of retrying.
// GET /api/v1/instance/translation_languages
func (h *Handler) InstanceTranslationLanguages(w http.ResponseWriter, r *http.Request) {
	writeAPIJSON(w, http.StatusOK, map[string][]string{})
}

// InstanceRules returns the instance rules on their own.
// GET /api/v1/instance/rules
func (h *Handler) InstanceRules(w http.ResponseWriter, r *http.Request) {
	writeAPIJSON(w, http.StatusOK, rulesToMastodon(h.instanceMeta(r.Context()).Rules))
}

// InstanceExtendedDescription returns the long-form "about this server" text.
// GET /api/v1/instance/extended_description
func (h *Handler) InstanceExtendedDescription(w http.ResponseWriter, r *http.Request) {
	meta := h.instanceMeta(r.Context())
	body := map[string]interface{}{
		"updated_at": nil,
		"content":    renderInstanceText(meta.ExtendedDescription),
	}
	if !meta.UpdatedAt.IsZero() {
		body["updated_at"] = meta.UpdatedAt.UTC().Format(time.RFC3339)
	}
	writeAPIJSON(w, http.StatusOK, body)
}

// InstanceTermsOfService returns the instance terms of service, or 404 when the
// operator has published none - which is how Mastodon clients learn to hide the
// link rather than open an empty page.
// GET /api/v1/instance/terms_of_service
func (h *Handler) InstanceTermsOfService(w http.ResponseWriter, r *http.Request) {
	meta := h.instanceMeta(r.Context())
	if strings.TrimSpace(meta.TermsOfService) == "" {
		writeAPIError(w, http.StatusNotFound, "no terms of service")
		return
	}
	effective := meta.UpdatedAt
	if effective.IsZero() {
		effective = time.Now()
	}
	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"effective_date": effective.UTC().Format("2006-01-02"),
		"effective":      true,
		"content":        renderInstanceText(meta.TermsOfService),
		"succeeded_by":   nil,
	})
}

// renderInstanceText renders admin-authored Markdown to the sanitized HTML the
// instance API and the About page both publish. It is sanitized despite the
// author being an administrator so that one compromised admin session cannot
// plant a script into a document every visitor and every connected client
// loads.
func renderInstanceText(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	return content.SanitizeHTMLStr(content.MarkdownToHTML(src))
}

// getContactAccount returns the MastodonAccount for the instance contact.
func (h *Handler) getContactAccount(ctx context.Context) *MastodonAccount {
	// Try configured contact username first
	if h.ContactUsername != "" {
		account, err := h.store.GetAccountByUsernameDomain(ctx, h.ContactUsername, nil)
		if err == nil {
			return h.accountToMastodon(account, getAuthAccount(ctx), false)
		}
	}

	// Fall back to first admin account
	admins, err := h.store.GetAdmins(ctx)
	if err == nil && len(admins) > 0 {
		return h.accountToMastodon(admins[0], getAuthAccount(ctx), false)
	}

	// Fall back to first local account (legacy single-user mode)
	localAccount, err := h.store.GetLocalAccount(ctx)
	if err == nil {
		return h.accountToMastodon(localAccount, getAuthAccount(ctx), false)
	}

	return nil
}

// InstancePeers returns the list of known peer instances.
// GET /api/v1/instance/peers
func (h *Handler) InstancePeers(w http.ResponseWriter, r *http.Request) {
	instances, err := h.store.ListInstances(r.Context())
	if err != nil {
		writeAPIJSON(w, http.StatusOK, []string{})
		return
	}

	peers := make([]string, 0, len(instances))
	mode, _ := h.store.FederationPolicyMode(r.Context())
	for _, inst := range instances {
		if mode != "allowlist" {
			peers = append(peers, inst.Domain)
			continue
		}
		if allowed, _, err := h.store.FederationDomainAllowed(r.Context(), inst.Domain); err == nil && allowed {
			peers = append(peers, inst.Domain)
		}
	}

	writeAPIJSON(w, http.StatusOK, peers)
}

// InstanceDomainBlocks returns the publicly-listed domain blocks (Mastodon
// "moderated servers" view). Only blocks flagged Public are exposed, and the
// domain is masked when the block is marked obfuscate.
// GET /api/v1/instance/domain_blocks
func (h *Handler) InstanceDomainBlocks(w http.ResponseWriter, r *http.Request) {
	blocks, err := h.store.ListDomainBlocks(r.Context())
	if err != nil {
		writeAPIJSON(w, http.StatusOK, []interface{}{})
		return
	}

	type publicBlock struct {
		Domain   string `json:"domain"`
		Digest   string `json:"digest"`
		Severity string `json:"severity"`
		Comment  string `json:"comment,omitempty"`
	}

	result := make([]publicBlock, 0)
	for _, b := range blocks {
		if !b.Public {
			continue
		}
		domain := b.Domain
		if b.Obfuscate {
			domain = obfuscateDomain(domain)
		}
		result = append(result, publicBlock{
			Domain:   domain,
			Digest:   sha256Hex(b.Domain),
			Severity: b.Severity,
			Comment:  b.Comment,
		})
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// obfuscateDomain masks the middle of each domain label, keeping the first and
// last character, matching Mastodon's partial domain disclosure.
func obfuscateDomain(domain string) string {
	labels := strings.Split(domain, ".")
	for i, label := range labels {
		if len(label) <= 2 {
			labels[i] = strings.Repeat("*", len(label))
			continue
		}
		labels[i] = string(label[0]) + strings.Repeat("*", len(label)-2) + string(label[len(label)-1])
	}
	return strings.Join(labels, ".")
}

// sha256Hex returns the hex-encoded SHA-256 of s, used as the public block digest.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Preferences returns the user's preferences.
// GET /api/v1/preferences
func (h *Handler) Preferences(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	visibility := "public"
	if account.ManuallyApprovesFollowers {
		visibility = "private"
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"posting:default:visibility": visibility,
		"posting:default:sensitive":  false,
		"posting:default:language":   "en",
		"reading:expand:media":       "default",
		"reading:expand:spoilers":    false,
	})
}

// Search performs a combined search over accounts, statuses, and hashtags.
// GET /api/v2/search
//
// Supported query params: q, type (accounts|statuses|hashtags), resolve,
// account_id, offset, limit. When type is omitted all three categories are
// searched. Local results come from the database; when resolve=true (or the
// query is an explicit handle/URL) remote accounts are also fetched.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	searchType := r.URL.Query().Get("type")
	resolve := isTrue(r.URL.Query().Get("resolve"))

	var accountID *string
	if v := r.URL.Query().Get("account_id"); v != "" {
		accountID = &v
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 40 {
		limit = 40
	}
	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n > 0 {
			offset = n
		}
	}

	viewer := getAuthAccount(r.Context())
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}

	accounts := []*MastodonAccount{}
	statuses := []*MastodonStatus{}
	hashtags := []MastodonTag{}

	if q == "" {
		writeAPIJSON(w, http.StatusOK, map[string]interface{}{
			"accounts": accounts, "statuses": statuses, "hashtags": hashtags,
		})
		return
	}

	wantAccounts := searchType == "" || searchType == "accounts"
	wantStatuses := searchType == "" || searchType == "statuses"
	wantHashtags := searchType == "" || searchType == "hashtags"

	isURL := strings.HasPrefix(q, "https://") || strings.HasPrefix(q, "http://")
	handle := strings.TrimPrefix(q, "@")
	looksRemote := isURL || strings.Contains(handle, "@")

	if wantAccounts {
		seen := map[string]bool{}

		// Resolve an explicit handle/URL (or when the client asked us to) so the
		// account is fetched even if we've never seen it. Show it first.
		if offset == 0 && h.federator != nil && (resolve || looksRemote) {
			if acct, err := h.federator.ResolveAccount(r.Context(), handle); err == nil && acct != nil {
				accounts = append(accounts, h.accountToMastodon(acct, viewer, false))
				seen[acct.ID] = true
			} else if err != nil {
				slog.Debug("api: search account resolve failed", "query", handle, "error", err)
			}
		}

		local, err := h.store.SearchAccounts(r.Context(), q, offset, limit)
		if err != nil {
			slog.Error("api: search accounts error", "error", err)
		}
		for _, a := range local {
			if seen[a.ID] {
				continue
			}
			accounts = append(accounts, h.accountToMastodon(a, viewer, false))
			if len(accounts) >= limit {
				break
			}
		}
	}

	if wantStatuses {
		var results []*models.Status

		// A pasted status URL/URI resolves to that exact status, fetching it from
		// its origin when this instance has not seen it yet.
		if isURL && offset == 0 {
			if st, err := h.store.GetStatusByURI(r.Context(), q); err == nil && st != nil {
				results = append(results, st)
			} else if h.resolveStatus != nil {
				if st, err := h.resolveStatus(r.Context(), q); err == nil && st != nil {
					results = append(results, st)
				} else if err != nil {
					slog.Debug("api: search status URL resolve failed", "query", q, "error", err)
				}
			}
		}
		if len(results) == 0 {
			found, err := h.store.SearchStatuses(r.Context(), q, accountID, viewerID, offset, limit)
			if err != nil {
				slog.Error("api: search statuses error", "error", err)
			} else {
				results = found
			}
		}

		results = h.app.VisibleStatuses(r.Context(), results, viewer)
		h.app.EnrichStatuses(r.Context(), results, viewer)
		results, filterErr := h.app.FilterStatuses(r.Context(), "public", results, viewer)
		if filterErr != nil {
			writeAPIError(w, http.StatusInternalServerError, "error filtering search results")
			return
		}
		for _, st := range results {
			statuses = append(statuses, h.statusToMastodon(st, viewer))
		}
	}

	if wantHashtags {
		tags, err := h.store.SearchTags(r.Context(), q, offset, limit)
		if err != nil {
			slog.Error("api: search hashtags error", "error", err)
		}
		for _, t := range tags {
			hashtags = append(hashtags, h.toMastodonTag(t, false, false))
		}
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"accounts": accounts,
		"statuses": statuses,
		"hashtags": hashtags,
	})
}

// isTrue reports whether a query parameter represents a truthy value.
func isTrue(v string) bool {
	return v == "true" || v == "1" || v == "yes"
}

// SearchV1 performs a v1 search.
// GET /api/v1/search
func (h *Handler) SearchV1(w http.ResponseWriter, r *http.Request) {
	h.Search(w, r)
}

// Media Endpoints

// MediaUpload uploads a media attachment.
// POST /api/v1/media
func (h *Handler) MediaUpload(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	// Parse multipart form
	maxSize := h.config.MaxMediaSize
	if maxSize <= 0 {
		maxSize = 40 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+medialib.MaxPreviewSize+(1<<20))
	if err := r.ParseMultipartForm(maxSize); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid multipart form")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "file is required")
		return
	}
	defer file.Close()

	description := strings.TrimSpace(r.FormValue("description"))
	if utf8.RuneCountInString(description) > models.MaxMediaDescriptionLength {
		writeAPIError(w, http.StatusUnprocessableEntity, "description is too long")
		return
	}
	focus := r.FormValue("focus")
	if header.Size > maxSize {
		writeAPIError(w, http.StatusUnprocessableEntity, "file is too large")
		return
	}
	mimeType, mediaReader, err := medialib.SniffUpload(file, header.Header.Get("Content-Type"))
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid media")
		return
	}

	// Create a media attachment record
	media := &models.MediaAttachment{
		AccountID:   account.ID,
		Type:        mediaKind(mimeType),
		URL:         "", // Will be set by media handler
		PreviewURL:  "",
		Description: description,
		FileSize:    header.Size,
		MIMEType:    mimeType,
	}

	if err := h.store.CreateMediaAttachment(r.Context(), media); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error creating media attachment")
		return
	}

	// Use the renamed filename if configured
	safeFilename := medialib.MediaFilename(header.Filename, mimeType, h.config.RenameMedia)

	// Store the actual file
	err = medialib.StoreUpload(r.Context(), h.config.MediaStore, mediaReader, header.Size, media, safeFilename, h.config.StripEXIF)
	if err != nil {
		h.store.DeleteMediaAttachment(r.Context(), media.ID)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid media")
		return
	}
	if thumbnails := r.MultipartForm.File["thumbnail"]; len(thumbnails) > 0 && thumbnails[0].Size > 0 {
		poster, openErr := thumbnails[0].Open()
		if openErr == nil {
			openErr = medialib.StorePreview(r.Context(), h.config.MediaStore, poster, thumbnails[0].Size, media, h.config.StripEXIF)
			poster.Close()
		}
		if openErr != nil {
			_ = h.config.MediaStore.Delete(r.Context(), media.StorageKey)
			_ = h.store.DeleteMediaAttachment(r.Context(), media.ID)
			writeAPIError(w, http.StatusUnprocessableEntity, "invalid thumbnail")
			return
		}
	}
	media.Meta = mergeMediaFocus(media.Meta, focus)
	if err = h.store.UpdateMediaAttachment(r.Context(), media); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error updating media")
		return
	}

	writeAPIJSON(w, http.StatusOK, h.mediaAttachmentToMastodon(*media))
}

// MediaUploadV2 v2 media upload endpoint.
// POST /api/v2/media
func (h *Handler) MediaUploadV2(w http.ResponseWriter, r *http.Request) {
	// Same as v1 for now
	h.MediaUpload(w, r)
}

// MediaUpdate updates a media attachment.
// PUT /api/v1/media/{id}
func (h *Handler) MediaUpdate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	media, err := h.store.GetMediaAttachment(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "media not found")
		return
	}
	if media.AccountID != account.ID {
		writeAPIError(w, http.StatusForbidden, "not your media")
		return
	}
	// Changing an attachment that is already posted would skip the Update
	// activity and the status caches. Mastodon refuses it too; clients are
	// expected to send media_attributes with the status edit instead.
	if media.StatusID != nil && *media.StatusID != "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "media is already attached to a status; edit the status instead")
		return
	}

	if params, _, err := decodeMastodonParams(r, medialib.MaxPreviewSize); err == nil {
		// An empty description clears the alt text, so check for the key
		// rather than for a value.
		if values, ok := params["description"]; ok {
			desc := ""
			if len(values) > 0 {
				desc = strings.TrimSpace(values[0])
			}
			if utf8.RuneCountInString(desc) > models.MaxMediaDescriptionLength {
				writeAPIError(w, http.StatusUnprocessableEntity, "description is too long")
				return
			}
			media.Description = desc
		}
		media.Meta = mergeMediaFocus(media.Meta, params.First("focus"))
		if r.MultipartForm != nil {
			if thumbnails := r.MultipartForm.File["thumbnail"]; len(thumbnails) > 0 && thumbnails[0].Size > 0 {
				poster, openErr := thumbnails[0].Open()
				if openErr == nil {
					openErr = medialib.StorePreview(r.Context(), h.config.MediaStore, poster, thumbnails[0].Size, media, h.config.StripEXIF)
					poster.Close()
				}
				if openErr != nil {
					writeAPIError(w, http.StatusUnprocessableEntity, "invalid thumbnail")
					return
				}
			}
		}
		if err := h.store.UpdateMediaAttachment(r.Context(), media); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "error updating media")
			return
		}
	} else {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request")
		return
	}

	writeAPIJSON(w, http.StatusOK, h.mediaAttachmentToMastodon(*media))
}

// MediaGet returns a media attachment.
// GET /api/v1/media/{id}
func (h *Handler) MediaGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	media, err := h.store.GetMediaAttachment(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "media not found")
		return
	}
	account := getAuthAccount(r.Context())
	if media.StatusID == nil || *media.StatusID == "" {
		if account == nil || media.AccountID != account.ID {
			writeAPIError(w, http.StatusNotFound, "media not found")
			return
		}
	} else {
		status, statusErr := h.store.GetStatus(r.Context(), *media.StatusID)
		if statusErr != nil || !h.app.CanViewStatus(r.Context(), status, account) {
			writeAPIError(w, http.StatusNotFound, "media not found")
			return
		}
	}

	writeAPIJSON(w, http.StatusOK, h.mediaAttachmentToMastodon(*media))
}

func mediaKind(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "unknown"
	}
}

// Markers

// markerToAPI renders one marker in the Mastodon shape.
func markerToAPI(m *models.Marker) map[string]interface{} {
	return map[string]interface{}{
		"last_read_id": m.LastReadID,
		"version":      m.Version,
		"updated_at":   m.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// MarkersGet returns timeline markers. Apps compute the unread badge from
// these; before persistence existed every poll made everything look unread.
// GET /api/v1/markers?timeline[]=home&timeline[]=notifications
func (h *Handler) MarkersGet(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	timelines := r.URL.Query()["timeline[]"]
	if len(timelines) == 0 {
		timelines = r.URL.Query()["timeline"]
	}
	if len(timelines) == 0 {
		// Default to the two Mastodon timelines rather than every stored
		// marker: the web UI keeps its catch-up position in this table too, and
		// a client that asked for nothing should not receive it.
		timelines = []string{"home", "notifications"}
	}
	markers, err := h.store.GetMarkers(r.Context(), account.ID, timelines)
	if err != nil {
		slog.Error("api: markers get error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching markers")
		return
	}
	out := map[string]interface{}{}
	for _, m := range markers {
		out[m.Timeline] = markerToAPI(m)
	}
	writeAPIJSON(w, http.StatusOK, out)
}

// MarkersSave saves timeline markers.
// POST /api/v1/markers with home[last_read_id] / notifications[last_read_id]
// (form) or nested JSON.
func (h *Handler) MarkersSave(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusBadRequest, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	out := map[string]interface{}{}
	for _, timeline := range []string{"home", "notifications"} {
		lastRead := params.First(timeline+"[last_read_id]", timeline+".last_read_id")
		if lastRead == "" {
			continue
		}
		m, err := h.store.SetMarker(r.Context(), account.ID, timeline, lastRead)
		if err != nil {
			slog.Error("api: marker save error", "timeline", timeline, "error", err)
			writeAPIError(w, http.StatusInternalServerError, "error saving marker")
			return
		}
		out[timeline] = markerToAPI(m)
	}
	writeAPIJSON(w, http.StatusOK, out)
}

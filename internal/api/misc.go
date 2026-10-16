package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/statusfilter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (h *Handler) PollGet(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	viewer := ""
	if a != nil {
		viewer = a.ID
	}
	p, err := h.store.GetPoll(r.Context(), chi.URLParam(r, "id"), viewer)
	if err != nil {
		writeAPIError(w, 404, "poll not found")
		return
	}
	status, err := h.store.GetStatus(r.Context(), p.StatusID)
	if err != nil || !h.app.CanViewStatus(r.Context(), status, a) {
		writeAPIError(w, 404, "poll not found")
		return
	}
	writeAPIJSON(w, 200, p)
}
func (h *Handler) PollVote(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	poll, err := h.store.GetPoll(r.Context(), chi.URLParam(r, "id"), a.ID)
	if err != nil {
		writeAPIError(w, 404, "poll not found")
		return
	}
	status, err := h.store.GetStatus(r.Context(), poll.StatusID)
	if err != nil || !h.app.CanViewStatus(r.Context(), status, a) {
		writeAPIError(w, 404, "poll not found")
		return
	}
	params, _, err := decodeMastodonParams(r, 1<<20)
	if err != nil {
		writeAPIError(w, 422, "invalid request")
		return
	}
	raw := params.All("choices[]", "choices")
	choices := make([]int, 0, len(raw))
	for _, v := range raw {
		n, e := strconv.Atoi(v)
		if e != nil {
			writeAPIError(w, 422, "invalid choice")
			return
		}
		choices = append(choices, n)
	}
	p, err := h.app.VotePoll(r.Context(), a, chi.URLParam(r, "id"), choices)
	if err != nil {
		writeAPIError(w, 422, err.Error())
		return
	}
	writeAPIJSON(w, 200, p)
}

type filterRequest struct {
	Title                string    `json:"title"`
	Context              []string  `json:"context"`
	FilterAction         string    `json:"filter_action"`
	ExpiresIn            *int      `json:"expires_in"`
	Languages            *[]string `json:"languages"`
	InvertLanguages      *bool     `json:"invert_languages"`
	ShowUnknownLanguages *bool     `json:"show_unknown_languages"`
	Keywords             []struct {
		Keyword   string `json:"keyword"`
		WholeWord bool   `json:"whole_word"`
	} `json:"keywords"`
}

func decodeFilter(r *http.Request) (filterRequest, error) {
	var v filterRequest
	r.Body = http.MaxBytesReader(nil, r.Body, 2<<20)
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v)
		return v, err
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		_ = r.ParseForm()
	}
	v.Title = r.FormValue("title")
	v.Context = append(v.Context, r.Form["context[]"]...)
	v.FilterAction = r.FormValue("filter_action")
	if values, ok := r.Form["languages[]"]; ok {
		v.Languages = &values
	} else if values, ok := r.Form["languages"]; ok {
		v.Languages = &values
	}
	if raw, ok := r.Form["invert_languages"]; ok && len(raw) > 0 {
		value := parseAPIBool(raw[0])
		v.InvertLanguages = &value
	}
	if raw, ok := r.Form["show_unknown_languages"]; ok && len(raw) > 0 {
		value := parseAPIBool(raw[0])
		v.ShowUnknownLanguages = &value
	}
	if raw := r.FormValue("expires_in"); raw != "" {
		if n, e := strconv.Atoi(raw); e == nil {
			v.ExpiresIn = &n
		}
	}
	indexes := map[string]int{}
	keyPattern := regexp.MustCompile(`^keywords_attributes\[([^]]+)\]\[(keyword|whole_word)\]$`)
	for key, values := range r.Form {
		m := keyPattern.FindStringSubmatch(key)
		if len(m) != 3 || len(values) == 0 {
			continue
		}
		idx, ok := indexes[m[1]]
		if !ok {
			idx = len(v.Keywords)
			indexes[m[1]] = idx
			v.Keywords = append(v.Keywords, struct {
				Keyword   string `json:"keyword"`
				WholeWord bool   `json:"whole_word"`
			}{})
		}
		if m[2] == "keyword" {
			v.Keywords[idx].Keyword = values[0]
		} else {
			v.Keywords[idx].WholeWord = parseAPIBool(values[0])
		}
	}
	return v, nil
}
func filterFromRequest(a, id string, v filterRequest) *models.Filter {
	f := &models.Filter{ID: id, AccountID: a, Title: strings.TrimSpace(v.Title), FilterAction: v.FilterAction, Statuses: []any{}}
	for _, context := range v.Context {
		if validFilterContext(context) && !containsAPIString(f.Context, context) {
			f.Context = append(f.Context, context)
		}
	}
	if f.FilterAction == "" {
		f.FilterAction = "warn"
	}
	if v.ExpiresIn != nil {
		t := time.Now().Add(time.Duration(*v.ExpiresIn) * time.Second)
		f.ExpiresAt = &t
	}
	if v.Languages != nil {
		f.Languages = statusfilter.NormalizeLanguages(*v.Languages)
	}
	if v.InvertLanguages != nil {
		f.InvertLanguages = *v.InvertLanguages
	}
	if v.ShowUnknownLanguages != nil {
		f.ShowUnknownLanguages = *v.ShowUnknownLanguages
	}
	for _, k := range v.Keywords {
		if s := strings.TrimSpace(k.Keyword); s != "" {
			f.Keywords = append(f.Keywords, models.FilterKeyword{Keyword: s, WholeWord: k.WholeWord})
		}
	}
	return f
}

func validFilterRequest(v filterRequest) bool {
	if strings.TrimSpace(v.Title) == "" || (v.FilterAction != "" && v.FilterAction != "warn" && v.FilterAction != "hide") {
		return false
	}
	for _, context := range v.Context {
		if !validFilterContext(context) {
			return false
		}
	}
	return true
}

func validFilterContext(context string) bool {
	switch context {
	case "home", "notifications", "public", "thread", "account":
		return true
	default:
		return false
	}
}

func containsAPIString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func (h *Handler) FiltersList(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	v, e := h.store.ListFilters(r.Context(), a.ID)
	if e != nil {
		writeAPIError(w, 500, "filters failed")
		return
	}
	writeAPIJSON(w, 200, v)
}
func (h *Handler) FilterGet(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	v, e := h.store.GetFilter(r.Context(), chi.URLParam(r, "id"), a.ID)
	if e != nil {
		writeAPIError(w, 404, "filter not found")
		return
	}
	writeAPIJSON(w, 200, v)
}
func (h *Handler) FilterCreate(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	v, e := decodeFilter(r)
	if e != nil || !validFilterRequest(v) {
		writeAPIError(w, 422, "invalid filter")
		return
	}
	f := filterFromRequest(a.ID, uuid.NewString(), v)
	if e = h.store.CreateFilter(r.Context(), f); e != nil {
		writeAPIError(w, 500, "create filter failed")
		return
	}
	h.app.InvalidateFilterPages(a.ID)
	writeAPIJSON(w, 200, f)
}
func (h *Handler) FilterUpdate(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	v, e := decodeFilter(r)
	if e != nil || !validFilterRequest(v) {
		writeAPIError(w, 422, "invalid filter")
		return
	}
	f := filterFromRequest(a.ID, chi.URLParam(r, "id"), v)
	if v.Languages == nil || v.InvertLanguages == nil || v.ShowUnknownLanguages == nil {
		existing, err := h.store.GetFilter(r.Context(), f.ID, a.ID)
		if err != nil {
			writeAPIError(w, 404, "filter not found")
			return
		}
		if v.Languages == nil {
			f.Languages = existing.Languages
		}
		if v.InvertLanguages == nil {
			f.InvertLanguages = existing.InvertLanguages
		}
		if v.ShowUnknownLanguages == nil {
			f.ShowUnknownLanguages = existing.ShowUnknownLanguages
		}
	}
	if e = h.store.UpdateFilter(r.Context(), f); e != nil {
		writeAPIError(w, 404, "filter not found")
		return
	}
	h.app.InvalidateFilterPages(a.ID)
	writeAPIJSON(w, 200, f)
}
func (h *Handler) FilterDelete(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	if h.store.DeleteFilter(r.Context(), chi.URLParam(r, "id"), a.ID) != nil {
		writeAPIError(w, 404, "filter not found")
		return
	}
	h.app.InvalidateFilterPages(a.ID)
	writeAPIJSON(w, 200, map[string]any{})
}

func (h *Handler) Conversations(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	ss, e := h.store.GetDirectTimeline(r.Context(), a.ID, nil, nil, nil, 80)
	if e != nil {
		writeAPIError(w, 500, "conversations failed")
		return
	}
	h.app.EnrichStatuses(r.Context(), ss, a)
	ss, e = h.app.FilterStatuses(r.Context(), "home", ss, a)
	if e != nil {
		writeAPIError(w, 500, "conversations failed")
		return
	}
	accountIDs := map[string]struct{}{}
	for _, s := range ss {
		if s.AccountID != a.ID {
			accountIDs[s.AccountID] = struct{}{}
		}
		for _, m := range s.Mentions {
			if m.AccountID != a.ID {
				accountIDs[m.AccountID] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(accountIDs))
	for id := range accountIDs {
		ids = append(ids, id)
	}
	accounts, _ := h.store.GetAccountsByIDs(r.Context(), ids)
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, s := range ss {
		if s.Visibility != models.VisibilityDirect {
			continue
		}
		id := s.ID
		if s.ConversationID != nil {
			id = *s.ConversationID
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		participants := []*MastodonAccount{}
		used := map[string]bool{}
		for _, candidate := range append([]string{s.AccountID}, mentionAccountIDs(s.Mentions)...) {
			if candidate == a.ID || used[candidate] {
				continue
			}
			if acct := accounts[candidate]; acct != nil {
				participants = append(participants, h.accountToMastodon(acct, a, false))
				used[candidate] = true
			}
		}
		out = append(out, map[string]any{"id": id, "unread": false, "accounts": participants, "last_status": h.statusToMastodon(s, a)})
	}
	writeAPIJSON(w, 200, out)
}
func mentionAccountIDs(ms []models.Mention) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.AccountID)
	}
	return out
}
func (h *Handler) Announcements(w http.ResponseWriter, r *http.Request) {
	v, e := h.store.ListAnnouncements(r.Context(), time.Now())
	if e != nil {
		writeAPIError(w, 500, "announcements failed")
		return
	}
	out := make([]map[string]any, 0, len(v))
	for _, a := range v {
		out = append(out, map[string]any{"id": a.ID, "content": a.Content, "starts_at": a.StartsAt, "ends_at": a.EndsAt, "all_day": a.AllDay, "published_at": a.PublishedAt, "updated_at": a.PublishedAt, "read": false, "mentions": []any{}, "statuses": []any{}, "tags": []any{}, "emojis": []any{}, "reactions": []any{}})
	}
	writeAPIJSON(w, 200, out)
}
func (h *Handler) EmptyArray(w http.ResponseWriter, r *http.Request) { writeAPIJSON(w, 200, []any{}) }

func (h *Handler) CustomEmojis(w http.ResponseWriter, r *http.Request) {
	emojis, err := h.store.ListLocalCustomEmojis(r.Context())
	if err != nil {
		writeAPIError(w, 500, "custom emojis failed")
		return
	}
	vals := make([]models.CustomEmoji, len(emojis))
	for i, e := range emojis {
		vals[i] = *e
	}
	writeAPIJSON(w, 200, h.emojisToMastodon(vals))
}

// scheduledStatusEntity builds a Mastodon-shaped ScheduledStatus entity from
// the internally stored app.StatusInput, mapping to the field names real
// clients expect in "params" rather than dumping Go struct field names.
func (h *Handler) scheduledStatusEntity(v *models.ScheduledStatus) map[string]any {
	var input app.StatusInput
	_ = json.Unmarshal([]byte(v.ParamsJSON), &input)
	var poll map[string]any
	if len(input.PollOptions) > 0 {
		poll = map[string]any{
			"options":     input.PollOptions,
			"multiple":    input.PollMultiple,
			"hide_totals": input.PollHideTotals,
			"expires_in":  int(input.PollExpiresIn.Seconds()),
		}
	}
	var inReplyTo any
	if input.InReplyToID != nil {
		inReplyTo = *input.InReplyToID
	}
	params := map[string]any{
		"text":            input.Content,
		"poll":            poll,
		"media_ids":       input.MediaIDs,
		"sensitive":       input.Sensitive,
		"spoiler_text":    input.Summary,
		"visibility":      string(input.Visibility),
		"in_reply_to_id":  inReplyTo,
		"language":        input.Language,
		"scheduled_at":    nil,
		"idempotency":     nil,
		"with_rate_limit": false,
	}
	return map[string]any{"id": v.ID, "scheduled_at": v.ScheduledAt, "params": params, "media_attachments": []any{}}
}
func (h *Handler) ScheduledStatuses(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	limit := parseIntDefault(r.URL.Query().Get("limit"), 20)
	vs, e := h.store.ListScheduledStatuses(r.Context(), a.ID, limit)
	if e != nil {
		writeAPIError(w, 500, "scheduled statuses failed")
		return
	}
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, h.scheduledStatusEntity(v))
	}
	writeAPIJSON(w, 200, out)
}
func (h *Handler) ScheduledStatusGet(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	v, e := h.store.GetScheduledStatus(r.Context(), chi.URLParam(r, "id"), a.ID)
	if e != nil {
		writeAPIError(w, 404, "scheduled status not found")
		return
	}
	writeAPIJSON(w, 200, h.scheduledStatusEntity(v))
}
func (h *Handler) ScheduledStatusUpdate(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	params, _, err := decodeMastodonParams(r, 1<<20)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}
	raw := params.First("scheduled_at")
	scheduledAt, parseErr := time.Parse(time.RFC3339, raw)
	if raw == "" || parseErr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "scheduled_at must be a valid RFC3339 timestamp")
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.app.RescheduleStatus(r.Context(), a.ID, id, scheduledAt); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	v, e := h.store.GetScheduledStatus(r.Context(), id, a.ID)
	if e != nil {
		writeAPIError(w, 404, "scheduled status not found")
		return
	}
	writeAPIJSON(w, 200, h.scheduledStatusEntity(v))
}
func (h *Handler) ScheduledStatusDelete(w http.ResponseWriter, r *http.Request) {
	a := getAuthAccount(r.Context())
	if e := h.app.CancelScheduledStatus(r.Context(), a.ID, chi.URLParam(r, "id")); e != nil {
		writeAPIError(w, 404, "scheduled status not found")
		return
	}
	writeAPIJSON(w, 200, map[string]any{})
}
func (h *Handler) PublishScheduledStatus(ctx context.Context, payload string) error {
	var ref struct {
		ID        string `json:"id"`
		AccountID string `json:"account_id"`
	}
	if e := json.Unmarshal([]byte(payload), &ref); e != nil {
		return e
	}
	return h.app.PublishScheduledStatus(ctx, ref.ID, ref.AccountID)
}

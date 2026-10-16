// Package api implements the Mastodon-compatible REST API.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/accounts"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Admin Middleware

// AdminMiddleware checks that the authenticated user has admin privileges.
func (h *Handler) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := getAuthAccount(r.Context())
		if account == nil {
			writeAPIError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		if !account.IsAdmin {
			writeAPIError(w, http.StatusForbidden, "admin privileges required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logModeration writes a moderation audit-log entry attributed to the request's
// authenticated admin. Best-effort: failures are logged, not surfaced.
func (h *Handler) logModeration(r *http.Request, targetType, targetID, action, note string) {
	adminID := ""
	if a := getAuthAccount(r.Context()); a != nil {
		adminID = a.ID
	}
	if err := h.store.CreateModerationLog(r.Context(), &models.ModerationLog{
		AdminAccountID: adminID,
		TargetType:     targetType,
		TargetID:       targetID,
		Action:         action,
		Note:           note,
	}); err != nil {
		slog.Warn("api: failed to write moderation log", "error", err)
	}
}

// Admin Account Endpoints

// AdminAccountsList returns a list of local accounts.
// GET /api/v1/admin/accounts
func (h *Handler) AdminAccountsList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status") // local, remote, suspended, pending
	username := r.URL.Query().Get("username")

	var accounts []*models.Account
	var err error

	switch status {
	case "pending":
		accounts, err = h.store.GetPendingApprovals(r.Context())
	case "suspended":
		// Filter locally after fetching all local accounts - simpler than a new query
		allLocal, listErr := h.store.GetLocalAccounts(r.Context())
		if listErr != nil {
			writeAPIError(w, http.StatusInternalServerError, "error fetching accounts")
			return
		}
		for _, a := range allLocal {
			if a.SuspendedAt != nil {
				accounts = append(accounts, a)
			}
		}
	default:
		if username != "" {
			acct, err2 := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
			if err2 != nil {
				writeAPIError(w, http.StatusNotFound, "account not found")
				return
			}
			accounts = []*models.Account{acct}
		} else {
			accounts, err = h.store.GetLocalAccounts(r.Context())
		}
	}

	if err != nil {
		slog.Error("api: admin accounts list error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching accounts")
		return
	}

	result := make([]*MastodonAccount, 0, len(accounts))
	for _, a := range accounts {
		result = append(result, h.accountToMastodon(a, getAuthAccount(r.Context()), false))
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// AdminAccountGet returns details for a single account.
// GET /api/v1/admin/accounts/{id}
func (h *Handler) AdminAccountGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	account, err := h.store.GetAccount(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "account not found")
		return
	}

	result := h.accountToMastodon(account, getAuthAccount(r.Context()), false)
	writeAPIJSON(w, http.StatusOK, result)
}

// AdminAccountAction performs a moderation action on an account.
// POST /api/v1/admin/accounts/{id}/action
func (h *Handler) AdminAccountAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	actionType := r.FormValue("type") // "suspend", "silence", "quiet", "disable", "none"
	note := r.FormValue("text")

	switch actionType {
	case "suspend", "silence", "quiet", "disable", "none":
		// handled below
	default:
		writeAPIError(w, http.StatusBadRequest, "unknown action type")
		return
	}

	account, err := h.store.GetAccount(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "account not found")
		return
	}

	if err := h.app.ModerateAccount(r.Context(), getAuthAccount(r.Context()), account, actionType, note); err != nil {
		slog.Error("api: admin account action error", "action", actionType, "error", err)
		if errors.Is(err, app.ErrModerationRejected) {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "error applying moderation action")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

// AdminAccountApprove approves a pending account.
// POST /api/v1/admin/accounts/{id}/approve
func (h *Handler) AdminAccountApprove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	account, _ := h.store.GetAccount(r.Context(), id)
	if err := h.store.ApproveAccount(r.Context(), id); err != nil {
		slog.Error("api: admin approve error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error approving account")
		return
	}

	if h.mailer != nil && account != nil && account.Email != "" && account.EmailVerified {
		_ = h.mailer.SendApprovalGranted(account.Locale, account.Email, account.Username)
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// AdminAccountReject rejects (deletes) a pending account.
// POST /api/v1/admin/accounts/{id}/reject
func (h *Handler) AdminAccountReject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	account, _ := h.store.GetAccount(r.Context(), id)
	if err := h.store.RejectAccount(r.Context(), id); err != nil {
		slog.Error("api: admin reject error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error rejecting account")
		return
	}

	if h.mailer != nil && account != nil && account.Email != "" && account.EmailVerified {
		_ = h.mailer.SendApprovalRejected(account.Locale, account.Email, account.Username)
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// Admin Reports

// AdminReportsList lists reports, optionally filtered by ?resolved=true/false.
// GET /api/v1/admin/reports
func (h *Handler) AdminReportsList(w http.ResponseWriter, r *http.Request) {
	state := "open"
	switch r.URL.Query().Get("resolved") {
	case "true", "1":
		state = "resolved"
	case "":
		// default to open
	default:
		state = ""
	}
	params := parsePagination(r)
	reports, err := h.store.ListReports(r.Context(), state, params.MaxID, params.Limit)
	if err != nil {
		slog.Error("api: admin reports list error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching reports")
		return
	}
	result := make([]*MastodonReport, 0, len(reports))
	for _, rep := range reports {
		result = append(result, h.reportToMastodon(r, rep))
	}
	writeAPIJSON(w, http.StatusOK, result)
}

// AdminReportGet returns a single report.
// GET /api/v1/admin/reports/{id}
func (h *Handler) AdminReportGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	report, err := h.store.GetReport(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "report not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, h.reportToMastodon(r, report))
}

// AdminReportResolve resolves (closes) a report, optionally applying a follow-on
// moderation action to the reported account via ?action=suspend|silence|quiet|disable.
// POST /api/v1/admin/reports/{id}/resolve
func (h *Handler) AdminReportResolve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	action := r.FormValue("action")
	if err := h.app.ResolveReport(r.Context(), getAuthAccount(r.Context()), id, action); err != nil {
		slog.Error("api: resolve report error", "error", err)
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "report or target account not found")
			return
		}
		if errors.Is(err, app.ErrModerationRejected) {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "error resolving report")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

// Admin Domain Blocks

// AdminDomainBlocksList lists all domain blocks.
// GET /api/v1/admin/domain_blocks
func (h *Handler) AdminDomainBlocksList(w http.ResponseWriter, r *http.Request) {
	blocks, err := h.store.ListDomainBlocks(r.Context())
	if err != nil {
		slog.Error("api: admin domain blocks list error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching domain blocks")
		return
	}

	type AdminDomainBlock struct {
		ID        string `json:"id"`
		Domain    string `json:"domain"`
		Severity  string `json:"severity"`
		Public    bool   `json:"public"`
		Comment   string `json:"comment"`
		CreatedAt string `json:"created_at"`
	}

	result := make([]AdminDomainBlock, 0, len(blocks))
	for _, b := range blocks {
		result = append(result, AdminDomainBlock{
			ID:        b.ID,
			Domain:    b.Domain,
			Severity:  b.Severity,
			Public:    b.Public,
			Comment:   b.Comment,
			CreatedAt: b.CreatedAt.Format(time.RFC3339),
		})
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// AdminDomainBlockCreate creates a domain block.
// POST /api/v1/admin/domain_blocks
func (h *Handler) AdminDomainBlockCreate(w http.ResponseWriter, r *http.Request) {
	domain := r.FormValue("domain")
	severity := r.FormValue("severity")
	comment := r.FormValue("comment")
	publicStr := r.FormValue("public")

	if domain == "" {
		writeAPIError(w, http.StatusBadRequest, "domain is required")
		return
	}

	if severity == "" {
		severity = "silence"
	}

	isTrue := func(v string) bool { return v == "true" || v == "1" || v == "on" }
	block := &models.DomainBlock{
		Domain:        domain,
		Severity:      severity,
		Comment:       comment,
		Public:        isTrue(publicStr),
		RejectMedia:   isTrue(r.FormValue("reject_media")),
		RejectReports: isTrue(r.FormValue("reject_reports")),
		Obfuscate:     isTrue(r.FormValue("obfuscate")),
	}

	if err := h.store.CreateDomainBlock(r.Context(), block); err != nil {
		slog.Error("api: admin domain block create error", "error", err)
		if errors.Is(err, storage.ErrInvalidDomainBlock) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "error creating domain block")
		return
	}

	h.logModeration(r, "domain", block.Domain, "block_"+severity, comment)

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"id":         block.ID,
		"domain":     block.Domain,
		"severity":   block.Severity,
		"public":     block.Public,
		"comment":    block.Comment,
		"created_at": block.CreatedAt.Format(time.RFC3339),
	})
}

// AdminDomainBlockDelete removes a domain block.
// DELETE /api/v1/admin/domain_blocks/{id}
func (h *Handler) AdminDomainBlockDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	if err := h.store.DeleteDomainBlock(r.Context(), id); err != nil {
		slog.Error("api: admin domain block delete error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error deleting domain block")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type adminDomainAllow struct {
	ID        string `json:"id"`
	Domain    string `json:"domain"`
	CreatedAt string `json:"created_at"`
}

func domainAllowResponse(allow *models.DomainAllow) adminDomainAllow {
	return adminDomainAllow{ID: allow.ID, Domain: allow.Domain, CreatedAt: allow.CreatedAt.Format(time.RFC3339)}
}

// AdminDomainAllowsList implements Mastodon's limited-federation allow API.
func (h *Handler) AdminDomainAllowsList(w http.ResponseWriter, r *http.Request) {
	allows, err := h.store.ListDomainAllows(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error fetching domain allows")
		return
	}
	result := make([]adminDomainAllow, 0, len(allows))
	for _, allow := range allows {
		result = append(result, domainAllowResponse(allow))
	}
	writeAPIJSON(w, http.StatusOK, result)
}

func (h *Handler) AdminDomainAllowGet(w http.ResponseWriter, r *http.Request) {
	allow, err := h.store.GetDomainAllow(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "domain allow not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, domainAllowResponse(allow))
}

func (h *Handler) AdminDomainAllowCreate(w http.ResponseWriter, r *http.Request) {
	allow := &models.DomainAllow{Domain: r.FormValue("domain")}
	if err := h.store.CreateDomainAllow(r.Context(), allow); err != nil {
		if errors.Is(err, storage.ErrInvalidDomainBlock) {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeAPIError(w, http.StatusUnprocessableEntity, "domain already allowed")
		return
	}
	h.logModeration(r, "domain", allow.Domain, "allow", "")
	writeAPIJSON(w, http.StatusOK, domainAllowResponse(allow))
}

func (h *Handler) AdminDomainAllowDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	allow, err := h.store.GetDomainAllow(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "domain allow not found")
		return
	}
	if err := h.store.DeleteDomainAllow(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "error deleting domain allow")
		return
	}
	h.logModeration(r, "domain", allow.Domain, "unallow", "")
	w.WriteHeader(http.StatusOK)
}

// Admin Invites API

// AdminInvitesList returns all invite codes.
// GET /api/v1/admin/invites
func (h *Handler) AdminInvitesList(w http.ResponseWriter, r *http.Request) {
	invites, err := h.store.ListInvites(r.Context())
	if err != nil {
		slog.Error("api: admin invites list error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching invites")
		return
	}

	type InviteResponse struct {
		ID          string  `json:"id"`
		Code        string  `json:"code"`
		MaxUses     int     `json:"max_uses"`
		UseCount    int     `json:"use_count"`
		ExpiresAt   *string `json:"expires_at,omitempty"`
		CreatedAt   string  `json:"created_at"`
		CreatedByID string  `json:"created_by_id"`
	}

	result := make([]InviteResponse, 0, len(invites))
	for _, inv := range invites {
		resp := InviteResponse{
			ID:          inv.ID,
			Code:        inv.Code,
			MaxUses:     inv.MaxUses,
			UseCount:    inv.UseCount,
			CreatedAt:   inv.CreatedAt.Format(time.RFC3339),
			CreatedByID: inv.CreatedByID,
		}
		if inv.ExpiresAt != nil {
			e := inv.ExpiresAt.Format(time.RFC3339)
			resp.ExpiresAt = &e
		}
		result = append(result, resp)
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// AdminInvitesCreate creates a new invite code.
// POST /api/v1/admin/invites
func (h *Handler) AdminInvitesCreate(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	maxUsesStr := r.FormValue("max_uses")
	expiresStr := r.FormValue("expires_at")
	maxUses := 0

	if ct := r.Header.Get("Content-Type"); strings.Contains(ct, "/json") || strings.Contains(ct, "+json") {
		var body struct {
			Code      string `json:"code"`
			MaxUses   int    `json:"max_uses"`
			ExpiresAt string `json:"expires_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "invalid JSON body")
			return
		}
		code = body.Code
		maxUses = body.MaxUses
		expiresStr = body.ExpiresAt
	} else if maxUsesStr != "" {
		fmt.Sscanf(maxUsesStr, "%d", &maxUses)
	}

	var expiresAt *time.Time
	if expiresStr != "" {
		t, err := time.Parse(time.RFC3339, expiresStr)
		if err == nil {
			expiresAt = &t
		} else {
			t, err := time.Parse("2006-01-02", expiresStr)
			if err == nil {
				expiresAt = &t
			} else {
				slog.Warn("api: admin invites create: invalid expires_at", "value", expiresStr)
			}
		}
	}

	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	invite, err := accounts.CreateInvite(r.Context(), h.store, account.ID, code, maxUses, expiresAt)
	if err != nil {
		slog.Error("api: admin invites create error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error creating invite")
		return
	}

	e := invite.CreatedAt.Format(time.RFC3339)
	resp := map[string]interface{}{
		"id":            invite.ID,
		"code":          invite.Code,
		"max_uses":      invite.MaxUses,
		"use_count":     invite.UseCount,
		"created_at":    e,
		"created_by_id": invite.CreatedByID,
	}
	if invite.ExpiresAt != nil {
		resp["expires_at"] = invite.ExpiresAt.Format(time.RFC3339)
	}

	writeAPIJSON(w, http.StatusOK, resp)
}

// AdminInvitesRevoke revokes an invite code.
// DELETE /api/v1/admin/invites/{id}
func (h *Handler) AdminInvitesRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	if err := h.store.DeleteInvite(r.Context(), id); err != nil {
		slog.Error("api: admin invites revoke error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error revoking invite")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// Admin Bubble Instances

// AdminBubbleInstancesList returns all configured bubble instances.
// GET /api/v1/admin/bubble_instances
func (h *Handler) AdminBubbleInstancesList(w http.ResponseWriter, r *http.Request) {
	instances, err := h.store.ListBubbleInstances(r.Context())
	if err != nil {
		slog.Error("api: admin bubble instances list error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error fetching bubble instances")
		return
	}

	type BubbleInstanceResponse struct {
		ID        string `json:"id"`
		Domain    string `json:"domain"`
		Note      string `json:"note,omitempty"`
		CreatedAt string `json:"created_at"`
	}

	result := make([]BubbleInstanceResponse, 0, len(instances))
	for _, inst := range instances {
		result = append(result, BubbleInstanceResponse{
			ID:        inst.ID,
			Domain:    inst.Domain,
			Note:      inst.Note,
			CreatedAt: inst.CreatedAt.Format(time.RFC3339),
		})
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// AdminBubbleInstanceCreate adds a domain to the bubble.
// POST /api/v1/admin/bubble_instances
func (h *Handler) AdminBubbleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	domain := r.FormValue("domain")
	note := r.FormValue("note")

	if domain == "" {
		writeAPIError(w, http.StatusBadRequest, "domain is required")
		return
	}

	b := &models.BubbleInstance{
		Domain: domain,
		Note:   note,
	}

	if err := h.store.CreateBubbleInstance(r.Context(), b); err != nil {
		slog.Error("api: admin bubble instance create error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error creating bubble instance")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]interface{}{
		"id":         b.ID,
		"domain":     b.Domain,
		"note":       b.Note,
		"created_at": b.CreatedAt.Format(time.RFC3339),
	})
}

// AdminBubbleInstanceDelete removes a domain from the bubble.
// DELETE /api/v1/admin/bubble_instances/{id}
func (h *Handler) AdminBubbleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	if err := h.store.DeleteBubbleInstance(r.Context(), id); err != nil {
		slog.Error("api: admin bubble instance delete error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error deleting bubble instance")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Admin Instance Health API

// AdminInstancesList returns all known remote instances.
// GET /api/v1/admin/instances
func (h *Handler) AdminInstancesList(w http.ResponseWriter, r *http.Request) {
	instances, err := h.store.ListInstances(r.Context())
	if err != nil {
		slog.Error("api: admin instances list error", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error listing instances")
		return
	}

	type apiInstance struct {
		ID               string     `json:"id"`
		Domain           string     `json:"domain"`
		Software         string     `json:"software"`
		Version          string     `json:"version"`
		Suspended        bool       `json:"suspended"`
		UnreachableSince *time.Time `json:"unreachable_since,omitempty"`
		LastSuccess      *time.Time `json:"last_success,omitempty"`
		LastFailure      *time.Time `json:"last_failure,omitempty"`
		BackoffCount     int        `json:"backoff_count"`
		CreatedAt        time.Time  `json:"created_at"`
		FollowersFromUs  int        `json:"followers_from_us"`
		FollowingToUs    int        `json:"following_to_us"`
	}

	result := make([]apiInstance, 0, len(instances))
	for _, inst := range instances {
		followersFromUs, _ := h.store.CountFollowersByDomain(r.Context(), inst.Domain)
		followingToUs, _ := h.store.CountFollowingByDomain(r.Context(), inst.Domain)
		result = append(result, apiInstance{
			ID:               inst.ID,
			Domain:           inst.Domain,
			Software:         inst.Software,
			Version:          inst.Version,
			Suspended:        inst.Suspended,
			UnreachableSince: inst.UnreachableSince,
			LastSuccess:      inst.LastSuccess,
			LastFailure:      inst.LastFailure,
			BackoffCount:     inst.BackoffCount,
			CreatedAt:        inst.CreatedAt,
			FollowersFromUs:  followersFromUs,
			FollowingToUs:    followingToUs,
		})
	}

	writeAPIJSON(w, http.StatusOK, result)
}

// AdminInstanceReset marks an instance as reachable again.
// POST /api/v1/admin/instances/{id}/reset
func (h *Handler) AdminInstanceReset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	instance, err := h.store.GetInstanceByID(r.Context(), id)
	if err != nil {
		slog.Error("api: admin instance get for reset", "id", id, "error", err)
		writeAPIError(w, http.StatusNotFound, "instance not found")
		return
	}

	if err := h.store.MarkInstanceReachable(r.Context(), instance.Domain); err != nil {
		slog.Error("api: admin instance reset error", "domain", instance.Domain, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error resetting instance")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// AdminInstanceToggleSuspend toggles the suspended flag on an instance.
// POST /api/v1/admin/instances/{id}/suspend
func (h *Handler) AdminInstanceToggleSuspend(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	instance, err := h.store.GetInstanceByID(r.Context(), id)
	if err != nil {
		slog.Error("api: admin instance get for suspend", "id", id, "error", err)
		writeAPIError(w, http.StatusNotFound, "instance not found")
		return
	}

	if err := h.store.SuspendInstance(r.Context(), id, !instance.Suspended); err != nil {
		slog.Error("api: admin instance suspend error", "id", id, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error toggling instance suspend")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"suspended": fmt.Sprintf("%t", !instance.Suspended)})
}

// AdminInstancePurge removes all data related to a remote instance.
// POST /api/v1/admin/instances/{id}/purge
func (h *Handler) AdminInstancePurge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	instance, err := h.store.GetInstanceByID(r.Context(), id)
	if err != nil {
		slog.Error("api: admin instance get for purge", "id", id, "error", err)
		writeAPIError(w, http.StatusNotFound, "instance not found")
		return
	}

	if err := h.store.DeleteInstanceData(r.Context(), id); err != nil {
		slog.Error("api: admin purge instance data", "domain", instance.Domain, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "error purging instance data")
		return
	}

	slog.Info("api: purged all data for instance", "domain", instance.Domain)
	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "purged"})
}

// AdminAccountMove force-migrates a local account to another instance.
// POST /api/v1/admin/accounts/{id}/move
// Body: { "target_account": "user@domain" }
// Admin version - skips password re-authentication.
func (h *Handler) AdminAccountMove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "missing id")
		return
	}

	account, err := h.store.GetAccount(r.Context(), id)
	if err != nil {
		slog.Error("api: admin move account not found", "id", id, "error", err)
		writeAPIError(w, http.StatusNotFound, "account not found")
		return
	}

	// Only local accounts can be moved
	if account.Domain != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "cannot migrate a remote account")
		return
	}
	if h.config.MigrationConfigured && !h.config.MigrationEnabled {
		writeAPIError(w, http.StatusForbidden, "account migration is disabled on this instance")
		return
	}

	params, parseMode, err := decodeMastodonParams(r, 0)
	if err != nil {
		logAPIValidationError(r, http.StatusUnprocessableEntity, "invalid request body", "parse_mode", parseMode, "parse_error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}

	targetAcct := params.First("target_account")
	if targetAcct == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "target_account is required")
		return
	}

	// Parse target domain
	parts := strings.SplitN(targetAcct, "@", 2)
	if len(parts) != 2 {
		writeAPIError(w, http.StatusUnprocessableEntity, "target_account must be in username@domain format")
		return
	}
	targetDomain := parts[1]

	if strings.EqualFold(targetDomain, h.localDomain) {
		writeAPIError(w, http.StatusUnprocessableEntity, "cannot move to the same instance")
		return
	}

	// Resolve target account
	targetAccount, err := h.federator.ResolveAccount(r.Context(), targetAcct)
	if err != nil {
		slog.Warn("api: admin move target resolution failed", "target", targetAcct, "error", err)
		writeAPIError(w, http.StatusNotFound, "target account not found")
		return
	}

	newActorURI := targetAccount.ActorURI
	if h.config.MigrationRequireTargetAlias {
		refreshedTarget, err := h.federator.RefreshActor(r.Context(), targetAccount.ActorURI, federation.ActorRefreshOptions{Force: true})
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "target account could not be refreshed for alias verification")
			return
		}
		targetAccount = refreshedTarget
		if err := accountmigration.VerifyMovePair(account, targetAccount, false); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "target account must list this account in alsoKnownAs before moving")
			return
		}
	}

	slog.Info("api: admin force-moving account",
		"account_id", account.ID,
		"username", account.Username,
		"from", fmt.Sprintf("https://%s/users/%s", h.domain, account.Username),
		"to", newActorURI,
	)

	// Mark as moved
	if err := h.store.SetAccountMoved(r.Context(), account.ID, newActorURI); err != nil {
		slog.Error("api: admin move failed to mark account", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "failed to move account")
		return
	}
	h.app.NotifyAdminsAccountMoved(r.Context(), account)

	// Build and enqueue Move activity delivery (same as user-initiated)
	moveActivity := map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       account.ActorURI + "/move/" + uuid.New().String(),
		"type":     "Move",
		"actor":    account.ActorURI,
		"object":   account.ActorURI,
		"target":   newActorURI,
	}

	activityJSON, _ := json.Marshal(moveActivity)
	if _, err := app.MigrateLocalFollowers(r.Context(), h.store, h.federator, account, targetAccount); err != nil {
		slog.Warn("admin move: failed to migrate local followers", "error", err)
	}

	// Enqueue to all effective follower destinations.
	inboxes, err := h.store.GetFollowerDestinations(r.Context(), account.ID)
	if err != nil {
		slog.Error("admin move: failed to get follower destinations", "error", err)
		return
	}
	seen := make(map[string]bool, len(inboxes))
	for _, inbox := range inboxes {
		seen[inbox] = true
		payload, _ := json.Marshal(struct {
			Inbox    string          `json:"inbox"`
			Activity json.RawMessage `json:"activity"`
		}{
			Inbox:    inbox,
			Activity: activityJSON,
		})
		job := &models.Job{
			Kind:        "delivery",
			Payload:     string(payload),
			RunAt:       time.Now(),
			MaxAttempts: models.DeliveryMaxAttempts,
			State:       "queued",
		}
		if err := h.store.EnqueueJob(r.Context(), job); err != nil {
			slog.Warn("admin move: enqueue delivery failed", "inbox", inbox, "error", err)
		}
	}

	// The destination must receive the Move as well so it can record the old
	// actor as an alias, even when it was not already following this account.
	targetInbox := targetAccount.InboxURI
	if targetAccount.SharedInboxURI != nil && *targetAccount.SharedInboxURI != "" {
		targetInbox = *targetAccount.SharedInboxURI
	}
	if targetInbox != "" && !seen[targetInbox] {
		payload, _ := json.Marshal(struct {
			Inbox    string          `json:"inbox"`
			Activity json.RawMessage `json:"activity"`
		}{Inbox: targetInbox, Activity: activityJSON})
		job := &models.Job{Kind: "delivery", Payload: string(payload), RunAt: time.Now(), MaxAttempts: models.DeliveryMaxAttempts, State: "queued"}
		if err := h.store.EnqueueJob(r.Context(), job); err != nil {
			slog.Warn("admin move: enqueue target delivery failed", "inbox", targetInbox, "error", err)
		}
	}

	writeAPIJSON(w, http.StatusOK, map[string]string{"status": "moved", "target": newActorURI})
}

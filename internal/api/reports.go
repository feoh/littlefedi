package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// splitNonEmpty splits s on sep and drops empty fields.
func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// MastodonReport is the Mastodon-compatible representation of a report returned
// to the reporter.
type MastodonReport struct {
	ID            string           `json:"id"`
	ActionTaken   bool             `json:"action_taken"`
	ActionTakenAt *string          `json:"action_taken_at"`
	Category      string           `json:"category"`
	Comment       string           `json:"comment"`
	Forwarded     bool             `json:"forwarded"`
	CreatedAt     string           `json:"created_at"`
	StatusIDs     []string         `json:"status_ids"`
	RuleIDs       []string         `json:"rule_ids"`
	TargetAccount *MastodonAccount `json:"target_account"`
}

// ReportCreate files a moderation report.
// POST /api/v1/reports
// Params: account_id (required), status_ids[], comment, category, forward.
func (h *Handler) ReportCreate(w http.ResponseWriter, r *http.Request) {
	account := getAuthAccount(r.Context())
	if account == nil {
		writeAPIError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	if err := r.ParseForm(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid form")
		return
	}

	targetID := r.FormValue("account_id")
	if targetID == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "account_id is required")
		return
	}

	statusIDs := r.Form["status_ids[]"]
	if len(statusIDs) == 0 {
		statusIDs = r.Form["status_ids"]
	}

	comment := r.FormValue("comment")
	category := r.FormValue("category")
	forward := r.FormValue("forward") == "true" || r.FormValue("forward") == "1"

	report, err := h.app.CreateReport(r.Context(), account, targetID, statusIDs, comment, category, forward)
	if err != nil {
		slog.Error("api: create report error", "error", err)
		writeAPIError(w, http.StatusUnprocessableEntity, "error creating report")
		return
	}

	resp := h.reportToMastodon(r, report)
	writeAPIJSON(w, http.StatusOK, resp)
}

func (h *Handler) reportToMastodon(r *http.Request, report *models.Report) *MastodonReport {
	ids := splitNonEmpty(report.StatusIDs, ",")
	if ids == nil {
		ids = []string{}
	}
	var target *MastodonAccount
	if acct, err := h.store.GetAccount(r.Context(), report.TargetAccountID); err == nil {
		target = h.accountToMastodon(acct, getAuthAccount(r.Context()), false)
	}
	var actionAt *string
	if report.ResolvedAt != nil {
		s := report.ResolvedAt.Format(time.RFC3339)
		actionAt = &s
	}
	return &MastodonReport{
		ID:            report.ID,
		ActionTaken:   report.State == "resolved",
		ActionTakenAt: actionAt,
		Category:      report.Category,
		Comment:       report.Comment,
		Forwarded:     report.Forwarded,
		CreatedAt:     report.CreatedAt.Format(time.RFC3339),
		StatusIDs:     ids,
		RuleIDs:       []string{},
		TargetAccount: target,
	}
}

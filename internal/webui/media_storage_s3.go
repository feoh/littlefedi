//go:build s3

package webui

import (
	"net/http"
	"strconv"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mediamigration"
	"github.com/go-chi/chi/v5"
)

func (h *Handler) registerMediaStorageRoutes(r chi.Router) {
	r.Get("/admin/media-storage", h.adminMediaStorage)
	r.Post("/admin/media-storage/start", h.adminMediaStorageStart)
	r.Post("/admin/media-storage/{id}/cancel", h.adminMediaStorageCancel)
	r.Post("/admin/media-storage/{id}/resume", h.adminMediaStorageResume)
}
func (h *Handler) adminMediaStorage(w http.ResponseWriter, r *http.Request) {
	m, _ := h.store.GetActiveMediaStorageMigration(r.Context())
	h.renderPage(w, r, "Admin - Media storage", map[string]any{"Migration": m, "Config": h.cfg.Media, "CSRFToken": getCSRFToken(r.Context())}, "admin_media_storage.html")
}
func (h *Handler) adminMediaStorageStart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	dst := h.cfg.Media
	dst.Backend = r.FormValue("backend")
	dst.CacheBackend = r.FormValue("cache_backend")
	dst.Path = strings.TrimSpace(r.FormValue("path"))
	dst.PublicBaseURL = strings.TrimSpace(r.FormValue("public_base_url"))
	dst.S3.Bucket = strings.TrimSpace(r.FormValue("bucket"))
	dst.S3.Region = strings.TrimSpace(r.FormValue("region"))
	dst.S3.Endpoint = strings.TrimSpace(r.FormValue("endpoint"))
	dst.S3.Prefix = strings.Trim(r.FormValue("prefix"), "/")
	dst.S3.PathStyle, _ = strconv.ParseBool(r.FormValue("path_style"))
	if _, err := mediamigration.Start(r.Context(), h.store, h.cfg.Media, dst); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/admin/media-storage", http.StatusSeeOther)
}
func (h *Handler) adminMediaStorageCancel(w http.ResponseWriter, r *http.Request) {
	if err := mediamigration.Cancel(r.Context(), h.store, chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/admin/media-storage", 303)
}
func (h *Handler) adminMediaStorageResume(w http.ResponseWriter, r *http.Request) {
	if err := mediamigration.Resume(r.Context(), h.store, chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/admin/media-storage", 303)
}

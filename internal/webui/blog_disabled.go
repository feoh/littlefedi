//go:build !blog

package webui

import (
	"context"

	"github.com/go-chi/chi/v5"
)

func (h *Handler) registerBlogRoutes(chi.Router)                      {}
func (h *Handler) blogCapable() bool                                  { return false }
func (h *Handler) enqueueBlogSettingsRebuild(context.Context, string) {}
func (h *Handler) enqueueBlogRebuildAll(context.Context) (int, error) { return 0, nil }

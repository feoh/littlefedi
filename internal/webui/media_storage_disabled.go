//go:build !s3

package webui

import "github.com/go-chi/chi/v5"

func (h *Handler) registerMediaStorageRoutes(chi.Router) {}

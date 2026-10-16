package blog

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
)

// RegisterRoutes mounts the pre-rendered static blog under /@{username}/blog
// on r. Files are served straight off disk (cfg.ResolvedOutputDir()); nothing
// is executed per-request, since Generate already rendered everything at
// publish time.
func RegisterRoutes(r chi.Router, cfg config.Blog) {
	root := cfg.ResolvedOutputDir()

	r.Get("/@{username}/blog", func(w http.ResponseWriter, req *http.Request) {
		serveBlogFile(w, req, root, "")
	})
	r.Get("/@{username}/blog/*", func(w http.ResponseWriter, req *http.Request) {
		serveBlogFile(w, req, root, chi.URLParam(req, "*"))
	})
}

func serveBlogFile(w http.ResponseWriter, req *http.Request, root, requested string) {
	generateMu.RLock()
	defer generateMu.RUnlock()

	username := chi.URLParam(req, "username")
	if username == "" || strings.ContainsAny(username, "/\\") || username == "." || username == ".." {
		http.NotFound(w, req)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+requested), "/")
	if rel == "" {
		rel = "index.html"
	}
	base := filepath.Join(root, username)
	full := filepath.Join(base, filepath.FromSlash(rel))
	contained, err := filepath.Rel(base, full)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		http.NotFound(w, req)
		return
	}
	// The stylesheet keeps one URL across theme changes, so it has to be
	// revalidated rather than reused from cache; ServeFile still answers 304
	// on an unchanged file, which is the whole cost.
	if rel == "style.css" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	info, err := os.Stat(full)
	if err == nil && info.IsDir() {
		full = filepath.Join(full, "index.html")
		info, err = os.Stat(full)
	}
	if err != nil || info.IsDir() {
		http.NotFound(w, req)
		return
	}
	http.ServeFile(w, req, full)
}

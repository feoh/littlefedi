package main

import (
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
)

func localMediaHandler(root string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel, ok := strings.CutPrefix(r.URL.Path, "/media/")
		id, name, hasName := strings.Cut(rel, "/")
		if !ok || !hasName || uuid.Validate(id) != nil || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			http.NotFound(w, r)
			return
		}
		dir, err := os.OpenRoot(root)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer dir.Close()
		parent, err := dir.Lstat(id)
		if err != nil || !parent.IsDir() {
			http.NotFound(w, r)
			return
		}
		entry, err := dir.Lstat(rel)
		if err != nil || !entry.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		file, err := dir.Open(rel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(w, r, name, info.ModTime(), file)
	})
}

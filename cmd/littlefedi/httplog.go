package main

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// quietRequestPath reports whether a request path is high-volume, low-signal
// traffic (static assets, proxied media, probes) that should log at debug
// only, so a media-heavy page load doesn't emit dozens of info lines.
func quietRequestPath(path string) bool {
	return strings.HasPrefix(path, "/static/") ||
		strings.HasPrefix(path, "/media/") ||
		strings.HasPrefix(path, "/emoji/") ||
		path == "/health" || path == "/readyz" || path == "/metrics" ||
		path == "/favicon.ico" || path == "/sw.js" || path == "/robots.txt"
}

// accessLogMiddleware logs one structured slog line per request. It replaces
// chi's default logger, which formats a colored text line per request through
// the log package regardless of level - measurable per-request overhead on
// single-core hardware. When the line would be filtered by the current log
// level the response writer is not even wrapped.
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		level := slog.LevelInfo
		if quietRequestPath(r.URL.Path) {
			level = slog.LevelDebug
		}
		if !slog.Default().Enabled(r.Context(), level) {
			next.ServeHTTP(w, r)
			return
		}
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		defer func() {
			slog.Log(r.Context(), level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		}()
		next.ServeHTTP(ww, r)
	})
}

// startPprofServer serves Go's pprof profiler on its own listener, kept off
// the main router so profiling can't be reached through the public port. The
// handler is unauthenticated; the config comment tells users to bind it to
// localhost.
func startPprofServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		slog.Info("pprof listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("pprof server exited", "error", err)
		}
	}()
}

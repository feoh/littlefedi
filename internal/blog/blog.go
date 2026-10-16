// Package blog renders an account's flagged posts into a static, no-JS,
// accessible HTML site plus an Atom feed. It is only meaningfully imported
// from -tags blog build files (see cmd/littlefedi/blog_enabled.go and
// internal/webui/blog_enabled.go); a plain build never references it, so it
// is never compiled into that binary.
package blog

// RebuildJobKind is the internal/queue job kind used to trigger an async
// rebuild of one account's static site.
const RebuildJobKind = "blog_rebuild"

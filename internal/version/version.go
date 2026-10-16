// Package version holds the build version, injected at build time via
// -ldflags "-X .../internal/version.Version=26.07.17". See the Makefile.
package version

// Version format is CalVer YY.MM.DD (build date), with an optional trailing
// letter (26.07.17b) for a second same-day build. "dev" means a plain
// `go build` with no version injected.
var Version = "dev"

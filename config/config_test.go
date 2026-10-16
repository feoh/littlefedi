package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "littlefedi.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMinimalConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, `local_domain = "Example.COM"`+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalDomain != "example.com" || cfg.WebDomain != "example.com" || cfg.Domain != "example.com" {
		t.Errorf("domains = %q / %q / %q", cfg.LocalDomain, cfg.WebDomain, cfg.Domain)
	}
	if cfg.Instance.Name != "littleFedi" {
		t.Errorf("Instance.Name = %q", cfg.Instance.Name)
	}
	if !strings.Contains(cfg.Federation.UserAgent, "example.com") {
		t.Errorf("UserAgent = %q", cfg.Federation.UserAgent)
	}
	if cfg.Streaming.Backend != "local" || cfg.Media.OwnedBackend() != "local" {
		t.Errorf("backends = %q / %q", cfg.Streaming.Backend, cfg.Media.OwnedBackend())
	}
	if !cfg.Auth.EnforceOAuthScopes {
		t.Error("OAuth scope enforcement must always be on")
	}
	if cfg.IsMultiUser() {
		t.Error("a config without registrations should be single-user")
	}
}

func TestLoadSplitDomainsAndSections(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
local_domain = "example.com"
web_domain = "social.example.com"
[instance]
name = "  My Instance  "
[auth]
registrations = true
[http]
listen = "127.0.0.1:9090"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalDomain != "example.com" || cfg.WebDomain != "social.example.com" {
		t.Errorf("domains = %q / %q", cfg.LocalDomain, cfg.WebDomain)
	}
	if cfg.Instance.Name != "My Instance" {
		t.Errorf("Instance.Name = %q", cfg.Instance.Name)
	}
	if !cfg.IsMultiUser() {
		t.Error("open registrations should make the instance multi-user")
	}
	if cfg.HTTP.Listen != "127.0.0.1:9090" {
		t.Errorf("HTTP.Listen = %q", cfg.HTTP.Listen)
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	t.Setenv("LITTLEFEDI_WEB_DOMAIN", "env.example.com")
	t.Setenv("LITTLEFEDI_DB_DSN", "/tmp/env.db")
	cfg, err := Load(writeConfig(t, `local_domain = "example.com"`+"\n"+`web_domain = "file.example.com"`+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebDomain != "env.example.com" {
		t.Errorf("WebDomain = %q, want the environment value", cfg.WebDomain)
	}
	if cfg.DB.DSN != "/tmp/env.db" {
		t.Errorf("DB.DSN = %q", cfg.DB.DSN)
	}
}

func TestLoadRejectsInvalidConfigs(t *testing.T) {
	bad := map[string]string{
		"missing domain":     "",
		"bad streaming":      "local_domain = \"example.com\"\n[streaming]\nbackend = \"redis\"\n",
		"short password min": "local_domain = \"example.com\"\n[auth]\nmin_password_length = 8\n",
		"mfa without key":    "local_domain = \"example.com\"\n[auth]\nrequire_admin_mfa = true\n",
		"public pprof":       "local_domain = \"example.com\"\n[observability]\npprof_listen = \"0.0.0.0:6060\"\n",
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Error("Load accepted an invalid config")
			}
		})
	}
}

func TestDefaultConfigAndMediaPaths(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.DB.DSN == "" || cfg.HTTP.Listen == "" {
		t.Errorf("DefaultConfig left DSN or listen empty: %q %q", cfg.DB.DSN, cfg.HTTP.Listen)
	}
	m := Media{Path: "/srv/media"}
	if m.ResolvedCachePath() != filepath.Join("/srv/media", "cache") || m.ResolvedCacheBackend() != "local" {
		t.Errorf("media cache defaults = %q %q", m.ResolvedCachePath(), m.ResolvedCacheBackend())
	}
	if ua := DefaultUserAgent("example.com"); !strings.Contains(ua, "littleFedi") || !strings.Contains(ua, "example.com") {
		t.Errorf("DefaultUserAgent = %q", ua)
	}
}

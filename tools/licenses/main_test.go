package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLicenceFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"LICENSE", "PATENTS", "NOTICE.md", "README.md", "licence.txt", "go.mod"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "LICENSES"), 0o700); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, path := range licenceFiles(dir) {
		names = append(names, filepath.Base(path))
	}
	if got := strings.Join(names, ","); got != "LICENSE,NOTICE.md,PATENTS,licence.txt" {
		t.Errorf("licenceFiles = %s", got)
	}
}

// Every third-party module compiled into a release must be credited in
// THIRD_PARTY_LICENSES.md, which ships next to the binary.
func TestThirdPartyNoticesCoverReleaseModules(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	notices, err := os.ReadFile(filepath.Join("..", "..", "THIRD_PARTY_LICENSES.md"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join("..", ".."))
	for _, m := range listModules("blog", "linux", "amd64") {
		if !strings.Contains(string(notices), m.Path) {
			t.Errorf("THIRD_PARTY_LICENSES.md does not credit %s %s", m.Path, m.Version)
		}
	}
}

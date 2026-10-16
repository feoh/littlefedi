package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBinarySmoke builds the real binary and walks the operator's first-run
// path: migrate, create an administrator, start the server, and federate-
// discover the account. It is skipped with -short because it compiles.
func TestBinarySmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "littlefedi")
	build := exec.Command(goBin, "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	configPath := filepath.Join(dir, "littlefedi.toml")
	config := fmt.Sprintf("local_domain = \"smoke.example\"\n[http]\nlisten = %q\n[db]\ndsn = %q\n[media]\npath = %q\n",
		addr, filepath.Join(dir, "littlefedi.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)",
		filepath.Join(dir, "media"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"--config", configPath}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("littlefedi %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if out := run("version"); strings.TrimSpace(out) == "" {
		t.Error("version printed nothing")
	}
	if out := run("migrate"); !strings.Contains(out, "Migrations complete") {
		t.Errorf("migrate output = %q", out)
	}
	if out := run("admin", "create-user", "--username", "alice", "--password", "a-long-enough-test-password", "--admin"); !strings.Contains(out, "User created: alice") {
		t.Errorf("create-user output = %q", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, binary, "--config", configPath)
	server.Dir = dir
	logs, _ := os.Create(filepath.Join(dir, "server.log"))
	server.Stdout, server.Stderr = logs, logs
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = server.Process.Signal(syscall.SIGTERM)
		_ = server.Wait()
		if t.Failed() {
			out, _ := os.ReadFile(filepath.Join(dir, "server.log"))
			t.Logf("server log:\n%s", out)
		}
	}()

	base := "http://" + addr
	for {
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("server never became ready")
		case <-time.After(100 * time.Millisecond):
		}
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/.well-known/webfinger?resource=acct:alice@smoke.example", nil)
	req.Host = "smoke.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var finger map[string]interface{}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &finger) != nil || finger["subject"] != "acct:alice@smoke.example" {
		t.Errorf("webfinger = %d %s", resp.StatusCode, body)
	}
}

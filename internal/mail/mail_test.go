package mail

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/testutil"
)

// fakeSMTP is a minimal plain-text SMTP server that records each message.
type fakeSMTP struct {
	ln       net.Listener
	mu       sync.Mutex
	messages []string
	rcpts    []string
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	write("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			write("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			write("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			s.mu.Lock()
			s.rcpts = append(s.rcpts, strings.TrimSpace(line))
			s.mu.Unlock()
			write("250 ok")
		case cmd == "DATA":
			write("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.messages = append(s.messages, b.String())
			s.mu.Unlock()
			write("250 queued")
		case cmd == "QUIT":
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func (s *fakeSMTP) mailer(t *testing.T) *Mailer {
	t.Helper()
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	m := New(config.SMTP{Host: host, Port: p, From: "littlefedi@test.example"}, "social.test.example", testutil.Domain)
	if !m.IsConfigured() {
		t.Fatal("mailer not configured")
	}
	return m
}

func (s *fakeSMTP) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func TestUnconfiguredMailer(t *testing.T) {
	m := New(config.SMTP{}, "a", "b")
	if m != nil || m.IsConfigured() {
		t.Fatal("a mailer without a host should be nil and unconfigured")
	}
	if err := m.Send("someone@example.com", "subject", "body"); err != nil {
		t.Errorf("unconfigured Send should log and succeed, got %v", err)
	}
}

func TestSendDeliversMessage(t *testing.T) {
	server := startFakeSMTP(t)
	if err := server.mailer(t).Send("Alice <alice@example.com>", "Hello there", "Line one\nLine two"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := server.sent()
	if len(msgs) != 1 {
		t.Fatalf("server received %d messages", len(msgs))
	}
	for _, want := range []string{"Subject: Hello there", "alice@example.com", "Line one", "Line two", "From:"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("message lacks %q:\n%s", want, msgs[0])
		}
	}
	if len(server.rcpts) != 1 || !strings.Contains(server.rcpts[0], "<alice@example.com>") {
		t.Errorf("RCPT = %v", server.rcpts)
	}
}

func TestSendRejectsHeaderInjection(t *testing.T) {
	server := startFakeSMTP(t)
	err := server.mailer(t).Send("alice@example.com", "Hi\r\nBcc: victim@example.com", "body")
	if !errors.Is(err, errInvalidMessage) || !permanentSendError(err) {
		t.Errorf("injected subject error = %v", err)
	}
	if len(server.sent()) != 0 {
		t.Error("an invalid message reached the server")
	}
}

func TestEmailVerificationJobIsSent(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	account := testutil.CreateLocalAccount(t, store, "alice")
	account.Email = "alice@example.com"

	if err := EnqueueEmailVerification(ctx, store, "social.test.example", account); err != nil {
		t.Fatalf("EnqueueEmailVerification: %v", err)
	}
	job, err := store.DequeueJob(ctx, []string{"send_email"})
	if err != nil || job == nil {
		t.Fatalf("no send_email job queued: %v", err)
	}
	var payload EmailJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.To != "alice@example.com" || !strings.Contains(payload.Body, "https://social.test.example/auth/verify_email?token=") {
		t.Errorf("payload = %+v", payload)
	}

	server := startFakeSMTP(t)
	if err := HandleEmailJob(store, server.mailer(t))(ctx, job.Payload); err != nil {
		t.Fatalf("HandleEmailJob: %v", err)
	}
	if msgs := server.sent(); len(msgs) != 1 || !strings.Contains(msgs[0], "verify_email?token=") {
		t.Errorf("verification email not delivered: %v", msgs)
	}
}

func TestHandleEmailJobRejectsBadPayload(t *testing.T) {
	server := startFakeSMTP(t)
	if err := HandleEmailJob(testutil.NewStore(t), server.mailer(t))(context.Background(), "not json"); err == nil {
		t.Error("a malformed payload was accepted")
	}
}

func TestNotifyAdminsBackupQueuesForVerifiedAdmins(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewStore(t)
	admin := testutil.CreateLocalAccount(t, store, "admin_user")
	if err := store.SetAdmin(ctx, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	admin, _ = store.GetAccount(ctx, admin.ID)
	admin.Email = "admin@example.com"
	admin.EmailVerified = true
	admin.EmailNotifyBackupSuccess = true
	if err := store.UpdateAccount(ctx, admin); err != nil {
		t.Fatal(err)
	}

	NotifyAdminsBackup(ctx, store, testutil.Domain, true, "/backups/x.tar")
	job, err := store.DequeueJob(ctx, []string{"send_email"})
	if err != nil || job == nil {
		t.Fatalf("no admin email queued: %v", err)
	}
	if !strings.Contains(job.Payload, "admin@example.com") || !strings.Contains(job.Payload, "Backup completed") {
		t.Errorf("payload = %s", job.Payload)
	}
}

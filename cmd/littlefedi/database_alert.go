package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const databaseAlertCooldown = time.Hour

type databaseAlert struct {
	mu         sync.Mutex
	sendMail   func(to, subject, body string) error
	domain     string
	configured []string
	recipients []string
	lastSent   time.Time
}

func newDatabaseAlert(sendMail func(to, subject, body string) error, domain, configuredRecipients string, admins []*models.Account) *databaseAlert {
	alert := &databaseAlert{sendMail: sendMail, domain: domain, configured: splitDatabaseAlertRecipients(configuredRecipients)}
	alert.refresh(admins)
	return alert
}

func splitDatabaseAlertRecipients(value string) []string {
	var recipients []string
	for _, address := range strings.Split(value, ",") {
		if address = strings.TrimSpace(address); address != "" {
			recipients = append(recipients, address)
		}
	}
	return recipients
}

func (a *databaseAlert) refresh(admins []*models.Account) {
	seen := make(map[string]bool, len(a.configured)+len(admins))
	recipients := make([]string, 0, len(a.configured)+len(admins))
	for _, address := range a.configured {
		if !seen[address] {
			seen[address] = true
			recipients = append(recipients, address)
		}
	}
	for _, admin := range admins {
		if admin.Email != "" && !seen[admin.Email] {
			seen[admin.Email] = true
			recipients = append(recipients, admin.Email)
		}
	}
	a.mu.Lock()
	a.recipients = recipients
	a.mu.Unlock()
}

func (a *databaseAlert) notify(err error) {
	recipients := a.beginAlert()
	if len(recipients) != 0 {
		go a.send(recipients, err)
	}
}

func (a *databaseAlert) notifyBeforeExit(err error) {
	recipients := a.beginAlert()
	if len(recipients) != 0 {
		a.send(recipients, err)
	}
}

func (a *databaseAlert) beginAlert() []string {
	a.mu.Lock()
	if a.sendMail == nil || len(a.recipients) == 0 || time.Since(a.lastSent) < databaseAlertCooldown {
		a.mu.Unlock()
		return nil
	}
	a.lastSent = time.Now()
	recipients := append([]string(nil), a.recipients...)
	a.mu.Unlock()
	return recipients
}

func (a *databaseAlert) send(recipients []string, databaseErr error) {
	subject := fmt.Sprintf("[%s] CRITICAL database problem", a.domain)
	body := fmt.Sprintf(
		"littleFedi detected a critical database problem on %s. The service may be unable to serve requests or save data. "+
			"Check the database and service logs immediately.\n\nDetected at: %s\n\n- littleFedi",
		a.domain, time.Now().UTC().Format(time.RFC3339),
	)
	for _, recipient := range recipients {
		if sendErr := a.sendMail(recipient, subject, body); sendErr != nil {
			slog.Error("database alert email failed", "recipient", recipient, "error", sendErr, "database_error", databaseErr)
		}
	}
}

func runDatabaseMonitor(ctx context.Context, store storage.Storage, alert *databaseAlert) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := store.Ping(checkCtx); err != nil {
				cancel()
				alert.notify(err)
				continue
			}
			admins, err := store.GetAdmins(checkCtx)
			cancel()
			if err != nil {
				alert.notify(err)
				continue
			}
			alert.refresh(admins)
		}
	}
}

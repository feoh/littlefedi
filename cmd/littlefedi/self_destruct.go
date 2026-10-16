package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

func cmdSelfDestruct(ctx context.Context, store storage.Storage, cfg *config.Config, in io.Reader, out io.Writer) error {
	if cfg.SelfDestruct {
		run, err := store.GetSelfDestructRun(ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fmt.Fprintln(out, "Self-destruct is enabled but no run is scheduled yet; restart the server to create the delivery queue.")
				return nil
			}
			return fmt.Errorf("read self-destruct status: %w", err)
		}
		stats, err := store.GetQueueStats(ctx, time.Now(), time.Now().Add(-cfg.Queue.StaleRunningTimeout))
		if err != nil {
			return fmt.Errorf("read queue status: %w", err)
		}
		fmt.Fprintf(out, "Self-destruct enabled since %s: %d identities, %d peer inboxes, %d deliveries.\n", run.StartedAt.Format(time.RFC3339), run.IdentityCount, run.InboxCount, run.DeliveryCount)
		for _, kind := range stats.ByKind {
			if kind.Kind == models.SelfDestructDeliveryJobKind {
				fmt.Fprintf(out, "self_destruct_delivery: ready=%d scheduled=%d running=%d failed=%d\n", kind.Ready, kind.Scheduled, kind.Running, kind.Failed)
				return nil
			}
		}
		fmt.Fprintln(out, "self_destruct_delivery: ready=0 scheduled=0 running=0 failed=0")
		return nil
	}
	reader := bufio.NewReader(in)
	fmt.Fprintf(out, "This is irreversible. Local data will be retained; back up first. Type the configured web domain (%s): ", cfg.WebDomain)
	domain, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.TrimSpace(domain) != cfg.WebDomain {
		return fmt.Errorf("confirmation did not match web domain")
	}
	fmt.Fprint(out, "Type SELF-DESTRUCT to continue: ")
	confirm, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.TrimSpace(confirm) != "SELF-DESTRUCT" {
		return fmt.Errorf("confirmation did not match SELF-DESTRUCT")
	}
	fmt.Fprintln(out, "Confirmed. Set self_destruct = true (or LITTLEFEDI_SELF_DESTRUCT=1) and restart littleFedi with queue workers running. It will retain local data and deliver Delete(Person) activities to known peers.")
	return nil
}

func selfDestructGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions ||
			(r.Method == http.MethodPost && (r.URL.Path == "/auth/sign_in" || r.URL.Path == "/oauth/authorize" || r.URL.Path == "/oauth/token")) {
			next.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusGone)
	})
}

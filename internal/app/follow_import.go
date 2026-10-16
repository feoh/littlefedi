package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/queue"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const followImportBlockSize = 128

var errFollowImportStorage = errors.New("follow import storage failure")

func RequestFollowImport(ctx context.Context, store storage.Storage, account *models.Account, handles []string) (*models.AccountImport, error) {
	if account == nil || account.Domain != nil || account.IsServiceActor || account.DeletionState != "" || len(handles) == 0 {
		return nil, fmt.Errorf("follow import is unavailable")
	}
	result := models.AccountImportResult{FollowImport: &models.FollowImportProgress{Total: len(handles)}}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	imp := &models.AccountImport{ID: uuid.NewString(), AccountID: account.ID, ResultJSON: string(resultJSON)}
	blocks := make([]string, 0, (len(handles)+followImportBlockSize-1)/followImportBlockSize)
	for start := 0; start < len(handles); start += followImportBlockSize {
		body, err := json.Marshal(handles[start:min(start+followImportBlockSize, len(handles))])
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, string(body))
	}
	payload, _ := json.Marshal(accountImportPayload{ImportID: imp.ID, AccountID: account.ID})
	job := &models.Job{ID: imp.ID, Kind: models.FollowImportJobKind, Payload: string(payload), MaxAttempts: 10}
	if err := store.CreateFollowImport(ctx, imp, blocks, job); err != nil {
		return nil, err
	}
	return imp, nil
}

func HandleFollowImportJob(store storage.Storage, federator *federation.Federator, localDomain, webDomain string, rate time.Duration) func(context.Context, string) error {
	var resolve func(context.Context, string) (*models.Account, error)
	if federator != nil {
		resolve = federator.ResolveAccount
	}
	return func(ctx context.Context, body string) error {
		return processFollowImport(ctx, store, resolve, localDomain, webDomain, rate, body)
	}
}

func processFollowImport(ctx context.Context, store storage.Storage, resolve func(context.Context, string) (*models.Account, error), localDomain, webDomain string, rate time.Duration, body string) error {
	var payload accountImportPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil || payload.ImportID == "" || payload.AccountID == "" {
		return queue.Permanent(fmt.Errorf("invalid follow import job"))
	}
	imp, err := store.GetAccountImport(ctx, payload.ImportID, payload.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if imp.State == "complete" || imp.State == "failed" {
		return nil
	}
	var result models.AccountImportResult
	if err := json.Unmarshal([]byte(imp.ResultJSON), &result); err != nil || result.FollowImport == nil {
		return queue.Permanent(fmt.Errorf("invalid follow import progress"))
	}
	p := result.FollowImport
	if p.Total <= 0 || p.Processed < 0 || p.Processed >= p.Total || p.Retries < 0 || p.Retries > 2 {
		return queue.Permanent(fmt.Errorf("invalid follow import cursor"))
	}
	account, err := store.GetAccount(ctx, imp.AccountID)
	if err != nil {
		return err
	}
	if account.Domain != nil || account.IsServiceActor || account.DeletionState != "" || account.SuspendedAt != nil || account.ModerationState == "disabled" {
		return queue.Permanent(fmt.Errorf("follow import account is unavailable"))
	}
	if rate <= 0 {
		rate = importRateLimit
	}
	deadline := time.Now().Add(45 * time.Second)
	nextAt := time.Now()
	blockIndex := -1
	var handles []string
	for n := 0; n < 25; n++ {
		if deadline.Sub(nextAt) < 20*time.Second || time.Until(deadline) < 20*time.Second {
			return queue.Reschedule(nextAt)
		}
		timer := time.NewTimer(max(0, time.Until(nextAt)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		nextAt = time.Now().Add(rate)
		if index := p.Processed / followImportBlockSize; index != blockIndex {
			block, err := store.GetJobPayload(ctx, imp.ID+":"+strconv.Itoa(index))
			if err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(block), &handles); err != nil || len(handles) != min(followImportBlockSize, p.Total-index*followImportBlockSize) {
				return queue.Permanent(fmt.Errorf("invalid follow import block"))
			}
			blockIndex = index
		}
		handle := handles[p.Processed%followImportBlockSize]
		target, err := lookupImportAccount(ctx, store, resolve, localDomain, handle)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errFollowImportStorage) {
			return err
		}
		if err == nil && (target == nil || target.Domain != nil && target.InboxURI == "" && (target.SharedInboxURI == nil || *target.SharedInboxURI == "")) {
			err = fmt.Errorf("resolved account has no delivery inbox")
		}
		expected := imp.ResultJSON
		var follow *models.Follow
		var jobs []*models.Job
		if err != nil {
			imp.LastError = err.Error()
			if len(imp.LastError) > 1024 {
				imp.LastError = imp.LastError[:1024]
			}
			if retryableImportError(err) && p.Retries < 2 {
				p.Retries++
				imp.State = "running"
				encoded, _ := json.Marshal(result)
				imp.ResultJSON = string(encoded)
				advanced, saveErr := store.AdvanceFollowImport(ctx, imp, expected, nil, nil)
				if saveErr != nil {
					return saveErr
				}
				if !advanced {
					return queue.Reschedule(time.Now())
				}
				return queue.Reschedule(time.Now().Add(time.Duration(p.Retries) * time.Minute))
			}
			slog.Warn("import: follow resolve failed", "import_id", imp.ID, "handle", handle, "error", err)
			p.Failed++
			result.Following.Skipped++
		} else {
			var existing bool
			follow, jobs, existing, err = prepareImportFollow(ctx, store, webDomain, account, target)
			if err != nil {
				return err
			}
			imp.LastError = ""
			if follow != nil {
				result.Following.Imported++
			} else {
				result.Following.Skipped++
				if existing {
					p.Existing++
				}
			}
		}
		p.Processed++
		p.Retries = 0
		imp.State = "running"
		if p.Processed == p.Total {
			imp.State = "complete"
		}
		encoded, _ := json.Marshal(result)
		imp.ResultJSON = string(encoded)
		advanced, err := store.AdvanceFollowImport(ctx, imp, expected, follow, jobs)
		if err != nil {
			return err
		}
		if !advanced {
			return queue.Reschedule(time.Now())
		}
		if imp.State == "complete" {
			slog.Info("import: follows complete", "import_id", imp.ID, "imported", result.Following.Imported, "skipped", result.Following.Skipped)
			return nil
		}
	}
	return queue.Reschedule(nextAt)
}

func lookupImportAccount(ctx context.Context, store storage.Storage, resolve func(context.Context, string) (*models.Account, error), domain, handle string) (*models.Account, error) {
	username, host, remote := strings.Cut(handle, "@")
	var accountDomain *string
	if remote && !strings.EqualFold(host, domain) {
		allowed, reason, err := store.FederationDomainAllowed(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errFollowImportStorage, err)
		}
		if !allowed {
			return nil, &federation.FederationPolicyError{Domain: host, Reason: reason}
		}
		accountDomain = &host
	}
	target, err := store.GetAccountByUsernameDomain(ctx, username, accountDomain)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %w", errFollowImportStorage, err)
	}
	if err == nil || accountDomain == nil {
		return target, err
	}
	if resolve == nil {
		return nil, fmt.Errorf("cannot resolve remote account without federation")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return resolve(lookupCtx, handle)
}

func prepareImportFollow(ctx context.Context, store storage.Storage, domain string, account, target *models.Account) (*models.Follow, []*models.Job, bool, error) {
	if target == nil {
		return nil, nil, false, fmt.Errorf("resolved follow account is missing")
	}
	if target.ID == account.ID {
		return nil, nil, false, nil
	}
	for _, pair := range [][2]string{{account.ID, target.ID}, {target.ID, account.ID}} {
		blocked, err := store.BlockExists(ctx, pair[0], pair[1])
		if err != nil || blocked {
			return nil, nil, false, err
		}
	}
	if target.Domain != nil {
		allowed, _, err := store.FederationDomainAllowed(ctx, *target.Domain)
		if err != nil || !allowed {
			return nil, nil, false, err
		}
	}
	if _, err := store.GetFollowByPair(ctx, account.ID, target.ID); err == nil {
		return nil, nil, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, err
	}
	follow := &models.Follow{AccountID: account.ID, TargetAccountID: target.ID,
		URI: fmt.Sprintf("https://%s/follows/%s", domain, uuid.NewString()), State: models.FollowAccepted}
	if target.Domain != nil || target.ManuallyApprovesFollowers {
		follow.State = models.FollowPending
	}
	if target.Domain == nil {
		return follow, nil, false, nil
	}
	inbox := target.InboxURI
	if target.SharedInboxURI != nil && *target.SharedInboxURI != "" {
		inbox = *target.SharedInboxURI
	}
	job, err := federation.PrepareDelivery(ctx, store, inbox, map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams", "id": follow.URI,
		"type": "Follow", "actor": account.ActorURI, "object": target.ActorURI,
	})
	if err != nil {
		if errors.Is(err, storage.ErrFederationBlocked) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	return follow, []*models.Job{job}, false, nil
}

func retryableImportError(err error) bool {
	var fetch *federation.RemoteFetchError
	if errors.As(err, &fetch) {
		return fetch.StatusCode == http.StatusTooManyRequests || fetch.StatusCode >= 500
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return false
	}
	var network net.Error
	var connection *net.OpError
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &connection) || errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

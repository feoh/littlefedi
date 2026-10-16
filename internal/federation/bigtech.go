package federation

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const (
	commercialListMaxBytes   = 256 << 10
	commercialListMaxDomains = 1000
)

const CommercialBigTechRefreshJobKind = "commercial_bigtech_refresh"

type BigTechListSyncer struct {
	store  storage.Storage
	url    string
	client *http.Client
}

func NewBigTechListSyncer(store storage.Storage, sourceURL string) (*BigTechListSyncer, error) {
	u, err := url.Parse(sourceURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("commercial list URL must use HTTPS without credentials")
	}
	host := u.Host
	transport := &http.Transport{}
	transport.DialContext = ssrfGuardedDialer().DialContext
	return &BigTechListSyncer{store: store, url: sourceURL, client: &http.Client{
		Timeout: 10 * time.Second, Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, host) {
				return fmt.Errorf("commercial list redirect must stay on the same HTTPS host")
			}
			return nil
		},
	}}, nil
}

func (s *BigTechListSyncer) Refresh(ctx context.Context) error {
	acquired, err := s.store.AcquireFederationPolicyLease(ctx, time.Now().Add(time.Minute))
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("commercial list refresh already in progress")
	}
	defer func() { _ = s.store.ReleaseFederationPolicyLease(context.WithoutCancel(ctx)) }()

	now := time.Now()
	old, err := s.store.GetFederationPolicyState(ctx)
	if err != nil {
		return err
	}
	state := &models.FederationPolicyState{SourceURL: s.url, ETag: old.ETag, LastModified: old.LastModified, LastAttempt: &now, LastSuccess: old.LastSuccess}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return err
	}
	if old.SourceURL == s.url {
		if old.ETag != "" {
			req.Header.Set("If-None-Match", old.ETag)
		}
		if old.LastModified != "" {
			req.Header.Set("If-Modified-Since", old.LastModified)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		state.LastError = err.Error()
		_ = s.store.UpdateFederationPolicySync(ctx, state)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		blocks, listErr := s.store.ListCommercialDomainBlocks(ctx)
		if listErr != nil || len(blocks) == 0 {
			err = fmt.Errorf("commercial list returned 304 without a valid local snapshot")
			state.LastError = err.Error()
			_ = s.store.UpdateFederationPolicySync(ctx, state)
			return err
		}
		state.LastSuccess = &now
		state.LastError = ""
		return s.store.UpdateFederationPolicySync(ctx, state)
	}
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("commercial list returned HTTP %d", resp.StatusCode)
		state.LastError = err.Error()
		_ = s.store.UpdateFederationPolicySync(ctx, state)
		return err
	}
	blocks, err := parseCommercialList(io.LimitReader(resp.Body, commercialListMaxBytes+1))
	if err != nil {
		state.LastError = err.Error()
		_ = s.store.UpdateFederationPolicySync(ctx, state)
		return err
	}
	state.ETag, state.LastModified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	state.LastSuccess, state.LastError = &now, ""
	return s.store.ReplaceCommercialDomainBlocks(ctx, blocks, state)
}

func parseCommercialList(r io.Reader) ([]*models.CommercialDomainBlock, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(body) > commercialListMaxBytes {
		return nil, fmt.Errorf("commercial list exceeds %d bytes", commercialListMaxBytes)
	}
	reader := csv.NewReader(bytes.NewReader(body))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read commercial list header: %w", err)
	}
	want := []string{"domain", "organization", "service", "reference"}
	if len(header) != len(want) {
		return nil, fmt.Errorf("commercial list must have %d columns", len(want))
	}
	for i := range want {
		if strings.TrimSpace(header[i]) != want[i] {
			return nil, fmt.Errorf("commercial list column %d must be %q", i+1, want[i])
		}
	}
	seen := make(map[string]struct{})
	blocks := make([]*models.CommercialDomainBlock, 0)
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read commercial list: %w", err)
		}
		if len(record) != 4 {
			return nil, fmt.Errorf("commercial list row has %d columns", len(record))
		}
		domain, err := storage.NormalizeFederationDomain(record[0])
		if err != nil {
			return nil, err
		}
		if _, ok := seen[domain]; ok {
			return nil, fmt.Errorf("commercial list contains duplicate %q", domain)
		}
		seen[domain] = struct{}{}
		organization, service, reference := strings.TrimSpace(record[1]), strings.TrimSpace(record[2]), strings.TrimSpace(record[3])
		refURL, refErr := url.Parse(reference)
		if organization == "" || service == "" || refErr != nil || refURL.Scheme != "https" || refURL.Hostname() == "" || refURL.User != nil {
			return nil, fmt.Errorf("commercial list metadata for %s is incomplete or invalid", domain)
		}
		blocks = append(blocks, &models.CommercialDomainBlock{Domain: domain, Organization: organization, Service: service, Reference: reference})
		if len(blocks) > commercialListMaxDomains {
			return nil, fmt.Errorf("commercial list exceeds %d domains", commercialListMaxDomains)
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("commercial list is empty")
	}
	return blocks, nil
}

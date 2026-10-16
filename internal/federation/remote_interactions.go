package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

type remoteRESTAccount struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	Acct           string `json:"acct"`
	DisplayName    string `json:"display_name"`
	URL            string `json:"url"`
	Avatar         string `json:"avatar"`
	Header         string `json:"header"`
	Note           string `json:"note"`
	Locked         bool   `json:"locked"`
	CreatedAt      string `json:"created_at"`
	FollowersCount int    `json:"followers_count"`
	FollowingCount int    `json:"following_count"`
	StatusesCount  int    `json:"statuses_count"`
}

type remoteRESTReaction struct {
	Name      string              `json:"name"`
	Count     int                 `json:"count"`
	Accounts  []remoteRESTAccount `json:"accounts"`
	URL       string              `json:"url"`
	StaticURL string              `json:"static_url"`
}

// FetchStatusInteractionAccounts requests Mastodon's public interaction API
// from the status's origin instance. Cached accounts are reused; previously
// unknown accounts are represented from the complete REST account payload so
// they can be shown immediately without dozens of serial actor fetches.
func (f *Federator) FetchStatusInteractionAccounts(ctx context.Context, statusURI, statusURL, collection string) ([]*models.Account, error) {
	if collection != "favourited_by" && collection != "reblogged_by" {
		return nil, fmt.Errorf("unsupported interaction collection %q", collection)
	}
	endpoint, originHost, err := statusInteractionEndpoint(statusURI, statusURL, collection)
	if err != nil {
		return nil, err
	}

	var refs []remoteRESTAccount
	if err := f.fetchRESTJSON(ctx, endpoint, &refs); err != nil {
		return nil, err
	}
	accounts := make([]*models.Account, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ctx.Err() != nil || len(accounts) >= maxInteractionActors {
			break
		}
		account := f.cachedRESTAccount(ctx, ref, originHost)
		if account == nil {
			account = restAccountModel(ref, originHost)
		}
		key := interactionAccountKey(account)
		if account != nil && key != "" && !seen[key] {
			seen[key] = true
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (f *Federator) FetchStatusReactions(ctx context.Context, statusURI, statusURL string) ([]models.EmojiReactionSummary, error) {
	endpoint, originHost, err := statusReactionEndpoint(statusURI, statusURL)
	if err != nil {
		return nil, err
	}
	var refs []remoteRESTReaction
	if err := f.fetchRESTJSON(ctx, endpoint, &refs); err != nil {
		return nil, err
	}
	result := make([]models.EmojiReactionSummary, 0, len(refs))
	for _, ref := range refs {
		if ref.Name == "" || ref.Count < 1 {
			continue
		}
		summary := models.EmojiReactionSummary{Name: ref.Name, Key: ref.Name, Count: ref.Count}
		if ref.URL != "" {
			domain := originHost
			shortcode := strings.Trim(ref.Name, ":")
			if parts := strings.SplitN(shortcode, "@", 2); len(parts) == 2 {
				shortcode, domain = parts[0], parts[1]
			}
			summary.Name = ":" + shortcode + "@" + domain + ":"
			if ref.StaticURL == "" {
				ref.StaticURL = ref.URL
			}
			summary.Emoji = &models.CustomEmoji{Shortcode: shortcode, Domain: &domain, URL: ref.URL, StaticURL: ref.StaticURL}
		}
		for _, accountRef := range ref.Accounts {
			account := f.cachedRESTAccount(ctx, accountRef, originHost)
			if account == nil {
				account = restAccountModel(accountRef, originHost)
			}
			if account != nil {
				summary.Accounts = mergeInteractionAccounts(summary.Accounts, []*models.Account{account})
			}
		}
		result = append(result, summary)
	}
	return result, nil
}

func restAccountModel(ref remoteRESTAccount, originHost string) *models.Account {
	handle := strings.TrimPrefix(ref.Acct, "@")
	parts := strings.SplitN(handle, "@", 2)
	domain := originHost
	if len(parts) == 2 && parts[1] != "" {
		domain = parts[1]
	}
	username := ref.Username
	if username == "" && len(parts) > 0 {
		username = parts[0]
	}
	if username == "" || ref.URL == "" {
		return nil
	}
	createdAt := time.Time{}
	if parsed, err := time.Parse(time.RFC3339, ref.CreatedAt); err == nil {
		createdAt = parsed
	}
	return &models.Account{
		ID:                        "remote-rest:" + originHost + ":" + ref.ID,
		Username:                  username,
		Domain:                    &domain,
		ActorURI:                  ref.URL,
		DisplayName:               ref.DisplayName,
		Note:                      ref.Note,
		AvatarRemote:              ref.Avatar,
		HeaderRemote:              ref.Header,
		Locked:                    ref.Locked,
		ManuallyApprovesFollowers: ref.Locked,
		CreatedAt:                 createdAt,
		FollowersCount:            ref.FollowersCount,
		FollowingCount:            ref.FollowingCount,
		StatusesCount:             ref.StatusesCount,
	}
}

func (f *Federator) cachedRESTAccount(ctx context.Context, ref remoteRESTAccount, originHost string) *models.Account {
	handle := strings.TrimPrefix(ref.Acct, "@")
	if handle == "" {
		handle = ref.Username
	}
	parts := strings.SplitN(handle, "@", 2)
	username := parts[0]
	domain := originHost
	if len(parts) == 2 && parts[1] != "" {
		domain = parts[1]
	}
	if username == "" {
		return nil
	}
	if strings.EqualFold(domain, f.domain) {
		account, _ := f.store.GetAccountByUsernameDomain(ctx, username, nil)
		return account
	}
	account, _ := f.store.GetAccountByUsernameDomain(ctx, username, &domain)
	return account
}

func statusInteractionEndpoint(statusURI, statusURL, collection string) (endpoint, originHost string, err error) {
	statusID, u, err := originStatusID(statusURI, statusURL)
	if err != nil {
		return "", "", err
	}
	endpointURL := &url.URL{
		Scheme:   u.Scheme,
		Host:     u.Host,
		Path:     "/api/v1/statuses/" + statusID + "/" + collection,
		RawQuery: "limit=80",
	}
	return endpointURL.String(), u.Hostname(), nil
}

func statusReactionEndpoint(statusURI, statusURL string) (endpoint, originHost string, err error) {
	statusID, u, err := originStatusID(statusURI, statusURL)
	if err != nil {
		return "", "", err
	}
	endpointURL := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/api/v1/pleroma/statuses/" + statusID + "/reactions"}
	return endpointURL.String(), u.Hostname(), nil
}

func originStatusID(statusURI, statusURL string) (string, *url.URL, error) {
	u, err := url.Parse(statusURI)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", nil, fmt.Errorf("invalid status URI")
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	statusID := ""
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == "statuses" && i+1 < len(parts) {
			statusID, err = url.PathUnescape(parts[i+1])
			break
		}
	}
	if statusID == "" && statusURL != "" {
		if publicURL, parseErr := url.Parse(statusURL); parseErr == nil && publicURL.Host == u.Host {
			publicParts := strings.Split(strings.Trim(publicURL.EscapedPath(), "/"), "/")
			if len(publicParts) > 0 {
				statusID, err = url.PathUnescape(publicParts[len(publicParts)-1])
			}
		}
	}
	if err != nil || statusID == "" || strings.Contains(statusID, "/") {
		return "", nil, fmt.Errorf("status URI has no origin status ID")
	}
	return statusID, u, nil
}

func (f *Federator) fetchRESTJSON(ctx context.Context, endpoint string, target interface{}) error {
	if err := f.checkFederationURI(ctx, endpoint); err != nil {
		return err
	}
	if err := validateURL(endpoint); err != nil {
		return err
	}
	cacheKey := "remote:rest:" + endpoint
	if f.fetchCache != nil {
		if v, ok := f.fetchCache.Get(cacheKey); ok {
			cached := v.(cachedRemoteFetch)
			if cached.status != http.StatusOK {
				return &RemoteFetchError{URI: endpoint, StatusCode: cached.status}
			}
			return json.Unmarshal(cached.body, target)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if f.fetchCache != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			f.fetchCache.Set(cacheKey, cachedRemoteFetch{status: resp.StatusCode}, len(cacheKey)+64, f.negativeTTL)
		}
		return &RemoteFetchError{URI: endpoint, StatusCode: resp.StatusCode}
	}
	maxBytes := int64(1 << 20)
	if f.fetchMaxBytes > 0 && f.fetchMaxBytes < maxBytes {
		maxBytes = f.fetchMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > maxBytes {
		return fmt.Errorf("remote interaction response exceeds %d bytes", maxBytes)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode remote interaction response: %w", err)
	}
	if f.fetchCache != nil {
		f.fetchCache.Set(cacheKey, cachedRemoteFetch{body: body, status: http.StatusOK}, len(cacheKey)+len(body)+64, f.fetchTTL)
	}
	return nil
}

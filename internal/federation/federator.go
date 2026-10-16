package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mail"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/mesh"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/netutil"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// Federator handles outbound federation: fetching remote actors, objects,
// and delivering activities to remote inboxes.
type Federator struct {
	store       storage.Storage
	domain      string             // local account/handle domain (legacy field name)
	webDomain   string             // host used by local ActivityPub identifiers
	signer      *Signer            // instance actor signer for signed fetches
	userSigners map[string]*Signer // cached user signers by actor URI, guarded by signerMu
	signerMu    sync.RWMutex
	client      *http.Client
	// baseTransport is the public-internet transport that sits underneath the
	// mesh-aware wrapper in client.Transport. Tuning knobs apply to it, since
	// the wrapper is only a router.
	baseTransport *http.Transport
	// outbound is baseTransport with the IPv4 fallback around it, and is what
	// the mesh wrapper has to sit on top of.
	outbound http.RoundTripper
	// meshNode overrides the process-wide mesh node; nil means use that one.
	meshNode          *mesh.Node
	userAgent         string
	staleActorRefresh func(context.Context, *models.Account) bool
	accountUpdated    func(string)
	fetchCache        *cache.MemoryCache
	fetchTTL          time.Duration
	discoveryTTL      time.Duration
	negativeTTL       time.Duration
	fetchMaxBytes     int64
	fetchMu           sync.Mutex
	fetchInflight     map[string]*remoteFetchCall
	// instanceMemo remembers domains known to have an Instance record so
	// EnsureInstance (called per inbound activity and per delivery) skips the
	// storage read. TTL-bounded; a record deleted by an admin is recreated at
	// most instanceMemoTTL late, which is harmless.
	instanceMemoMu sync.Mutex
	instanceMemo   map[string]time.Time
}

type federationTransport struct {
	federator *Federator
	next      http.RoundTripper
}

type federationRequestContextKey uint8

const (
	federationPolicyBypass federationRequestContextKey = iota
	federationRedirectSigner
	federationRedirectBody
)

func (t federationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Context().Value(federationPolicyBypass) == nil {
		if err := t.federator.checkFederationURI(req.Context(), req.URL.String()); err != nil {
			return nil, err
		}
	} else if err := validateURL(req.URL.String()); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

type deliveryStatusError struct {
	inbox      string
	statusCode int
}

var errDeliveryRedirect = errors.New("delivery redirect refused")

func (e *deliveryStatusError) Error() string {
	return fmt.Sprintf("deliver to %s: unexpected status %d", e.inbox, e.statusCode)
}

func (e *deliveryStatusError) permanent() bool {
	return e.statusCode >= 400 && e.statusCode < 500 &&
		e.statusCode != http.StatusRequestTimeout && e.statusCode != http.StatusTooManyRequests
}

const (
	instanceMemoTTL = 5 * time.Minute
	instanceMemoMax = 1024
)

type remoteFetchCall struct {
	done chan struct{}
	doc  map[string]interface{}
	body []byte
	err  error
}
type cachedRemoteFetch struct {
	body   []byte
	status int
}

// SetStaleActorRefresh wires the non-blocking refresh scheduler without making
// normal federation fetches depend on the queue package.
func (f *Federator) SetStaleActorRefresh(fn func(context.Context, *models.Account) bool) {
	f.staleActorRefresh = fn
}

// SetAccountUpdateHook registers a callback after a remote actor is saved.
func (f *Federator) SetAccountUpdateHook(fn func(string)) {
	f.accountUpdated = fn
}

// RevalidateActorAsync schedules a stale cached actor and never performs
// remote I/O in the caller's request path.
func (f *Federator) RevalidateActorAsync(ctx context.Context, account *models.Account) bool {
	return f.staleActorRefresh != nil && f.staleActorRefresh(ctx, account)
}

// NewFederator creates a new Federator.
func NewFederator(store storage.Storage, domain string, signer *Signer, userAgent string) *Federator {
	return NewFederatorWithDomains(store, domain, domain, signer, userAgent)
}

// NewFederatorWithDomains creates a Federator whose account handles and
// ActivityPub resources may use different domains.
func NewFederatorWithDomains(store storage.Storage, localDomain, webDomain string, signer *Signer, userAgent string) *Federator {
	if signer != nil {
		signer.SetUserAgent(userAgent)
	}
	// IP-level SSRF guard on the address actually being dialed: one DNS
	// resolution (no separate LookupHost pre-check), no resolve/dial TOCTOU
	// window, and it also covers Deliver, whose inbox URLs come from remote
	// actor documents.
	dialer := ssrfGuardedDialer()
	transport := &http.Transport{
		MaxIdleConns: 20,
		// Fan-out concentrates on a few big shared inboxes; the
		// default of 2 idle conns per host forces repeated TLS
		// handshakes, which are the expensive part on slow CPUs.
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
		DisableCompression:  false,
	}
	// A peer whose IPv6 path accepts the connect and then stalls would
	// otherwise fail every fetch and every delivery to it until the timeout,
	// with no second attempt.
	outbound := netutil.WithIPv4Fallback(transport, dialer)
	f := &Federator{
		store:         store,
		domain:        localDomain,
		webDomain:     webDomain,
		signer:        signer,
		userSigners:   make(map[string]*Signer),
		userAgent:     userAgent,
		baseTransport: transport,
		outbound:      outbound,
		fetchInflight: make(map[string]*remoteFetchCall),
	}
	f.client = &http.Client{
		Timeout:       30 * time.Second,
		Transport:     federationTransport{federator: f, next: mesh.WrapDefault(outbound)},
		CheckRedirect: f.checkRedirect,
	}
	return f
}

func (f *Federator) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 5 {
		return errors.New("too many redirects")
	}
	bypass := req.Context().Value(federationPolicyBypass) != nil
	if bypass {
		if err := validateURL(req.URL.String()); err != nil {
			return err
		}
	} else if err := f.checkFederationURI(req.Context(), req.URL.String()); err != nil {
		return err
	}

	status := 0
	if req.Response != nil {
		status = req.Response.StatusCode
	}
	if len(via) > 0 && via[len(via)-1].Method == http.MethodPost &&
		(status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther) {
		return fmt.Errorf("%w: status %d", errDeliveryRedirect, status)
	}
	switch req.Method {
	case http.MethodGet:
		if f.signer == nil {
			return nil
		}
		req.Header.Del("Signature")
		req.Header.Del("Host")
		return f.signer.SignRequest(req, nil)
	case http.MethodPost:
		if status != http.StatusTemporaryRedirect && status != http.StatusPermanentRedirect {
			return fmt.Errorf("%w: status %d", errDeliveryRedirect, status)
		}
		signer, _ := req.Context().Value(federationRedirectSigner).(*Signer)
		if signer == nil {
			return nil
		}
		body, _ := req.Context().Value(federationRedirectBody).([]byte)
		req.Header.Del("Signature")
		req.Header.Del("Host")
		req.Header.Del("Digest")
		return signer.SignRequest(req, body)
	default:
		return http.ErrUseLastResponse
	}
}

// SetTransportLimits overrides the outbound connection-pool sizes. Call it
// right after construction, before any request is made; values <= 0 keep the
// built-in defaults. The IPv4 retry path keeps the sizes it was built with,
// which is what we want: it only carries the few hosts whose IPv6 is broken.
func (f *Federator) SetTransportLimits(maxIdleConns, maxIdleConnsPerHost int) {
	if f.baseTransport == nil {
		return
	}
	if maxIdleConns > 0 {
		f.baseTransport.MaxIdleConns = maxIdleConns
	}
	if maxIdleConnsPerHost > 0 {
		f.baseTransport.MaxIdleConnsPerHost = maxIdleConnsPerHost
	}
}

// SetMeshNode routes mesh hostnames over a specific node instead of the
// process-wide one. An instance never needs this - it has exactly one node -
// but a process hosting several instances at once does.
func (f *Federator) SetMeshNode(node *mesh.Node) {
	f.meshNode = node
	f.client.Transport = federationTransport{federator: f, next: mesh.WrapTransport(f.outbound, node)}
}

// ssrfGuardedDialer returns a dialer whose Control hook rejects private,
// loopback, link-local, and other non-routable destination IPs at connect
// time. Because it inspects the literal address being dialed (after DNS
// resolution), a hostname that resolves to a private IP (or a DNS answer
// that changes between check and connect) can never reach it.
func ssrfGuardedDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("ssrf guard: split %s: %w", address, err)
			}
			if isPrivateIP(host) {
				return fmt.Errorf("refusing to connect to private IP %s", host)
			}
			return nil
		},
	}
}

// SetFetchCache enables bounded caching and miss coalescing for safe outbound
// federation GETs. Deliveries and all non-GET traffic bypass it.
func (f *Federator) SetFetchCache(c *cache.MemoryCache, ttl, discoveryTTL, negativeTTL time.Duration, maxBytes int) {
	f.fetchCache, f.fetchTTL, f.discoveryTTL, f.negativeTTL = c, ttl, discoveryTTL, negativeTTL
	if maxBytes > 0 {
		f.fetchMaxBytes = int64(maxBytes)
	}
}

func (f *Federator) isLocalDomain(domain string) bool {
	return strings.EqualFold(domain, f.domain) || strings.EqualFold(domain, f.webDomain)
}

// RemoteFetchError preserves HTTP status information for failed signed fetches.
type RemoteFetchError struct {
	URI        string
	StatusCode int
}

// FederationPolicyError is permanent: retries cannot change an admin policy.
type FederationPolicyError struct {
	Domain string
	Reason string
}

func (e *FederationPolicyError) Error() string {
	return fmt.Sprintf("federation with %s is blocked (%s)", e.Domain, e.Reason)
}

func (e *FederationPolicyError) Unwrap() error { return storage.ErrFederationBlocked }

func (e *RemoteFetchError) Error() string {
	return fmt.Sprintf("fetch %s: status %d", e.URI, e.StatusCode)
}

func isPermanentRemoteFetchError(err error) bool {
	if errors.Is(err, storage.ErrFederationBlocked) {
		return true
	}
	var fetchErr *RemoteFetchError
	if !errors.As(err, &fetchErr) {
		return false
	}
	switch fetchErr.StatusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusUnavailableForLegalReasons:
		return true
	default:
		return false
	}
}

func (f *Federator) checkFederationURI(ctx context.Context, rawURI string) error {
	if err := validateURL(rawURI); err != nil {
		return fmt.Errorf("invalid federation URL %q: %w", rawURI, err)
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return fmt.Errorf("invalid federation URL %q", rawURI)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if f.isLocalDomain(host) {
		return nil
	}
	if f.store == nil {
		return nil
	}
	allowed, reason, err := f.store.FederationDomainAllowed(ctx, host)
	if err != nil {
		return fmt.Errorf("load federation policy: %w", err)
	}
	if !allowed {
		metrics.IncrementFederationPolicyRejected("outbound", reason)
		return &FederationPolicyError{Domain: host, Reason: reason}
	}
	return nil
}

// knownInstance reports whether the domain was recently confirmed to have an
// Instance record.
func (f *Federator) knownInstance(domain string, now time.Time) bool {
	f.instanceMemoMu.Lock()
	defer f.instanceMemoMu.Unlock()
	seen, ok := f.instanceMemo[domain]
	return ok && now.Sub(seen) <= instanceMemoTTL
}

func (f *Federator) rememberInstance(domain string, now time.Time) {
	f.instanceMemoMu.Lock()
	defer f.instanceMemoMu.Unlock()
	if f.instanceMemo == nil {
		f.instanceMemo = make(map[string]time.Time)
	}
	if len(f.instanceMemo) >= instanceMemoMax {
		for d, seen := range f.instanceMemo {
			if now.Sub(seen) > instanceMemoTTL {
				delete(f.instanceMemo, d)
			}
		}
		// Still full of fresh entries: drop the map rather than grow unbounded.
		if len(f.instanceMemo) >= instanceMemoMax {
			f.instanceMemo = make(map[string]time.Time)
		}
	}
	f.instanceMemo[domain] = now
}

// EnsureInstanceExists guarantees an Instance record for the domain, with the
// existence check memoised. The inbound-activity and delivery hot paths call
// this once per activity and never use the record itself, so a memo hit skips
// the storage read entirely.
func (f *Federator) EnsureInstanceExists(ctx context.Context, domain string) error {
	if f.knownInstance(domain, time.Now()) {
		return nil
	}
	_, err := f.EnsureInstance(ctx, domain)
	return err
}

// EnsureInstance creates an Instance record for the given domain if one doesn't
// already exist. It is called whenever we first contact a remote domain.
func (f *Federator) EnsureInstance(ctx context.Context, domain string) (*models.Instance, error) {
	now := time.Now()
	instance, err := f.store.GetInstance(ctx, domain)
	if err == nil {
		f.rememberInstance(domain, now)
		return instance, nil
	}

	// Create a new instance record
	instance = &models.Instance{
		Domain:    domain,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := f.store.CreateInstance(ctx, instance); err != nil {
		return nil, fmt.Errorf("create instance record for %s: %w", domain, err)
	}
	f.rememberInstance(domain, now)

	slog.Info("created instance record", "domain", domain)

	// Best-effort fetch NodeInfo to populate software/version
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if software, version, nodeInfoURL := f.fetchNodeInfo(ctx, domain); software != "" {
			instance.Software = software
			instance.Version = version
			instance.NodeInfoURL = nodeInfoURL
			if err := f.store.UpdateInstance(ctx, instance); err != nil {
				slog.Warn("failed to update instance with NodeInfo", "domain", domain, "error", err)
			}
		}
	}()

	return instance, nil
}

// FetchActor fetches a remote actor document, parses it, and stores/updates it.
func (f *Federator) FetchActor(ctx context.Context, actorURI string) (*models.Account, error) {
	return f.fetchActor(ctx, actorURI, true)
}

// FetchActorLight fetches and stores an actor without the optional REST and
// collection-count enrichment. Thread completion uses it to stay within its
// small request-wide latency budget, and the inbox uses it for every inbound
// activity (counts are refreshed by the ActorRefresher queue instead - paying
// several signed HTTPS fetches per routine delivery overwhelmed low-power
// hardware).
func (f *Federator) FetchActorLight(ctx context.Context, actorURI string) (*models.Account, error) {
	return f.fetchActor(ctx, actorURI, false)
}

type ActorRefreshOptions struct {
	Force        bool
	EnrichCounts bool
}

// RefreshActor makes cache bypass and count enrichment explicit.
func (f *Federator) RefreshActor(ctx context.Context, actorURI string, opts ActorRefreshOptions) (*models.Account, error) {
	return f.fetchActorWithOptions(ctx, actorURI, opts.EnrichCounts, opts.Force)
}

func (f *Federator) fetchActor(ctx context.Context, actorURI string, enrichCounts bool) (*models.Account, error) {
	return f.fetchActorWithOptions(ctx, actorURI, enrichCounts, false)
}

func (f *Federator) fetchActorWithOptions(ctx context.Context, actorURI string, enrichCounts, force bool) (*models.Account, error) {
	if err := f.checkFederationURI(ctx, actorURI); err != nil {
		return nil, err
	}
	// Check cache first
	account, err := f.store.GetAccountByURI(ctx, actorURI)
	if err == nil && !force {
		// If recently fetched, return cached
		if account.FetchedAt != nil && time.Since(*account.FetchedAt) < 1*time.Hour &&
			(!enrichCounts || account.Domain == nil || account.FollowersCount != 0 || account.FollowingCount != 0 || account.StatusesCount != 0) {
			return account, nil
		}
	}

	// Fetch the ActivityPub actor document
	var doc map[string]interface{}
	if force {
		var body []byte
		doc, body, err = f.signedFetchFresh(ctx, actorURI)
		if err == nil {
			if body != nil {
				f.storeRemoteDocumentBytes(actorURI, body, f.fetchTTL)
			} else {
				f.storeRemoteDocument(actorURI, doc, f.fetchTTL)
			}
		}
	} else {
		doc, err = f.signedFetch(ctx, actorURI)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch actor %s: %w", actorURI, err)
	}
	actorDocHasCounts := docHasAnyCountField(doc)

	actor, err := f.parseAndStoreActor(ctx, doc, actorURI)
	if err != nil {
		return nil, fmt.Errorf("parse actor %s: %w", actorURI, err)
	}

	// If the ActivityPub document didn't include follower/following/status
	// counts (common with Mastodon, which only provides these via its REST
	// API), try fetching the REST representation as a fallback.
	// Use a short timeout so we don't block the caller for too long;
	// the profile page has a fallback to local counts if this fails.
	if enrichCounts && !actorDocHasCounts {
		f.fetchAndMergeRESTCounts(ctx, actor)
	}
	if enrichCounts {
		f.fetchAndMergeCollectionCounts(ctx, actor)
	}

	return actor, nil
}

func (f *Federator) storeRemoteDocument(uri string, doc map[string]interface{}, ttl time.Duration) {
	if f.fetchCache == nil || doc == nil {
		return
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return
	}
	f.storeRemoteDocumentBytes(uri, body, ttl)
}

// storeRemoteDocumentBytes caches an already-serialized fetch response,
// skipping the re-marshal when the raw bytes are still in hand.
func (f *Federator) storeRemoteDocumentBytes(uri string, body []byte, ttl time.Duration) {
	if f.fetchCache == nil || body == nil {
		return
	}
	if f.fetchMaxBytes > 0 && int64(len(body)) > f.fetchMaxBytes {
		return
	}
	key := "remote:ap:" + uri
	f.fetchCache.Set(key, cachedRemoteFetch{body: body, status: http.StatusOK}, len(key)+len(body)+64, ttl)
}

// FetchObject fetches a remote ActivityPub object by URI.
func (f *Federator) FetchObject(ctx context.Context, objectURI string) (map[string]interface{}, error) {
	doc, err := f.signedFetch(ctx, objectURI)
	if err != nil {
		return nil, fmt.Errorf("fetch object %s: %w", objectURI, err)
	}

	return doc, nil
}

// fetchAndMergeRESTCounts attempts to fetch the Mastodon-compatible REST API
// representation of an actor to obtain follower/following/status counts.
// Mastodon's ActivityPub actor document omits these counts; they are only
// available via the REST API or by requesting the actor URL with
// Accept: application/json. This method does a best-effort fetch and
// updates the account record if it gets non-zero counts.
func (f *Federator) fetchAndMergeRESTCounts(ctx context.Context, account *models.Account) {
	actorURI := account.ActorURI

	// Create a short-lived context to avoid blocking the caller
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", actorURI, nil)
	if err != nil {
		return
	}
	// Request the JSON REST representation (not ActivityPub). Mastodon and
	// many other Fediverse servers use content negotiation: application/json
	// returns the REST API account object which includes counts.
	req.Header.Set("Accept", "application/json")
	if f.signer != nil {
		if err := f.signer.SignRequest(req, nil); err != nil {
			slog.Debug("federator: failed to sign REST count request", "error", err)
		}
	}

	resp, err := f.client.Do(req)
	if err != nil {
		slog.Debug("federator: REST count fetch failed", "uri", actorURI, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	maxBytes := int64(512 << 10)
	if f.fetchMaxBytes > 0 && f.fetchMaxBytes < maxBytes {
		maxBytes = f.fetchMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		slog.Debug("federator: oversized REST response", "uri", actorURI)
		return
	}
	var restDoc map[string]interface{}
	if err := json.Unmarshal(body, &restDoc); err != nil {
		slog.Debug("federator: failed to decode REST response", "uri", actorURI, "error", err)
		return
	}

	// Mastodon REST API uses snake_case: followers_count, following_count,
	// statuses_count. Try both snake_case and camelCase for compatibility.
	followersCount, hasFollowersCount := extractIntFromDoc(restDoc, "followers_count")
	if !hasFollowersCount {
		followersCount, hasFollowersCount = extractIntFromDoc(restDoc, "followersCount")
	}
	followingCount, hasFollowingCount := extractIntFromDoc(restDoc, "following_count")
	if !hasFollowingCount {
		followingCount, hasFollowingCount = extractIntFromDoc(restDoc, "followingCount")
	}
	statusesCount, hasStatusesCount := extractIntFromDoc(restDoc, "statuses_count")
	if !hasStatusesCount {
		statusesCount, hasStatusesCount = extractIntFromDoc(restDoc, "statusesCount")
	}

	// If the response explicitly included any count, merge only those fields.
	if hasFollowersCount || hasFollowingCount || hasStatusesCount {
		now := time.Now()
		if hasFollowersCount {
			account.FollowersCount = followersCount
		}
		if hasFollowingCount {
			account.FollowingCount = followingCount
		}
		if hasStatusesCount {
			account.StatusesCount = statusesCount
		}
		account.CountsFetchedAt = &now
		if err := f.store.UpdateAccount(ctx, account); err != nil {
			slog.Warn("federator: failed to update account with REST counts",
				"uri", actorURI, "error", err)
		} else {
			slog.Info("federator: merged REST counts for remote account",
				"uri", actorURI,
				"followers", followersCount,
				"following", followingCount,
				"statuses", statusesCount,
			)
		}
	}
}

// fetchAndMergeCollectionCounts fetches ActivityPub collection summaries for
// servers that expose profile counts as totalItems on followers/following/outbox
// instead of as actor or REST account fields.
func (f *Federator) fetchAndMergeCollectionCounts(ctx context.Context, account *models.Account) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	type countResult struct {
		field string
		count int
		ok    bool
	}
	results := make(chan countResult, 3)
	var wait sync.WaitGroup
	fetch := func(field, uri string) {
		if uri == "" {
			return
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			count, ok := f.fetchCollectionTotalItems(ctx, uri)
			results <- countResult{field: field, count: count, ok: ok}
		}()
	}
	fetch("followers", account.FollowersURI)
	fetch("following", account.FollowingURI)
	fetch("statuses", account.OutboxURI)
	wait.Wait()
	close(results)

	changed := false
	for result := range results {
		if !result.ok {
			continue
		}
		switch result.field {
		case "followers":
			account.FollowersCount = result.count
		case "following":
			account.FollowingCount = result.count
		case "statuses":
			account.StatusesCount = result.count
		}
		changed = true
	}
	if !changed {
		return
	}
	now := time.Now()
	account.CountsFetchedAt = &now
	if err := f.store.UpdateAccount(ctx, account); err != nil {
		slog.Warn("federator: failed to update account with collection counts",
			"uri", account.ActorURI, "error", err)
		return
	}
	slog.Info("federator: merged collection counts for remote account",
		"uri", account.ActorURI,
		"followers", account.FollowersCount,
		"following", account.FollowingCount,
		"statuses", account.StatusesCount,
	)
}

func (f *Federator) fetchCollectionTotalItems(ctx context.Context, collectionURI string) (int, bool) {
	doc, err := f.signedFetch(ctx, collectionURI)
	if err != nil {
		slog.Debug("federator: collection count fetch failed", "uri", collectionURI, "error", err)
		return 0, false
	}
	count, ok := extractIntFromDoc(doc, "totalItems")
	if !ok {
		slog.Debug("federator: collection count missing totalItems", "uri", collectionURI)
		return 0, false
	}
	return count, true
}

// Deliver sends an activity to a remote inbox.
// Signing key selection: if the activity has an actor field pointing to a local
// user, the delivery is signed with that user's key. Otherwise the instance
// actor key is used (e.g. for signed fetches).
func (f *Federator) Deliver(ctx context.Context, inboxURI string, activity map[string]interface{}) error {
	if signed, err := maybeSignWithOIP(ctx, f.store, activity); err != nil {
		slog.Warn("delivery: OIP signing failed, falling back to HTTP signature", "inbox", inboxURI, "error", err)
	} else {
		activity = signed
	}
	body, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("marshal activity: %w", err)
	}
	actorURI, _ := activity["actor"].(string)
	return f.DeliverRaw(ctx, inboxURI, body, actorURI)
}

// DeliverRaw sends an already-serialized activity to a remote inbox. The
// delivery queue stores activities as raw JSON, so this path avoids the
// unmarshal + re-marshal round trip per destination inbox. actorURI selects
// the signing key ("" falls back to the instance actor).
func (f *Federator) DeliverRaw(ctx context.Context, inboxURI string, body []byte, actorURI string) (retErr error) {
	return f.deliverRaw(ctx, inboxURI, body, actorURI, false)
}

// DeliverRemovalRaw bypasses federation policy only for a pre-recorded
// account-removal destination. Callers must not use it for discovery.
func (f *Federator) DeliverRemovalRaw(ctx context.Context, inboxURI string, body []byte, actorURI string) error {
	return f.deliverRaw(ctx, inboxURI, body, actorURI, true)
}

func (f *Federator) deliverRaw(ctx context.Context, inboxURI string, body []byte, actorURI string, policyBypass bool) (retErr error) {
	if !policyBypass {
		if err := f.checkFederationURI(ctx, inboxURI); err != nil {
			return err
		}
	}
	metrics.IncrementDeliveriesAttempted()
	defer func() {
		if retErr == nil {
			metrics.IncrementDeliveriesSucceeded()
		} else if ctx.Err() == nil {
			metrics.IncrementDeliveriesFailed()
		}
	}()

	// Extract domain for instance health tracking
	domain := ""
	if parsed, err := url.Parse(inboxURI); err == nil {
		domain = parsed.Hostname()
	}

	// Instance health has to be recorded on a context that outlives the
	// delivery. A preemptible worker is cancelled mid-POST when a browser
	// request arrives, and writing the verdict through the same dead context
	// fails, so the breaker never arms and the job comes straight back.
	healthCtx := context.WithoutCancel(ctx)

	req, err := http.NewRequestWithContext(ctx, "POST", inboxURI, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/activity+json; charset=utf-8")

	signerStarted := time.Now()
	signer := f.signer // default: instance actor
	if actorURI != "" {
		if s, err := f.getActorSigner(ctx, actorURI); err == nil {
			signer = s
		}
	}
	metrics.ObserveDeliveryStage("signer", time.Since(signerStarted))

	if signer != nil {
		signStarted := time.Now()
		if err := signer.SignRequest(req, body); err != nil {
			metrics.ObserveDeliveryStage("sign", time.Since(signStarted))
			return fmt.Errorf("sign request: %w", err)
		}
		metrics.ObserveDeliveryStage("sign", time.Since(signStarted))
	}
	ctx = context.WithValue(ctx, federationRedirectBody, body)
	if signer != nil {
		ctx = context.WithValue(ctx, federationRedirectSigner, signer)
	}
	if policyBypass {
		ctx = context.WithValue(ctx, federationPolicyBypass, true)
	}
	req = req.WithContext(ctx)

	httpStarted := time.Now()
	resp, err := f.client.Do(req)
	metrics.ObserveDeliveryStage("http", time.Since(httpStarted))
	if err != nil {
		// Our own cancellation says nothing about the remote host: the worker
		// yielded to foreground traffic before the POST could finish. Blaming
		// the destination here marks healthy instances unreachable on a busy
		// single-core box.
		if ctx.Err() != nil {
			return fmt.Errorf("deliver to %s: %w", inboxURI, err)
		}
		if errors.Is(err, storage.ErrFederationBlocked) || errors.Is(err, errDeliveryRedirect) {
			return err
		}
		// Network error - mark instance unreachable
		if domain != "" && !f.isLocalDomain(domain) {
			if justBecame, markErr := f.store.MarkInstanceUnreachable(healthCtx, domain); markErr != nil {
				slog.Warn("failed to mark instance unreachable", "domain", domain, "error", markErr)
			} else if justBecame {
				mail.NotifyAdminsInstanceUnreachable(healthCtx, f.store, f.webDomain, domain)
			}
		}
		return fmt.Errorf("deliver to %s: %w", inboxURI, err)
	}
	defer resp.Body.Close()

	// Drain only a small prefix. A malicious inbox must not make a worker spend
	// unbounded bandwidth after the delivery result is already known.
	bodyStarted := time.Now()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	metrics.ObserveDeliveryStage("response_body", time.Since(bodyStarted))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Success - mark instance reachable
		if domain != "" && !f.isLocalDomain(domain) {
			if err := f.store.MarkInstanceReachable(healthCtx, domain); err != nil {
				slog.Warn("failed to mark instance reachable", "domain", domain, "error", err)
			}
		}
		return nil
	}

	statusErr := &deliveryStatusError{inbox: inboxURI, statusCode: resp.StatusCode}
	if !statusErr.permanent() && domain != "" && !f.isLocalDomain(domain) {
		if justBecame, markErr := f.store.MarkInstanceUnreachable(healthCtx, domain); markErr != nil {
			slog.Warn("failed to mark instance unreachable", "domain", domain, "error", markErr)
		} else if justBecame {
			mail.NotifyAdminsInstanceUnreachable(healthCtx, f.store, f.webDomain, domain)
		}
	}

	return statusErr
}

// FetchOutboxPage fetches a single page of a remote outbox collection and
// returns the raw orderedItems. The outboxPageURL should point to an
// OrderedCollectionPage (e.g. the "first" link of the outbox collection).
// maxItems limits how many items are returned from the page.
func (f *Federator) FetchOutboxPage(ctx context.Context, outboxPageURL string, maxItems int) ([]map[string]interface{}, error) {
	doc, err := f.signedFetch(ctx, outboxPageURL)
	if err != nil {
		return nil, fmt.Errorf("fetch outbox page %s: %w", outboxPageURL, err)
	}

	colType, _ := doc["type"].(string)

	// Handle OrderedCollection (not a page) by following the "first" link
	if colType == "OrderedCollection" {
		first, _ := doc["first"].(string)
		if first != "" {
			return f.FetchOutboxPage(ctx, first, maxItems)
		}
		// Fallback: try to read orderedItems directly
		if items, ok := doc["orderedItems"].([]interface{}); ok {
			return extractOrderedItems(items, maxItems), nil
		}
		return nil, fmt.Errorf("outbox collection has no first link and no orderedItems")
	}

	if colType != "OrderedCollectionPage" {
		return nil, fmt.Errorf("unexpected outbox collection type: %s", colType)
	}

	items, ok := doc["orderedItems"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("outbox page has no orderedItems")
	}

	return extractOrderedItems(items, maxItems), nil
}

// extractOrderedItems converts an []interface{} of orderedItems into a slice of maps.
func extractOrderedItems(items []interface{}, maxItems int) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
			if len(result) >= maxItems {
				break
			}
		}
	}
	return result
}

// FetchOutboxFirst fetches the first page of a remote outbox.
// It first fetches the outbox collection to find the "first" page link,
// then fetches that page. maxItems limits the returned items.
func (f *Federator) FetchOutboxFirst(ctx context.Context, outboxURI string, maxItems int) ([]map[string]interface{}, error) {
	doc, err := f.signedFetch(ctx, outboxURI)
	if err != nil {
		return nil, fmt.Errorf("fetch outbox %s: %w", outboxURI, err)
	}

	// If it's a page directly, return its items
	colType, _ := doc["type"].(string)
	if colType == "OrderedCollectionPage" {
		if items, ok := doc["orderedItems"].([]interface{}); ok {
			return extractOrderedItems(items, maxItems), nil
		}
		return nil, fmt.Errorf("outbox page has no orderedItems")
	}

	// It's an OrderedCollection - find the first page. "first" is usually a
	// bare URI, but some servers (e.g. snac) embed the page object inline
	// instead, with its items right there - skip the second fetch when so.
	if firstObj, ok := doc["first"].(map[string]interface{}); ok {
		if items, ok := firstObj["orderedItems"].([]interface{}); ok {
			return extractOrderedItems(items, maxItems), nil
		}
	}
	first := activityStreamsLink(doc["first"])
	if first == "" {
		// Some servers (e.g. snac) put items directly in the top-level
		// orderedItems instead of paginating at all.
		if items, ok := doc["orderedItems"].([]interface{}); ok {
			return extractOrderedItems(items, maxItems), nil
		}
		// Neither a first link, inline items, nor a totalItems hint: this is
		// a bare collection stub (observed on snac - {"@context","id","type"}
		// and nothing else), not a malformed response. Treat it as a
		// legitimately empty outbox rather than an error - erroring here
		// just relabels "nothing to fetch" as a job failure.
		keys := make([]string, 0, len(doc))
		for k := range doc {
			keys = append(keys, k)
		}
		slog.Debug("federator: empty/unrecognized outbox shape, treating as no items", "outbox", outboxURI, "type", colType, "keys", keys)
		return nil, nil
	}

	return f.FetchOutboxPage(ctx, first, maxItems)
}

// FetchCollectionURIs fetches actor URIs from an ActivityStreams collection.
// Unlike outboxes, following collections normally contain bare string IRIs;
// this also accepts embedded objects and follows paginated `first`/`next` links.
func (f *Federator) FetchCollectionURIs(ctx context.Context, collectionURI string, maxItems int) ([]string, error) {
	nextURI := collectionURI
	seenPages := make(map[string]bool)
	seenItems := make(map[string]bool)
	result := make([]string, 0)

	for nextURI != "" && (maxItems <= 0 || len(result) < maxItems) && !seenPages[nextURI] {
		seenPages[nextURI] = true
		doc, err := f.signedFetch(ctx, nextURI)
		if err != nil {
			return nil, fmt.Errorf("fetch collection %s: %w", nextURI, err)
		}

		if first := activityStreamsLink(doc["first"]); first != "" && first != nextURI {
			nextURI = first
			continue
		}

		var rawItems []interface{}
		if items, ok := doc["orderedItems"].([]interface{}); ok {
			rawItems = items
		} else if items, ok := doc["items"].([]interface{}); ok {
			rawItems = items
		}
		for _, item := range rawItems {
			uri := activityStreamsLink(item)
			if uri == "" || seenItems[uri] {
				continue
			}
			seenItems[uri] = true
			result = append(result, uri)
			if maxItems > 0 && len(result) >= maxItems {
				break
			}
		}
		nextURI = activityStreamsLink(doc["next"])
	}

	return result, nil
}

func activityStreamsLink(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]interface{}:
		if id, ok := v["id"].(string); ok {
			return id
		}
	case []interface{}:
		for _, item := range v {
			if uri := activityStreamsLink(item); uri != "" {
				return uri
			}
		}
	}
	return ""
}

func activityStreamsLinks(value interface{}) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []interface{}:
		links := make([]string, 0, len(v))
		seen := map[string]bool{}
		for _, item := range v {
			if uri := activityStreamsLink(item); uri != "" && !seen[uri] {
				seen[uri] = true
				links = append(links, uri)
			}
		}
		return links
	default:
		if uri := activityStreamsLink(value); uri != "" {
			return []string{uri}
		}
	}
	return nil
}

// ResolveAccount resolves an account from an acct URI (acct:user@domain)
// or an actor URI. Fetches and stores the remote actor if needed.
func (f *Federator) ResolveAccount(ctx context.Context, acct string) (*models.Account, error) {
	// acct:user@domain format
	if strings.HasPrefix(acct, "acct:") {
		acct = strings.TrimPrefix(acct, "acct:")
	}

	// If it's a full URI, try to fetch it directly
	if strings.HasPrefix(acct, "http://") || strings.HasPrefix(acct, "https://") {
		return f.FetchActor(ctx, acct)
	}

	// Parse user@domain
	parts := strings.SplitN(acct, "@", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid acct format: %s", acct)
	}

	username := parts[0]
	domain := parts[1]

	// Check local
	if strings.EqualFold(domain, f.domain) {
		return f.store.GetAccountByUsernameDomain(ctx, username, nil)
	}

	// Do WebFinger lookup
	actorURI, handleDomain, err := f.webfingerLookup(ctx, username, domain)
	if err != nil {
		return nil, fmt.Errorf("webfinger lookup %s@%s: %w", username, domain, err)
	}

	account, err := f.FetchActor(ctx, actorURI)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(account.Username, username) {
		return nil, fmt.Errorf("webfinger username %q resolved actor %q", username, account.Username)
	}
	if account.Domain != nil && !strings.EqualFold(*account.Domain, handleDomain) {
		oldDomain := account.Domain
		account.Domain = &handleDomain
		if err := f.store.UpdateAccount(ctx, account); err != nil {
			account.Domain = oldDomain
			return nil, fmt.Errorf("store WebFinger handle domain: %w", err)
		}
	}
	return account, nil
}

// webfingerLookup performs a WebFinger query to find the actor URI for
// username@domain.
func (f *Federator) webfingerLookup(ctx context.Context, username, domain string) (string, string, error) {
	jrd, err := f.webfingerResource(ctx, "acct:"+username+"@"+domain, domain)
	if err != nil {
		return "", "", err
	}
	href := webfingerSelfLink(jrd)
	if href == "" {
		return "", "", fmt.Errorf("webfinger: no self link found")
	}
	handleDomain := strings.ToLower(strings.TrimSuffix(domain, "."))
	if subject, _ := jrd["subject"].(string); subject != "" {
		subjectUser, subjectDomain, ok := parseAcctSubject(subject)
		if ok && strings.EqualFold(subjectUser, username) &&
			strings.EqualFold(strings.TrimSuffix(subjectDomain, "."), handleDomain) {
			handleDomain = strings.ToLower(strings.TrimSuffix(subjectDomain, "."))
		}
	}
	return href, handleDomain, nil
}

// webfingerResource fetches and decodes the WebFinger JRD for an arbitrary
// resource (e.g. "acct:user@host") served at the given domain.
func (f *Federator) webfingerResource(ctx context.Context, resource, domain string) (map[string]interface{}, error) {
	// Reject WebFinger lookups for raw IP addresses to prevent SSRF
	if net.ParseIP(domain) != nil {
		return nil, fmt.Errorf("refusing WebFinger lookup for IP address: %s", domain)
	}
	if err := f.checkFederationURI(ctx, "https://"+domain); err != nil {
		return nil, err
	}

	webfingerURL := fmt.Sprintf("https://%s/.well-known/webfinger?resource=%s", domain, url.QueryEscape(resource))
	cacheKey := "remote:webfinger:" + webfingerURL
	if f.fetchCache != nil {
		if v, ok := f.fetchCache.Get(cacheKey); ok {
			cached := v.(cachedRemoteFetch)
			if cached.status != http.StatusOK {
				return nil, &RemoteFetchError{URI: webfingerURL, StatusCode: cached.status}
			}
			var jrd map[string]interface{}
			if json.Unmarshal(cached.body, &jrd) == nil {
				return jrd, nil
			}
			f.fetchCache.Delete(cacheKey)
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", webfingerURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/jrd+json, application/json")
	if f.signer != nil {
		if err := f.signer.SignRequest(req, nil); err != nil {
			slog.Warn("failed to sign webfinger request", "error", err)
		}
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webfinger request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if f.fetchCache != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			f.fetchCache.Set(cacheKey, cachedRemoteFetch{status: resp.StatusCode}, len(cacheKey)+64, f.negativeTTL)
		}
		return nil, &RemoteFetchError{URI: webfingerURL, StatusCode: resp.StatusCode}
	}

	maxBytes := int64(512 << 10)
	if f.fetchMaxBytes > 0 {
		maxBytes = f.fetchMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read webfinger response: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("webfinger response exceeds %d bytes", maxBytes)
	}
	var jrd map[string]interface{}
	if err := json.Unmarshal(body, &jrd); err != nil {
		return nil, fmt.Errorf("decode webfinger response: %w", err)
	}
	if f.fetchCache != nil {
		f.fetchCache.Set(cacheKey, cachedRemoteFetch{body: append([]byte(nil), body...), status: http.StatusOK}, len(cacheKey)+len(body)+64, f.discoveryTTL)
	}
	return jrd, nil
}

// webfingerSelfLink returns the href of the self link of type
// application/activity+json from a decoded WebFinger JRD, or "" if absent.
func webfingerSelfLink(jrd map[string]interface{}) string {
	links, ok := jrd["links"].([]interface{})
	if !ok {
		return ""
	}
	for _, link := range links {
		l, ok := link.(map[string]interface{})
		if !ok {
			continue
		}
		rel, _ := l["rel"].(string)
		typ, _ := l["type"].(string)
		if rel == "self" && typ == "application/activity+json" {
			if href, _ := l["href"].(string); href != "" {
				return href
			}
		}
	}
	return ""
}

// parseAcctSubject splits a WebFinger subject ("acct:user@domain") into its
// username and domain parts. ok is false if the subject is not a usable acct.
func parseAcctSubject(subject string) (username, domain string, ok bool) {
	s := strings.TrimPrefix(subject, "acct:")
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// canonicalAccountDomain determines the canonical handle domain for a remote
// actor. Some servers serve actor documents from a "web" host (e.g.
// mastodon.example.com) while the canonical fediverse handle uses a different
// "account" domain (e.g. example.com). Treating these as distinct would split
// one person into two accounts, so we discover the real domain by WebFingering
// the actor at its own host, reading the subject, and verifying that the
// canonical domain's WebFinger points back to the same actor URI (guarding
// against a host claiming a handle it does not own). On any uncertainty we fall
// back to the actor host, which is always safe.
func (f *Federator) canonicalAccountDomain(ctx context.Context, username, actorHost, actorURI string) string {
	jrd, err := f.webfingerResource(ctx, "acct:"+username+"@"+actorHost, actorHost)
	if err != nil {
		return actorHost
	}
	subject, _ := jrd["subject"].(string)
	subjUser, subjDomain, ok := parseAcctSubject(subject)
	if !ok || strings.EqualFold(subjDomain, actorHost) {
		// No redirection: the handle domain matches the actor host.
		return actorHost
	}
	// The subject must refer to the same username, otherwise don't trust it.
	if !strings.EqualFold(subjUser, username) {
		return actorHost
	}
	// Verify the canonical domain agrees and resolves back to this actor.
	verify, err := f.webfingerResource(ctx, "acct:"+subjUser+"@"+subjDomain, subjDomain)
	if err != nil {
		return actorHost
	}
	if !strings.EqualFold(webfingerSelfLink(verify), actorURI) {
		return actorHost
	}
	return subjDomain
}

// signedFetch makes a signed GET request to a remote server.
func (f *Federator) signedFetch(ctx context.Context, uri string) (map[string]interface{}, error) {
	if err := f.checkFederationURI(ctx, uri); err != nil {
		return nil, err
	}
	if f.fetchCache == nil {
		doc, _, err := f.signedFetchFresh(ctx, uri)
		return doc, err
	}
	if err := validateURL(uri); err != nil {
		return nil, fmt.Errorf("invalid URL %s: %w", uri, err)
	}
	key := "remote:ap:" + uri
	if v, ok := f.fetchCache.Get(key); ok {
		cached := v.(cachedRemoteFetch)
		if cached.status != http.StatusOK {
			return nil, &RemoteFetchError{URI: uri, StatusCode: cached.status}
		}
		var doc map[string]interface{}
		if json.Unmarshal(cached.body, &doc) == nil {
			return doc, nil
		}
		f.fetchCache.Delete(key)
	}
	f.fetchMu.Lock()
	if call := f.fetchInflight[key]; call != nil {
		f.fetchMu.Unlock()
		select {
		case <-call.done:
			return documentFromCall(call)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &remoteFetchCall{done: make(chan struct{})}
	f.fetchInflight[key] = call
	f.fetchMu.Unlock()
	doc, body, err := f.signedFetchFresh(ctx, uri)
	call.doc, call.body, call.err = doc, body, err
	if err == nil {
		if body != nil {
			f.storeRemoteDocumentBytes(uri, body, f.fetchTTL)
		} else {
			f.storeRemoteDocument(uri, doc, f.fetchTTL)
		}
	} else {
		var remoteErr *RemoteFetchError
		if errors.As(err, &remoteErr) && (remoteErr.StatusCode == http.StatusNotFound || remoteErr.StatusCode == http.StatusGone) {
			f.fetchCache.Set(key, cachedRemoteFetch{status: remoteErr.StatusCode}, len(key)+64, f.negativeTTL)
		}
	}
	f.fetchMu.Lock()
	delete(f.fetchInflight, key)
	close(call.done)
	f.fetchMu.Unlock()
	if body != nil {
		// Waiters build their copies from the raw bytes, so the winner can
		// hand its parsed document to the caller without a defensive clone.
		return doc, err
	}
	return cloneDocument(doc), err
}

// documentFromCall gives a coalesced waiter its own independent copy of the
// fetched document: one unmarshal of the response bytes, instead of the old
// marshal + unmarshal clone of the winner's map.
func documentFromCall(call *remoteFetchCall) (map[string]interface{}, error) {
	if call.err == nil && call.body != nil {
		var out map[string]interface{}
		if json.Unmarshal(call.body, &out) == nil {
			return out, nil
		}
	}
	return cloneDocument(call.doc), call.err
}

func cloneDocument(doc map[string]interface{}) map[string]interface{} {
	if doc == nil {
		return nil
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	var out map[string]interface{}
	if json.Unmarshal(body, &out) != nil {
		return nil
	}
	return out
}

// signedFetchFresh performs the remote GET and returns both the parsed
// document and the raw response bytes (nil on the content-negotiation
// fallback path, where the recursive signedFetch owns the bytes).
func (f *Federator) signedFetchFresh(ctx context.Context, uri string) (map[string]interface{}, []byte, error) {
	if err := f.checkFederationURI(ctx, uri); err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/activity+json, application/ld+json; profile=\"https://www.w3.org/ns/activitystreams\"")
	if f.signer != nil {
		if err := f.signer.SignRequest(req, nil); err != nil {
			slog.Warn("failed to sign fetch request", "error", err)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", uri, err)
	}
	defer resp.Body.Close()
	uri = resp.Request.URL.String()
	u := resp.Request.URL
	hostname := u.Hostname()

	// Handle 410 Gone (tombstone) and 404
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		// Try to parse a tombstone from the response body
		maxBytes := int64(1 << 20)
		if f.fetchMaxBytes > 0 && f.fetchMaxBytes < maxBytes {
			maxBytes = f.fetchMaxBytes
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if int64(len(bodyBytes)) > maxBytes {
			bodyBytes = nil
		}
		var doc map[string]interface{}
		if len(bodyBytes) > 0 && json.Unmarshal(bodyBytes, &doc) == nil {
			if ttype, _ := doc["type"].(string); ttype == "Tombstone" {
				tombID, _ := doc["id"].(string)
				formerType, _ := doc["formerType"].(string)
				if formerType == "" {
					formerType = "Note"
				}
				deletedAt := time.Now()
				if deletedStr, _ := doc["deleted"].(string); deletedStr != "" {
					if t, err := time.Parse(time.RFC3339, deletedStr); err == nil {
						deletedAt = t
					}
				}
				tombURI := tombID
				if tombURI == "" {
					tombURI = uri
				}
				if !isStatusObjectType(formerType) {
					_ = f.store.CreateTombstone(ctx, &models.Tombstone{
						URI:        tombURI,
						FormerType: formerType,
						DeletedAt:  deletedAt,
					})
				}
				// Delete any local copy we may have
				_ = f.cleanupLocalObject(ctx, uri)
			}
		}
		return nil, nil, &RemoteFetchError{URI: uri, StatusCode: resp.StatusCode}
	}

	if resp.StatusCode != http.StatusOK {
		if debugSig {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			reqDump, _ := httputil.DumpRequestOut(req, false)
			slog.Warn("DEBUG signed fetch failed",
				"uri", uri,
				"status", resp.StatusCode,
				"resp_www_authenticate", resp.Header.Get("WWW-Authenticate"),
				"resp_body", string(bodyBytes),
				"request_sent", string(reqDump),
			)
		}
		return nil, nil, &RemoteFetchError{URI: uri, StatusCode: resp.StatusCode}
	}

	// Limit response size to 1MB to prevent memory exhaustion from
	// malicious instances serving oversized actor/object documents.
	maxFetchSize := int64(1 << 20)
	if f.fetchMaxBytes > 0 && f.fetchMaxBytes < maxFetchSize {
		maxFetchSize = f.fetchMaxBytes
	}
	limitedBody := io.LimitReader(resp.Body, maxFetchSize+1)

	body, err := io.ReadAll(limitedBody)
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxFetchSize {
		return nil, nil, fmt.Errorf("federation response exceeds %d bytes", maxFetchSize)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		// Content negotiation failure: the server returned HTML despite our
		// Accept header. This commonly happens when a CDN (e.g. Cloudflare)
		// caches the HTML variant and ignores Vary: Accept. Some servers
		// (snac) expose the actor JSON at ?actor as a workaround.
		if !strings.Contains(uri, "?actor") && isHTMLResponse(resp, body) {
			fallbackURI := appendActorParam(uri)
			slog.Debug("content negotiation failed, retrying with ?actor", "uri", uri, "fallback", fallbackURI)
			fallbackDoc, fallbackErr := f.signedFetch(ctx, fallbackURI)
			return fallbackDoc, nil, fallbackErr
		}
		return nil, nil, fmt.Errorf("parse JSON: %w", err)
	}

	// Successful fetch - mark instance as reachable
	if hostname != "" && !f.isLocalDomain(hostname) {
		if err := f.store.MarkInstanceReachable(ctx, hostname); err != nil {
			slog.Warn("failed to mark instance reachable after signed fetch", "domain", hostname, "error", err)
		}
	}

	return doc, body, nil
}

// fetchNodeInfo best-effort fetches NodeInfo metadata for a domain.
// Returns software name, version, and the resolved NodeInfo URL.
// All return values are empty on failure (errors are logged, not returned).
func (f *Federator) fetchNodeInfo(ctx context.Context, domain string) (software, version, nodeInfoURL string) {
	wellKnownURL := fmt.Sprintf("https://%s/.well-known/nodeinfo", domain)
	body, err := f.fetchDiscoveryBytes(ctx, wellKnownURL)
	if err != nil {
		slog.Debug("nodeinfo: failed to fetch well-known", "domain", domain, "error", err)
		return
	}

	var wellKnown struct {
		Links []struct {
			Rel  string `json:"rel"`
			HREF string `json:"href"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &wellKnown); err != nil {
		slog.Debug("nodeinfo: failed to decode well-known", "domain", domain, "error", err)
		return
	}

	// Prefer schema 2.1, fall back to 2.0.
	var targetURL string
	for _, link := range wellKnown.Links {
		if link.Rel == "http://nodeinfo.diaspora.software/ns/schema/2.1" {
			targetURL = link.HREF
			break
		}
		if link.Rel == "http://nodeinfo.diaspora.software/ns/schema/2.0" && targetURL == "" {
			targetURL = link.HREF
		}
	}

	if targetURL == "" {
		slog.Debug("nodeinfo: no supported schema link found", "domain", domain)
		return
	}

	nodeInfoBody, err := f.fetchDiscoveryBytes(ctx, targetURL)
	if err != nil {
		slog.Debug("nodeinfo: failed to fetch nodeinfo", "domain", domain, "error", err)
		return
	}

	var nodeInfo struct {
		Software struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"software"`
	}
	if err := json.Unmarshal(nodeInfoBody, &nodeInfo); err != nil {
		slog.Debug("nodeinfo: failed to decode nodeinfo", "domain", domain, "error", err)
		return
	}

	slog.Info("fetched NodeInfo", "domain", domain, "software", nodeInfo.Software.Name, "version", nodeInfo.Software.Version)
	return nodeInfo.Software.Name, nodeInfo.Software.Version, targetURL
}

func (f *Federator) fetchDiscoveryBytes(ctx context.Context, endpoint string) ([]byte, error) {
	if err := f.checkFederationURI(ctx, endpoint); err != nil {
		return nil, err
	}
	if err := validateURL(endpoint); err != nil {
		return nil, err
	}
	// IP-level SSRF is enforced by the shared transport's dial Control hook.
	key := "remote:discovery:" + endpoint
	if f.fetchCache != nil {
		if v, ok := f.fetchCache.Get(key); ok {
			cached := v.(cachedRemoteFetch)
			if cached.status != http.StatusOK {
				return nil, &RemoteFetchError{URI: endpoint, StatusCode: cached.status}
			}
			return append([]byte(nil), cached.body...), nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if f.fetchCache != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			f.fetchCache.Set(key, cachedRemoteFetch{status: resp.StatusCode}, len(key)+64, f.negativeTTL)
		}
		return nil, &RemoteFetchError{URI: endpoint, StatusCode: resp.StatusCode}
	}
	maxBytes := int64(512 << 10)
	if f.fetchMaxBytes > 0 {
		maxBytes = f.fetchMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("discovery response exceeds %d bytes", maxBytes)
	}
	if f.fetchCache != nil {
		f.fetchCache.Set(key, cachedRemoteFetch{body: append([]byte(nil), body...), status: http.StatusOK}, len(key)+len(body)+64, f.discoveryTTL)
	}
	return body, nil
}

// isActorObjectType reports whether an AS2 "type" value denotes an actor
// (as opposed to a Note/Question/etc. that content-negotiates on the same
// kind of URL a caller might paste into a search/resolve box).
func isActorObjectType(objectType string) bool {
	switch objectType {
	case "Person", "Service", "Application", "Group", "Organization":
		return true
	default:
		return false
	}
}

// parseAndStoreActor parses an ActivityPub actor document and stores/updates it.
func (f *Federator) parseAndStoreActor(ctx context.Context, doc map[string]interface{}, actorURI string) (*models.Account, error) {
	objType, _ := doc["type"].(string)
	if !isActorObjectType(objType) {
		return nil, fmt.Errorf("fetched document at %s is not an actor (type %q)", actorURI, objType)
	}
	// Service/Application actors are bots by AP convention (Mastodon's "bot" field).
	isBot := objType == "Service" || objType == "Application"

	canonicalURI, _ := doc["id"].(string)
	if canonicalURI == "" {
		return nil, fmt.Errorf("actor document has no id")
	}
	if err := f.checkFederationURI(ctx, canonicalURI); err != nil {
		return nil, fmt.Errorf("canonical actor id: %w", err)
	}
	parsedURI, _ := url.Parse(canonicalURI)
	domain := strings.ToLower(strings.TrimSuffix(parsedURI.Hostname(), "."))
	requestedURI, err := url.Parse(actorURI)
	if err != nil {
		return nil, fmt.Errorf("parse requested actor URI: %w", err)
	}
	if f.isLocalDomain(domain) && !f.isLocalDomain(requestedURI.Hostname()) {
		return nil, fmt.Errorf("remote actor id resolves to local domain %s", domain)
	}

	preferredUsername, _ := doc["preferredUsername"].(string)
	name, _ := doc["name"].(string)
	summary, _ := doc["summary"].(string)

	inbox, _ := doc["inbox"].(string)
	outbox, _ := doc["outbox"].(string)
	followers, _ := doc["followers"].(string)
	following, _ := doc["following"].(string)
	alsoKnownAs := strings.Join(activityStreamsLinks(doc["alsoKnownAs"]), "\n")
	movedTo := activityStreamsLink(doc["movedTo"])

	// Extract public keys (PEM, or Mastodon publicKeyBase64 / GoToSocial
	// publicKeyMultibase / assertionMethod Multikey for Ed25519 actors),
	// normalized to PKIX PEM. The Ed25519 key backs Object Integrity Proofs.
	keys, keyErr := extractActorPublicKeys(doc)
	if keyErr != nil {
		slog.Warn("federator: actor public key parse failed",
			"uri", canonicalURI,
			"error", keyErr,
		)
	}

	// Extract shared inbox from endpoints
	sharedInbox := ""
	if endpoints, ok := doc["endpoints"].(map[string]interface{}); ok {
		si, _ := endpoints["sharedInbox"].(string)
		sharedInbox = si
	}

	// Extract icon/avatar
	avatar := ""
	if icon, ok := doc["icon"].(map[string]interface{}); ok {
		url, _ := icon["url"].(string)
		avatar = url
	}

	// Extract header/image
	header := ""
	if image, ok := doc["image"].(map[string]interface{}); ok {
		url, _ := image["url"].(string)
		header = url
	}

	// Extract manuallyApprovesFollowers
	manuallyApprovesFollowers := false
	if maf, ok := doc["manuallyApprovesFollowers"].(bool); ok {
		manuallyApprovesFollowers = maf
	}

	// Extract follower/following/status counts from the actor document.
	// These are Mastodon/Pleroma extensions (not in the core ActivityPub
	// spec) and may use camelCase or snake_case depending on the server.
	// Most Mastodon servers do not include these in the ActivityPub actor
	// document at all; they are fetched separately via the REST API.
	followersCount, hasFollowersCount := extractIntFromDoc(doc, "followersCount")
	if !hasFollowersCount {
		followersCount, hasFollowersCount = extractIntFromDoc(doc, "followers_count")
	}
	followingCount, hasFollowingCount := extractIntFromDoc(doc, "followingCount")
	if !hasFollowingCount {
		followingCount, hasFollowingCount = extractIntFromDoc(doc, "following_count")
	}
	statusesCount, hasStatusesCount := extractIntFromDoc(doc, "statusesCount")
	if !hasStatusesCount {
		statusesCount, hasStatusesCount = extractIntFromDoc(doc, "statuses_count")
	}

	now := time.Now()

	// The actor's "published" field (ActivityPub/Mastodon extension) carries
	// the real account creation date on the remote server. Fall back to the
	// discovery time only when it's absent or unparsable.
	createdAt := now
	hasPublished := false
	if pub, ok := doc["published"].(string); ok && pub != "" {
		if parsed, err := time.Parse(time.RFC3339, pub); err == nil {
			createdAt = parsed
			hasPublished = true
		}
	}

	// Try to find an existing account. We use a multi-step lookup to handle
	// three cases:
	//   1. Exact match by canonical actor_uri (from the document id field)
	//   2. Match by the request URL (if it differs from canonical, e.g. due
	//      to a redirect that wasn't reflected in a previous store)
	//   3. Match by username+domain (catches records stored under a different
	//      URL variant, preventing UNIQUE constraint violations on insert)
	var needsCreate bool
	account, err := f.store.GetAccountByURI(ctx, canonicalURI)
	if err != nil && canonicalURI != actorURI {
		account, err = f.store.GetAccountByURI(ctx, actorURI)
	}
	// Never mutate a local account from a remote fetch. A self-fetch of one of
	// our own actors (e.g. the instance actor) must not overwrite its keys or
	// other fields with the parsed document, which would desync the served
	// public key from the private key we sign with and break signature
	// verification on authorized-fetch servers like GoToSocial.
	if err == nil && account.Domain == nil {
		return account, nil
	}

	// Resolve the canonical handle domain. The actor URL host may differ from
	// the domain used in the fediverse handle (web domain vs. account domain);
	// resolving it keeps a person fetched via a boost (actor URI) and via a
	// follow (WebFinger handle) as a single account. Known accounts were
	// already canonicalised on first discovery and their stored domain is
	// authoritative, so only pay for the WebFinger round-trip for new actors.
	if err == nil && account.Domain != nil {
		domain = *account.Domain
	} else if preferredUsername != "" {
		domain = f.canonicalAccountDomain(ctx, preferredUsername, domain, canonicalURI)
	}

	if err != nil {
		// Not found by either URI - try username+domain fallback
		existingByUser, lookupErr := f.store.GetAccountByUsernameDomain(ctx, preferredUsername, &domain)
		if lookupErr == nil {
			account = existingByUser
			// Fix up the actor_uri to the canonical form
			slog.Info("federator: found existing account by username+domain, updating actor_uri",
				"old_uri", existingByUser.ActorURI,
				"new_uri", canonicalURI,
				"username", preferredUsername,
				"domain", domain,
			)
		} else {
			needsCreate = true
		}
	}

	if needsCreate {
		// Create new account
		account = &models.Account{
			Username:                  preferredUsername,
			Domain:                    &domain,
			ActorURI:                  canonicalURI,
			InboxURI:                  inbox,
			SharedInboxURI:            &sharedInbox,
			OutboxURI:                 outbox,
			FollowersURI:              followers,
			FollowingURI:              following,
			PublicKeyPem:              keys.publicKeyPEM,
			PublicKeyID:               keys.publicKeyID,
			PublicKeysJSON:            keys.json(),
			Ed25519PublicKeyPem:       keys.ed25519PublicKeyPEM,
			Ed25519PublicKeyID:        keys.ed25519PublicKeyID,
			DisplayName:               name,
			Note:                      summary,
			Avatar:                    avatar,
			AvatarRemote:              avatar,
			Header:                    header,
			HeaderRemote:              header,
			ManuallyApprovesFollowers: manuallyApprovesFollowers,
			Locked:                    manuallyApprovesFollowers,
			IsBot:                     isBot,
			FollowersCount:            followersCount,
			FollowingCount:            followingCount,
			StatusesCount:             statusesCount,
			AlsoKnownAs:               alsoKnownAs,
			MovedToURI:                movedTo,
			FetchedAt:                 &now,
			CreatedAt:                 createdAt,
			UpdatedAt:                 now,
		}
		if hasFollowersCount || hasFollowingCount || hasStatusesCount {
			account.CountsFetchedAt = &now
		}

		if err := f.store.CreateAccount(ctx, account); err != nil {
			// If CreateAccount failed with a unique constraint violation
			// (race condition: another request created the same account
			// between our lookup and insert), try one more lookup by
			// username+domain and fall through to the update path.
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				existingByUser, lookupErr := f.store.GetAccountByUsernameDomain(ctx, preferredUsername, &domain)
				if lookupErr == nil {
					account = existingByUser
					slog.Warn("federator: recovered from UNIQUE constraint race, updating existing account",
						"uri", canonicalURI,
						"username", preferredUsername,
						"domain", domain,
					)
					needsCreate = false
				} else {
					return nil, fmt.Errorf("create remote account: %w", err)
				}
			} else {
				return nil, fmt.Errorf("create remote account: %w", err)
			}
		}
	}

	if !needsCreate {
		// Update existing account
		account.ActorURI = canonicalURI
		account.Username = preferredUsername
		account.InboxURI = inbox
		if sharedInbox != "" {
			account.SharedInboxURI = &sharedInbox
		}
		account.OutboxURI = outbox
		account.FollowersURI = followers
		account.FollowingURI = following
		// A successful actor refresh is authoritative. Empty values revoke keys;
		// retaining the old material would let a removed key authenticate forever.
		account.PublicKeyPem = keys.publicKeyPEM
		account.PublicKeyID = keys.publicKeyID
		account.PublicKeysJSON = keys.json()
		account.Ed25519PublicKeyPem = keys.ed25519PublicKeyPEM
		account.Ed25519PublicKeyID = keys.ed25519PublicKeyID
		account.DisplayName = name
		account.Note = summary
		// Preserve a locally cached avatar/header when the remote URL is
		// unchanged; otherwise adopt the new remote URL (the old cache entry
		// becomes unreferenced and is reclaimed by the cache GC).
		if avatar != account.AvatarRemote {
			account.Avatar = avatar
			account.AvatarRemote = avatar
		}
		if header != account.HeaderRemote {
			account.Header = header
			account.HeaderRemote = header
		}
		account.ManuallyApprovesFollowers = manuallyApprovesFollowers
		account.Locked = manuallyApprovesFollowers
		account.IsBot = isBot
		account.AlsoKnownAs = alsoKnownAs
		account.MovedToURI = movedTo
		if hasPublished {
			account.CreatedAt = createdAt
		}
		if hasFollowersCount {
			account.FollowersCount = followersCount
		}
		if hasFollowingCount {
			account.FollowingCount = followingCount
		}
		if hasStatusesCount {
			account.StatusesCount = statusesCount
		}
		if hasFollowersCount || hasFollowingCount || hasStatusesCount {
			account.CountsFetchedAt = &now
		}
		account.FetchedAt = &now

		if err := f.store.UpdateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("update remote account: %w", err)
		}
		if f.accountUpdated != nil {
			f.accountUpdated(account.ID)
		}
	}

	// Custom emojis used in the display name / bio. Stored globally keyed by
	// (shortcode, domain); resolved against the profile text at render time.
	upsertEmojiTags(ctx, f.store, doc["tag"], &domain)

	// Ensure an Instance record exists for this domain
	if !f.isLocalDomain(domain) {
		if err := f.EnsureInstanceExists(ctx, domain); err != nil {
			slog.Warn("failed to ensure instance record", "domain", domain, "error", err)
		}
	}

	return account, nil
}

func docHasAnyCountField(doc map[string]interface{}) bool {
	for _, key := range []string{
		"followersCount", "followers_count",
		"followingCount", "following_count",
		"statusesCount", "statuses_count",
	} {
		if _, ok := doc[key]; ok {
			return true
		}
	}
	return false
}

// extractIntFromDoc extracts an integer value from a map, handling JSON
// numeric types (float64 from json.Unmarshal). The bool reports whether the
// key was present with a supported integer value, so explicit zero can be
// distinguished from a missing count field.
func extractIntFromDoc(doc map[string]interface{}, key string) (int, bool) {
	val, ok := doc[key]
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// validateURL checks that a URL is safe to fetch (SSRF guard).
func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	if u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("URL must have a host and no credentials")
	}

	// Block private IP ranges, localhost, etc.
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return fmt.Errorf("refusing to fetch localhost")
	}

	// Check if host is an IP address
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(host) {
			return fmt.Errorf("refusing to fetch private IP: %s", host)
		}
	}

	// Basic check for internal hostnames (catch common patterns even if not IP)
	if strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "172.16.") ||
		strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "169.254.") ||
		strings.HasPrefix(host, "0.") || strings.HasPrefix(host, "127.") {
		return fmt.Errorf("refusing to fetch private IP range: %s", host)
	}

	return nil
}

// isPrivateIP checks whether an IP string is a private, loopback, link-local,
// unspecified, or otherwise non-public address.
func isPrivateIP(ipStr string) bool {
	return !netutil.IsPublicIP(net.ParseIP(ipStr))
}

// cleanupLocalObject deletes any local copy of an object identified by URI.
// Called when a remote server returns 410 Gone, indicating the object is
// permanently deleted. Tries both statuses and accounts.
func (f *Federator) cleanupLocalObject(ctx context.Context, uri string) error {
	// Try to find and delete a local status
	status, err := f.store.GetStatusByURI(ctx, uri)
	if err == nil {
		if err := f.store.DeleteStatus(ctx, status.ID); err != nil {
			return fmt.Errorf("cleanup: delete status %s: %w", uri, err)
		}
		slog.Info("federator: cleaned up local copy of deleted remote status", "uri", uri)
		return nil
	}

	// Try to find and delete a local account copy
	account, err := f.store.GetAccountByURI(ctx, uri)
	if err == nil {
		if err := f.store.DeleteAccount(ctx, account.ID); err != nil {
			return fmt.Errorf("cleanup: delete account %s: %w", uri, err)
		}
		slog.Info("federator: cleaned up local copy of deleted remote account", "uri", uri)
		return nil
	}

	return nil // nothing to clean up, not an error
}

// sealKeyPair seals a freshly generated pair of private keys before they are
// written. With no encryption key configured both come back unchanged.
func sealKeyPair(privateKeyPEM, ed25519PrivateKeyPEM string) (string, string, error) {
	sealed, err := keycrypt.Seal(privateKeyPEM)
	if err != nil {
		return "", "", fmt.Errorf("seal private key: %w", err)
	}
	sealedEd, err := keycrypt.Seal(ed25519PrivateKeyPEM)
	if err != nil {
		return "", "", fmt.Errorf("seal ed25519 private key: %w", err)
	}
	return sealed, sealedEd, nil
}

// getActorSigner returns a Signer for the given actor URI, caching it.
// Only returns a signer for local users (domain == nil); remote actors are
// skipped (the instance signer is used for signed fetches, not deliveries).
func (f *Federator) getActorSigner(ctx context.Context, actorURI string) (*Signer, error) {
	// Check cache first
	f.signerMu.RLock()
	if s, ok := f.userSigners[actorURI]; ok {
		f.signerMu.RUnlock()
		return s, nil
	}
	f.signerMu.RUnlock()

	account, err := f.store.GetAccountByURI(ctx, actorURI)
	if err != nil {
		return nil, fmt.Errorf("getActorSigner: %w", err)
	}

	// Only sign with local users' keys
	if account.Domain != nil || account.PrivateKeyPem == "" {
		return nil, fmt.Errorf("getActorSigner: not a local user with a private key")
	}

	privateKeyPEM, err := keycrypt.Open(account.PrivateKeyPem)
	if err != nil {
		return nil, fmt.Errorf("getActorSigner: %w", err)
	}

	keyID := account.ActorURI + "#main-key"
	s, err := NewSigner(keyID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("getActorSigner: new signer: %w", err)
	}
	s.SetUserAgent(f.userAgent)

	f.signerMu.Lock()
	if existing, ok := f.userSigners[actorURI]; ok {
		f.signerMu.Unlock()
		return existing, nil
	}
	f.userSigners[actorURI] = s
	f.signerMu.Unlock()
	return s, nil
}

// SetupInstanceActor creates the instance actor for signed fetches.
// Returns a Signer configured with the instance actor's key.
func SetupInstanceActor(ctx context.Context, store storage.Storage, domain string) (*Signer, error) {
	return SetupInstanceActorWithDomains(ctx, store, domain, domain)
}

// SetupInstanceActorWithDomains creates the service actor on webDomain. The
// local-domain argument keeps the API symmetric with human actor setup; the
// service actor itself is keyed by the web host for compatibility.
func SetupInstanceActorWithDomains(ctx context.Context, store storage.Storage, _ string, webDomain string) (*Signer, error) {
	// The instance actor uses a dedicated account or reuses the local user
	// For simplicity, we create a special "instance" actor
	// Keep the service actor keyed by the web host. Unlike human accounts it is
	// not addressed through the public LOCAL_DOMAIN handle namespace, and this
	// preserves the actor identity of existing single-domain installations.
	instanceUsername := webDomain

	account, err := store.GetAccountByUsernameDomain(ctx, instanceUsername, nil)
	if err != nil {
		// Create instance actor
		privKey, pubKey, edPrivKey, edPubKey, err := GenerateDualKeyPair()
		if err != nil {
			return nil, fmt.Errorf("generate instance key: %w", err)
		}
		sealedPrivKey, sealedEdPrivKey, err := sealKeyPair(privKey, edPrivKey)
		if err != nil {
			return nil, err
		}

		now := time.Now()
		account = &models.Account{
			Username:             instanceUsername,
			Domain:               nil,
			IsServiceActor:       true,
			ActorURI:             fmt.Sprintf("https://%s/users/%s", webDomain, instanceUsername),
			InboxURI:             fmt.Sprintf("https://%s/inbox", webDomain),
			OutboxURI:            fmt.Sprintf("https://%s/outbox", webDomain),
			FollowersURI:         fmt.Sprintf("https://%s/followers", webDomain),
			FollowingURI:         fmt.Sprintf("https://%s/following", webDomain),
			PublicKeyPem:         pubKey,
			PublicKeyID:          fmt.Sprintf("https://%s/users/%s#main-key", webDomain, instanceUsername),
			PrivateKeyPem:        sealedPrivKey,
			Ed25519PublicKeyPem:  edPubKey,
			Ed25519PublicKeyID:   fmt.Sprintf("https://%s/users/%s#ed25519-key", webDomain, instanceUsername),
			Ed25519PrivateKeyPem: sealedEdPrivKey,
			DisplayName:          webDomain,
			CreatedAt:            now,
			UpdatedAt:            now,
		}

		if err := store.CreateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("create instance account: %w", err)
		}

		slog.Info("created instance actor", "actor_uri", account.ActorURI)
	} else {
		// Older databases stored the instance actor as an indistinguishable local
		// account. Keep the persisted marker correct even if migration backfill
		// could not recognize a customized legacy actor.
		if !account.IsServiceActor {
			account.IsServiceActor = true
			if err := store.UpdateAccount(ctx, account); err != nil {
				return nil, fmt.Errorf("mark instance service actor: %w", err)
			}
		}

		// Migrate existing instance actor from /instance to /users/<domain> URI
		// so the key ID matches what WebFinger returns.
		oldInstanceURI := fmt.Sprintf("https://%s/instance", webDomain)
		newInstanceURI := fmt.Sprintf("https://%s/users/%s", webDomain, instanceUsername)
		if account.ActorURI == oldInstanceURI {
			account.ActorURI = newInstanceURI
			if err := store.UpdateAccount(ctx, account); err != nil {
				return nil, fmt.Errorf("migrate instance actor URI: %w", err)
			}
			slog.Info("migrated instance actor URI", "old", oldInstanceURI, "new", newInstanceURI)
		}
	}

	if account.PrivateKeyPem == "" {
		// Regenerate keys if missing
		privKey, pubKey, edPrivKey, edPubKey, err := GenerateDualKeyPair()
		if err != nil {
			return nil, fmt.Errorf("generate instance key: %w", err)
		}
		sealedPrivKey, sealedEdPrivKey, err := sealKeyPair(privKey, edPrivKey)
		if err != nil {
			return nil, err
		}
		account.PublicKeyPem = pubKey
		account.PublicKeyID = account.ActorURI + "#main-key"
		account.PrivateKeyPem = sealedPrivKey
		account.Ed25519PublicKeyPem = edPubKey
		account.Ed25519PublicKeyID = account.ActorURI + "#ed25519-key"
		account.Ed25519PrivateKeyPem = sealedEdPrivKey
		if err := store.UpdateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("update instance keys: %w", err)
		}
	}

	// Ensure the served public key actually matches the private key we sign
	// with. HTTP Signature verifiers (e.g. GoToSocial in authorized-fetch mode)
	// fetch our instance actor's publicKeyPem and verify our signed GETs against
	// it; if it is empty or stale (older schema, partial migration, or a
	// self-fetch overwriting it) every signed fetch is rejected with 401 while
	// servers that don't verify GET signatures (Mastodon, snac) keep working.
	// Repair it from the private key so the pair is always consistent.
	instancePrivateKeyPEM, err := keycrypt.Open(account.PrivateKeyPem)
	if err != nil {
		return nil, fmt.Errorf("instance actor private key: %w", err)
	}
	derivedPub, err := PublicKeyPEMFromPrivate(instancePrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("derive instance public key: %w", err)
	}
	if account.PublicKeyPem != derivedPub {
		slog.Warn("instance actor public key did not match private key; repairing",
			"actor_uri", account.ActorURI,
			"had_public_key", account.PublicKeyPem != "",
		)
		account.PublicKeyPem = derivedPub
		if err := store.UpdateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("repair instance public key: %w", err)
		}
	}
	if account.PublicKeyID != account.ActorURI+"#main-key" {
		account.PublicKeyID = account.ActorURI + "#main-key"
		if err := store.UpdateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("repair instance public key ID: %w", err)
		}
	}

	// Ensure the instance actor has an Ed25519 pair for OIPs and ed25519 HTTP
	// Signatures. Upgrade path: pre-ed25519 instances have none, so generate
	// one on first startup. Existing Ed25519 pairs are repaired exactly like
	// the RSA pair above (derived public must match the stored public).
	if account.Ed25519PrivateKeyPem == "" {
		edPrivKey, edPubKey, err := GenerateEd25519KeyPair()
		if err != nil {
			return nil, fmt.Errorf("generate instance ed25519 key: %w", err)
		}
		sealedEdPrivKey, err := keycrypt.Seal(edPrivKey)
		if err != nil {
			return nil, fmt.Errorf("seal instance ed25519 key: %w", err)
		}
		account.Ed25519PublicKeyPem = edPubKey
		account.Ed25519PublicKeyID = account.ActorURI + "#ed25519-key"
		account.Ed25519PrivateKeyPem = sealedEdPrivKey
		if err := store.UpdateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("update instance ed25519 keys: %w", err)
		}
		slog.Info("generated instance actor Ed25519 key", "actor_uri", account.ActorURI)
	} else if instanceEdPrivateKeyPEM, derr := keycrypt.Open(account.Ed25519PrivateKeyPem); derr != nil {
		return nil, fmt.Errorf("instance actor ed25519 private key: %w", derr)
	} else if derivedEdPub, derr := Ed25519PublicKeyPEMFromPrivate(instanceEdPrivateKeyPEM); derr == nil && account.Ed25519PublicKeyPem != derivedEdPub {
		slog.Warn("instance actor Ed25519 public key did not match private key; repairing",
			"actor_uri", account.ActorURI,
		)
		account.Ed25519PublicKeyPem = derivedEdPub
		if err := store.UpdateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("repair instance Ed25519 public key: %w", err)
		}
	}
	if account.Ed25519PublicKeyID != account.ActorURI+"#ed25519-key" {
		account.Ed25519PublicKeyID = account.ActorURI + "#ed25519-key"
		if err := store.UpdateAccount(ctx, account); err != nil {
			return nil, fmt.Errorf("repair instance Ed25519 key ID: %w", err)
		}
	}

	keyID := account.ActorURI + "#main-key"
	signer, err := NewSigner(keyID, instancePrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("create instance signer: %w", err)
	}

	// Self-check: sign a sample GET and verify it against the public key we
	// actually serve. This is exactly what a remote authorized-fetch server
	// (GoToSocial) does, so a failure here means every signed fetch to such a
	// server will be rejected with 401. Logged loudly so the cause is obvious.
	if err := verifyInstanceSigningHealth(signer, account.PublicKeyPem, keyID); err != nil {
		slog.Error("instance actor signing self-check FAILED - signed fetches to "+
			"authorized-fetch servers (e.g. GoToSocial) will be rejected with 401",
			"keyID", keyID, "error", err)
	} else {
		slog.Info("instance actor signing self-check passed", "keyID", keyID)
	}

	return signer, nil
}

// verifyInstanceSigningHealth signs a representative GET request with the
// instance signer and verifies the produced signature against servedPublicKey,
// mirroring a remote server's HTTP Signature verification.
func verifyInstanceSigningHealth(signer *Signer, servedPublicKey, keyID string) error {
	owner := ExtractKeyOwner(keyID)
	u, err := url.Parse(owner)
	if err != nil || u.Host == "" {
		// Fall back to a synthetic target; the host value is irrelevant to the
		// crypto as long as signing and verifying use the same request.
		u = &url.URL{Scheme: "https", Host: "self-check.invalid", Path: "/users/self-check"}
	} else {
		u.Path = "/.well-known/self-check"
		u.RawQuery = ""
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("build self-check request: %w", err)
	}
	req.Header.Set("Accept", "application/activity+json")
	if err := signer.SignRequest(req, nil); err != nil {
		return fmt.Errorf("sign self-check request: %w", err)
	}

	params := parseSignatureHeader(req.Header.Get("Signature"))
	if params == nil {
		return fmt.Errorf("self-check signature header missing")
	}
	signedHeaders := strings.Fields(params["headers"])
	if len(signedHeaders) == 0 {
		signedHeaders = []string{"(request-target)", "host", "date"}
	}
	signingString := buildSigningString(req, signedHeaders)
	if err := Verify(servedPublicKey, []byte(signingString), params["signature"]); err != nil {
		return fmt.Errorf("served public key does not verify the instance signer's signature: %w", err)
	}
	return nil
}

// isHTMLResponse checks whether an HTTP response is HTML rather than JSON.
// Used to detect content negotiation failures, e.g. when a CDN caches the
// HTML variant and ignores the Accept header.
func isHTMLResponse(resp *http.Response, body []byte) bool {
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		return true
	}
	// Belt-and-suspenders: check if the body starts like HTML
	if len(body) > 0 && body[0] == '<' {
		return true
	}
	return false
}

// appendActorParam appends ?actor to a URL, used as a fallback for servers
// (like snac) that expose actor JSON at ?actor when content negotiation fails.
func appendActorParam(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("actor", "")
	u.RawQuery = q.Encode()
	return u.String()
}

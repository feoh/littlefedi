package federation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/cache"
	contentlib "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/content"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/httpneg"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/instancemeta"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/opengraph"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/version"
)

// Handler serves ActivityPub endpoints for the federation layer.
type Handler struct {
	store         storage.Storage
	domain        string // web domain (legacy field name)
	localDomain   string
	verifier      *Verifier
	federator     *Federator
	responseCache *cache.MemoryCache
	responseTTL   time.Duration
	discoveryTTL  time.Duration
	responseMax   int
	responseMu    sync.Mutex
	responseCalls map[string]chan struct{}

	openRegistrations bool
	secureMode        bool

	// instanceName and metaDefaults feed the NodeInfo metadata block, which is
	// where fediverse crawlers and server directories read a node's name and
	// description from. metaDefaults holds the configured values; the
	// admin-saved profile overrides them per request.
	instanceName string
	metaDefaults instancemeta.Meta
}

// SetOpenRegistrations controls the "openRegistrations" flag NodeInfo reports.
func (h *Handler) SetOpenRegistrations(open bool) {
	h.openRegistrations = open
}

// SetInstanceMetadata sets the instance name and the configured description
// fallbacks NodeInfo publishes.
func (h *Handler) SetInstanceMetadata(name string, defaults instancemeta.Meta) {
	h.instanceName = name
	h.metaDefaults = defaults
}

// SetSecureMode enables authorized fetch: GET requests to actor, outbox,
// followers/following, and object endpoints must carry a valid HTTP
// Signature. The instance actor stays public for key discovery.
func (h *Handler) SetSecureMode(secure bool) {
	h.secureMode = secure
}

// requireSignedGET rejects unsigned GET requests in secure or allowlist mode.
func (h *Handler) requireSignedGET(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mode, err := h.store.FederationPolicyMode(r.Context())
		if err != nil {
			jsonError(w, http.StatusServiceUnavailable, "federation policy unavailable")
			return
		}
		mustSign := h.secureMode || mode == "allowlist"
		if mustSign {
			if h.requestSignerBlocked(r) {
				jsonError(w, http.StatusForbidden, "federation peer blocked")
				return
			}
			if h.verifier == nil {
				jsonError(w, http.StatusServiceUnavailable, "secure mode misconfigured")
				return
			}
			keyOwner, err := h.verifier.VerifyRequest(r)
			if err != nil {
				jsonError(w, http.StatusUnauthorized, "signed request required")
				return
			}
			if u, err := url.Parse(ExtractKeyOwner(keyOwner)); err == nil && hostInboundBlocked(r.Context(), h.store, u.Hostname(), h.localDomain, h.domain) {
				jsonError(w, http.StatusForbidden, "federation peer blocked")
				return
			}
		}
		next(w, r)
	}
}

// requireSignedAPGET leaves the instance actor public for key discovery and
// skips authorized fetch for browser redirects. Use it only on handlers that
// redirect text/html; collections must stay on requireSignedGET.
func (h *Handler) requireSignedAPGET(next http.HandlerFunc) http.HandlerFunc {
	signed := h.requireSignedGET(next)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept")
		if wantsHTML(r) || r.URL.Path == "/users/"+h.domain {
			next(w, r)
			return
		}
		signed(w, r)
	}
}

type cachedFederationResponse struct {
	status      int
	body        []byte
	contentType string
	etag        string
}

// setResponseCache enables caching of serialized, viewer-neutral ActivityPub
// objects. The supplied cache is also owned by the storage timeline decorator,
// whose successful writes clear it before changed data can be served.
func (h *Handler) setResponseCache(c *cache.MemoryCache, ttl time.Duration) {
	h.responseCache = c
	h.responseTTL = ttl
	h.discoveryTTL = ttl
	h.responseMax = 512 << 10
	if h.responseCalls == nil {
		h.responseCalls = make(map[string]chan struct{})
	}
}

// SetResponseCacheOptions configures distinct ordinary/discovery lifetimes and
// a hard per-response size bound.
func (h *Handler) SetResponseCacheOptions(c *cache.MemoryCache, ttl, discoveryTTL time.Duration, maxBytes int) {
	h.setResponseCache(c, ttl)
	h.discoveryTTL = discoveryTTL
	if maxBytes > 0 {
		h.responseMax = maxBytes
	}
}

// NewHandler creates a new federation handler.
func NewHandler(store storage.Storage, domain string, verifier *Verifier, federator *Federator) *Handler {
	return NewHandlerWithDomains(store, domain, domain, verifier, federator)
}

func NewHandlerWithDomains(store storage.Storage, localDomain, webDomain string, verifier *Verifier, federator *Federator) *Handler {
	return &Handler{
		store:       store,
		domain:      webDomain,
		localDomain: localDomain,
		verifier:    verifier,
		federator:   federator,
	}
}

// RegisterRoutes adds federation routes to the router.
func (h *Handler) RegisterRoutes(r chi.Router) {
	// Well-known endpoints
	r.Get("/.well-known/webfinger", h.cachedGET(true, h.WebFinger))
	r.Get("/.well-known/nodeinfo", h.cachedGET(true, h.NodeInfoWellKnown))
	r.Get("/.well-known/host-meta", h.cachedGET(true, h.HostMeta))

	// NodeInfo
	r.Get("/nodeinfo/2.1", h.cachedGET(true, h.NodeInfo))

	// ActivityPub actor and object endpoints. Wrapped in requireSignedGET
	// (outside cachedGET, so it runs on every request - cache hits included -
	// rather than only on cache misses) to enforce authorized fetch when
	// secure or allowlist mode is on.
	r.Get("/users/{username}", h.requireSignedAPGET(h.cachedGET(true, h.ActorGet)))
	r.Get("/users/{username}/inbox", h.requireSignedGET(h.InboxGet))
	r.Post("/users/{username}/inbox", h.InboxPost)
	r.Get("/users/{username}/outbox", h.requireSignedGET(h.cachedGET(false, h.OutboxGet)))
	r.Get("/users/{username}/followers", h.requireSignedGET(h.cachedFollowList(true, h.FollowersGet)))
	r.Get("/users/{username}/following", h.requireSignedGET(h.cachedFollowList(false, h.FollowingGet)))

	// Public alias for the instance actor's canonical /users/<web_domain> URL.
	r.Get("/instance", h.cachedGET(true, h.InstanceActorGet))

	// Shared inbox
	r.Post("/inbox", h.SharedInboxPost)

	// Object endpoints
	r.Get("/objects/{id}", h.requireSignedAPGET(h.cachedGET(false, h.ObjectGet)))
	r.Get("/objects/{id}/likes", h.requireSignedGET(h.cachedGET(false, h.ObjectLikesGet)))
	r.Get("/objects/{id}/shares", h.requireSignedGET(h.cachedGET(false, h.ObjectSharesGet)))
	r.Get("/objects/{id}/emoji_reactions", h.requireSignedGET(h.cachedGET(false, h.ObjectEmojiReactionsGet)))
	// Quote authorization proofs follow the same signed-read policy as objects.
	r.Get("/objects/{id}/quote_authorizations/{authID}", h.requireSignedGET(h.cachedGET(false, h.QuoteAuthorizationGet)))

	// Featured / pinned collections (empty by default, avoids 404 noise)
	r.Get("/users/{username}/collections/featured", h.requireSignedGET(h.cachedGET(false, h.FeaturedGet)))
	r.Get("/users/{username}/collections/tags", h.requireSignedGET(h.cachedGET(false, h.FeaturedTagsGet)))

}

type federationCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *federationCapture) Header() http.Header { return w.header }
func (w *federationCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *federationCapture) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func federationETag(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("\"%x\"", sum[:16])
}

// cachedGET caches only successful, public GET representations. The Accept
// header class is part of the key so actor HTML redirects never alias AP JSON.
// The request URI comes first in the key so the storage layer can invalidate
// one object's representations (all Accept classes and collection pages) with
// a single prefix delete instead of wiping the whole response cache - during a
// remote boost/like storm every inbound interaction would otherwise flush the
// very entries the accompanying fetch storm needs.
func (h *Handler) cachedGET(discovery bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.responseCache == nil {
			next(w, r)
			return
		}
		accept := "other"
		if acceptsAP(r) {
			accept = "activitypub"
		} else if strings.Contains(r.Header.Get("Accept"), "json") {
			accept = "json"
		}
		key := "fed:http:" + r.URL.RequestURI() + "|" + accept
		if v, ok := h.responseCache.Get(key); ok {
			h.serveCachedGET(w, r, v.(cachedFederationResponse))
			return
		}
		h.responseMu.Lock()
		if wait := h.responseCalls[key]; wait != nil {
			h.responseMu.Unlock()
			select {
			case <-wait:
				if v, ok := h.responseCache.Get(key); ok {
					h.serveCachedGET(w, r, v.(cachedFederationResponse))
					return
				}
				next(w, r)
				return
			case <-r.Context().Done():
				return
			}
		}
		done := make(chan struct{})
		h.responseCalls[key] = done
		h.responseMu.Unlock()
		defer func() { h.responseMu.Lock(); delete(h.responseCalls, key); close(done); h.responseMu.Unlock() }()
		cacheVersion := h.responseCache.Version()
		capture := &federationCapture{header: make(http.Header)}
		next(capture, r)
		for name, values := range capture.header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		cacheable := capture.status == http.StatusOK && capture.body.Len() > 0 && capture.body.Len() <= h.responseMax
		body := append([]byte(nil), capture.body.Bytes()...)
		etag := ""
		if cacheable {
			etag = federationETag(body)
			w.Header().Set("ETag", etag)
			w.Header().Set("X-littleFedi-Cache", "miss")
		}
		w.WriteHeader(capture.status)
		_, _ = w.Write(body)
		if cacheable {
			ttl := h.responseTTL
			if discovery {
				ttl = h.discoveryTTL
			}
			h.responseCache.SetIfVersion(cacheVersion, key, cachedFederationResponse{status: capture.status, body: body, contentType: w.Header().Get("Content-Type"), etag: etag}, len(key)+len(body)+128, ttl)
		}
	}
}

func (h *Handler) serveCachedGET(w http.ResponseWriter, r *http.Request, cached cachedFederationResponse) {
	if cached.etag != "" {
		w.Header().Set("ETag", cached.etag)
	}
	if cached.etag != "" && r.Header.Get("If-None-Match") == cached.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if cached.contentType != "" {
		w.Header().Set("Content-Type", cached.contentType)
	}
	w.Header().Set("X-littleFedi-Cache", "hit")
	w.WriteHeader(cached.status)
	_, _ = w.Write(cached.body)
}

// ActorGet serves the ActivityPub actor document for a local user.
func (h *Handler) ActorGet(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username == "" {
		jsonError(w, http.StatusBadRequest, "missing username")
		return
	}

	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		// Account not found - check for a tombstone for the actor URI
		actorURI := fmt.Sprintf("https://%s/users/%s", h.domain, username)
		tombstone, err := h.store.GetTombstoneByURI(r.Context(), actorURI)
		if err == nil {
			h.serveTombstone(w, tombstone)
			return
		}
		slog.Warn("actor not found", "username", username, "error", err)
		jsonError(w, http.StatusNotFound, "account not found")
		return
	}

	if acceptsAP(r) {
		actor := h.actorToAS2(r.Context(), account)
		writeJSON(w, http.StatusOK, actor)
		return
	}

	http.Redirect(w, r, "/@"+username, http.StatusFound)
}

// InstanceActorGet serves the instance actor document.
func (h *Handler) InstanceActorGet(w http.ResponseWriter, r *http.Request) {
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), h.domain, nil)
	if err != nil {
		slog.Warn("instance actor not found", "error", err)
		jsonError(w, http.StatusNotFound, "instance actor not found")
		return
	}

	if acceptsAP(r) {
		actor := h.instanceActorToAS2(account)
		writeJSON(w, http.StatusOK, actor)
		return
	}

	jsonError(w, http.StatusNotFound, "not found")
}

// instanceActorToAS2 builds the AS2 representation for the instance actor.
func (h *Handler) instanceActorToAS2(account *models.Account) map[string]interface{} {
	actor := make(map[string]interface{})
	actor["@context"] = []interface{}{
		"https://www.w3.org/ns/activitystreams",
		"https://w3id.org/security/v1",
		"https://w3id.org/security/multikey/v1",
	}
	actor["id"] = account.ActorURI
	actor["type"] = "Application"
	actor["preferredUsername"] = account.Username
	actor["name"] = account.DisplayName
	actor["publicKey"] = actorPublicKeyBlock(account.ActorURI, account.PublicKeyPem)
	if assertion := actorAssertionMethod(account.ActorURI, account.Ed25519PublicKeyPem); assertion != nil {
		actor["assertionMethod"] = assertion
	}
	actor["endpoints"] = map[string]interface{}{
		"sharedInbox": "https://" + h.domain + "/inbox",
	}
	actor["inbox"] = "https://" + h.domain + "/inbox"
	actor["outbox"] = "https://" + h.domain + "/outbox"
	return actor
}

// InboxGet handles GET to the inbox (authorized fetch).
func (h *Handler) InboxGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context":   "https://www.w3.org/ns/activitystreams",
		"id":         "https://" + h.domain + r.URL.Path,
		"type":       "OrderedCollection",
		"totalItems": 0,
		"first":      nil,
	})
}

// InboxPost handles incoming ActivityPub activities.
// Verifies the HTTP signature, validates the activity, and enqueues it for async processing.
func (h *Handler) InboxPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.verifier == nil {
		// Every activity below is only processed inside the signature-verified
		// branch; without a verifier there is nothing to check the signature
		// against, so fail loudly instead of silently accepting-and-dropping
		// every inbound activity.
		slog.Error("inbox: no signature verifier configured")
		jsonError(w, http.StatusInternalServerError, "inbox not configured")
		return
	}

	// Limit request body to 1 MB to prevent memory exhaustion
	const maxActivitySize = 1 << 20 // 1 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxActivitySize)

	// Read body once for both verification and parsing
	body, err := readBody(r)
	if err != nil {
		slog.Warn("inbox: failed to read body", "error", err)
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}

	// Verify HTTP signature first
	if h.verifier != nil {
		if h.requestSignerBlocked(r) {
			jsonError(w, http.StatusForbidden, "federation signer blocked")
			return
		}
		keyOwner, err := h.verifier.VerifyRequestWithBody(r, body)
		if err != nil {
			// FEP-8b32: an Object Integrity Proof may authenticate the
			// activity even when the HTTP signature is absent or fails, and
			// per the FEP it takes precedence over the HTTP signature.
			var activity map[string]interface{}
			if json.Unmarshal(body, &activity) == nil {
				if proofOwner, oipErr := h.tryObjectIntegrityProofFallback(ctx, activity); oipErr == nil {
					slog.Info("inbox: accepted activity via object integrity proof",
						"owner", proofOwner,
						"http_signature_error", err,
					)
					keyOwner = proofOwner
					err = nil
				}
			}
			if err != nil {
				slog.Warn("inbox: signature verification failed", "error", err)
				jsonError(w, http.StatusUnauthorized, "signature verification failed")
				return
			}
		}
		if u, err := url.Parse(ExtractKeyOwner(keyOwner)); err != nil || hostInboundBlocked(ctx, h.store, u.Hostname(), h.localDomain, h.domain) {
			jsonError(w, http.StatusForbidden, "federation signer blocked")
			return
		}

		// Collection-Synchronization: the sender attaches a digest of the
		// followers of the signing actor that it believes are hosted on our
		// domain. Comparing it is one indexed local query; only a mismatch
		// enqueues async work (the actual reconciliation fetch).
		if header := r.Header.Get("Collection-Synchronization"); header != "" {
			if target, syncURL, mismatch := evaluateCollectionSync(ctx, h.store, ExtractKeyOwner(keyOwner), header); mismatch {
				if h.federator == nil || h.federator.checkFederationURI(ctx, syncURL) == nil {
					enqueueCollectionSync(ctx, h.store, target, syncURL)
				}
			}
		}

		// Parse activity from the already-read body
		var activity map[string]interface{}
		if err := json.Unmarshal(body, &activity); err != nil {
			slog.Warn("inbox: invalid JSON", "error", err)
			jsonError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		activityType, _ := activity["type"].(string)
		activityID, _ := activity["id"].(string)
		if activityID == "" || activityType == "" {
			slog.Warn("inbox: activity missing id or type", "type", activityType, "id", activityID)
			jsonError(w, http.StatusBadRequest, "activity missing id or type")
			return
		}

		// Drop what the processor has no branch for before it reaches the
		// queue. The signature is already verified at this point, so the
		// remaining work (actor-mismatch fallbacks, the job row, the dequeue
		// and completion updates) would all be spent on an activity destined
		// to be discarded. 202 is still correct: we accepted it and chose to
		// do nothing with it.
		if !handledActivityType(activityType) {
			slog.Debug("inbox: ignoring unhandled activity type", "type", activityType, "id", activityID)
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
			return
		}

		// Anti-impersonation: verify the signing key owner matches the activity actor
		actor, _ := activity["actor"].(string)
		if actor != "" {
			if u, err := url.Parse(actor); err != nil || hostInboundBlocked(ctx, h.store, u.Hostname(), h.localDomain, h.domain) {
				jsonError(w, http.StatusForbidden, "federation actor blocked")
				return
			}
			keyActor := ExtractKeyOwner(keyOwner)
			if keyActor != actor {
				// The HTTP signer isn't the declared actor: this is not
				// necessarily spoofing. It's also the normal shape of a
				// legitimate forward - e.g. someone you follow boosted a
				// post whose author later edited or deleted it, and the
				// booster's server relays that Update/Delete to you signed
				// with its own key. An embedded Object Integrity Proof
				// (FEP-8b32, which per spec takes precedence) or a Linked
				// Data Signature can authenticate the true author despite
				// that, so try them before rejecting outright.
				_, authErr := h.tryObjectIntegrityProofFallback(ctx, activity)
				if authErr != nil {
					authErr = h.tryLinkedDataSignatureFallback(ctx, activity)
				}
				if authErr != nil {
					slog.Warn("inbox: actor mismatch", "key_owner", keyActor, "activity_actor", actor, "type", activityType, "id", activityID, "auth_error", authErr)
					slog.Debug("inbox: actor mismatch payload", "body", truncateForLog(body, 4096))
					jsonError(w, http.StatusForbidden, "actor mismatch")
					return
				}
				slog.Info("inbox: accepted forwarded activity via proof", "key_owner", keyActor, "activity_actor", actor, "type", activityType, "id", activityID)
			}
		}

		slog.Info("inbox: received activity",
			"type", activityType,
			"id", activityID,
		)

		// Enqueue for async processing. The raw body is enqueued as-is: it was
		// already validated as JSON above, and re-marshaling the parsed map on
		// the synchronous HTTP path only costs CPU (the async processor parses
		// from raw anyway).
		job := &models.Job{
			Kind:    "inbox",
			Payload: string(body),
			RunAt:   time.Now(),
		}
		enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := h.store.EnqueueJob(enqueueCtx, job); err != nil {
			slog.Error("inbox: failed to enqueue job", "error", err)
			jsonError(w, http.StatusServiceUnavailable, "inbox temporarily unavailable")
			return
		}
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

func (h *Handler) requestSignerBlocked(r *http.Request) bool {
	params := parseSignatureHeader(r.Header.Get("Signature"))
	if params == nil {
		return false
	}
	u, err := url.Parse(ExtractKeyOwner(params["keyId"]))
	return err == nil && u.Hostname() != "" && hostInboundBlocked(r.Context(), h.store, u.Hostname(), h.localDomain, h.domain)
}

// SharedInboxPost handles incoming activities to the shared inbox.
func (h *Handler) SharedInboxPost(w http.ResponseWriter, r *http.Request) {
	h.InboxPost(w, r)
}

// OutboxGet returns a user's outbox collection.
func (h *Handler) OutboxGet(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		jsonError(w, http.StatusNotFound, "account not found")
		return
	}

	if r.URL.Query().Get("page") == "" {
		count, _ := h.store.CountAccountStatuses(r.Context(), account.ID)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"@context":   "https://www.w3.org/ns/activitystreams",
			"id":         account.OutboxURI,
			"type":       "OrderedCollection",
			"totalItems": count,
			"first":      account.OutboxURI + "?page=true",
		})
		return
	}

	statuses, err := h.store.GetAccountStatuses(r.Context(), "", account.ID, nil, nil, nil, 20, false, false, false, false)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "error fetching outbox")
		return
	}

	items := make([]map[string]interface{}, 0, len(statuses))
	for _, s := range statuses {
		// Collections are unauthenticated reads: local-only posts must never
		// federate, and followers-only/direct posts have no reader check here.
		if !federatesPublicly(s.Visibility) {
			continue
		}
		if s.BoostOfID != nil {
			boosted, err := h.store.GetStatus(r.Context(), *s.BoostOfID)
			if err != nil {
				slog.Warn("federation: skipping boost with missing target", "boost", s.ID, "target", *s.BoostOfID, "error", err)
				continue
			}
			items = append(items, BuildAnnounceAS2(h.domain, s, account, boosted.URI))
			continue
		}
		items = append(items, h.statusToCreateAS2(s, account))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           account.OutboxURI + "?page=true",
		"type":         "OrderedCollectionPage",
		"partOf":       account.OutboxURI,
		"orderedItems": items,
	})
}

// Privacy is checked even on a response-cache hit. A preference change on
// another process must not expose the old collection until its cache expires.
func (h *Handler) cachedFollowList(followers bool, next http.HandlerFunc) http.HandlerFunc {
	cached := h.cachedGET(false, next)
	return func(w http.ResponseWriter, r *http.Request) {
		account, err := h.store.GetAccountByUsernameDomain(r.Context(), chi.URLParam(r, "username"), nil)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				jsonError(w, http.StatusNotFound, "account not found")
			} else {
				jsonError(w, http.StatusInternalServerError, "error fetching account")
			}
			return
		}
		if followers && account.HideFollowers || !followers && account.HideFollowing {
			next(w, r)
			return
		}
		cached(w, r)
	}
}

// FollowersGet returns a user's followers collection.
func (h *Handler) FollowersGet(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "account not found")
		} else {
			jsonError(w, http.StatusInternalServerError, "error fetching account")
		}
		return
	}

	// Mastodon treats a collection without first as private.
	if account.HideFollowers {
		if r.URL.Query().Get("page") == "" {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"@context":   "https://www.w3.org/ns/activitystreams",
				"id":         account.FollowersURI,
				"type":       "OrderedCollection",
				"totalItems": 0,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"@context":     "https://www.w3.org/ns/activitystreams",
			"id":           account.FollowersURI + "?page=true",
			"type":         "OrderedCollectionPage",
			"partOf":       account.FollowersURI,
			"orderedItems": []string{},
		})
		return
	}

	if r.URL.Query().Get("page") == "" {
		count, err := h.store.CountFollowers(r.Context(), account.ID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "error counting account list")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"@context":   "https://www.w3.org/ns/activitystreams",
			"id":         account.FollowersURI,
			"type":       "OrderedCollection",
			"totalItems": count,
			"first":      account.FollowersURI + "?page=true",
		})
		return
	}

	followers, err := h.store.ListFollowers(r.Context(), account.ID, nil, 100)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "error fetching followers")
		return
	}

	items := make([]string, len(followers))
	for i, f := range followers {
		items[i] = f.ActorURI
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           account.FollowersURI + "?page=true",
		"type":         "OrderedCollectionPage",
		"partOf":       account.FollowersURI,
		"orderedItems": items,
	})
}

// FollowingGet returns a user's following collection.
func (h *Handler) FollowingGet(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "account not found")
		} else {
			jsonError(w, http.StatusInternalServerError, "error fetching account")
		}
		return
	}

	// Mastodon treats a collection without first as private.
	if account.HideFollowing {
		if r.URL.Query().Get("page") == "" {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"@context":   "https://www.w3.org/ns/activitystreams",
				"id":         account.FollowingURI,
				"type":       "OrderedCollection",
				"totalItems": 0,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"@context":     "https://www.w3.org/ns/activitystreams",
			"id":           account.FollowingURI + "?page=true",
			"type":         "OrderedCollectionPage",
			"partOf":       account.FollowingURI,
			"orderedItems": []string{},
		})
		return
	}

	if r.URL.Query().Get("page") == "" {
		count, err := h.store.CountFollowing(r.Context(), account.ID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "error counting account list")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"@context":   "https://www.w3.org/ns/activitystreams",
			"id":         account.FollowingURI,
			"type":       "OrderedCollection",
			"totalItems": count,
			"first":      account.FollowingURI + "?page=true",
		})
		return
	}

	following, err := h.store.ListFollowing(r.Context(), account.ID, nil, 100)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "error fetching following")
		return
	}

	items := make([]string, len(following))
	for i, f := range following {
		items[i] = f.ActorURI
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           account.FollowingURI + "?page=true",
		"type":         "OrderedCollectionPage",
		"partOf":       account.FollowingURI,
		"orderedItems": items,
	})
}

// Featured Collections

// FeaturedGet returns the user's pinned posts.
func (h *Handler) FeaturedGet(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	account, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		jsonError(w, http.StatusNotFound, "account not found")
		return
	}

	pinned, err := h.store.GetPinnedStatuses(r.Context(), account.ID, 100)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "error fetching pinned")
		return
	}

	orderedItems := make([]map[string]interface{}, 0, len(pinned))
	for _, s := range pinned {
		// Collections are unauthenticated reads: local-only posts must never
		// federate, and followers-only/direct posts have no reader check here.
		if !federatesPublicly(s.Visibility) {
			continue
		}
		orderedItems = append(orderedItems, h.statusToNoteAS2(s, account))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           "https://" + h.domain + "/users/" + username + "/collections/featured",
		"type":         "OrderedCollection",
		"totalItems":   len(orderedItems),
		"orderedItems": orderedItems,
	})
}

// FeaturedTagsGet returns the user's featured tags (empty collection for now).
func (h *Handler) FeaturedTagsGet(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	_, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		jsonError(w, http.StatusNotFound, "account not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           "https://" + h.domain + "/users/" + username + "/collections/tags",
		"type":         "OrderedCollection",
		"totalItems":   0,
		"orderedItems": []interface{}{},
	})
}

// ObjectGet serves an ActivityPub object by ID.
func (h *Handler) ObjectGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "missing id")
		return
	}
	status, err := h.store.GetStatus(r.Context(), id)
	if err == nil {
		// Only world-readable posts are served by URI. There is no per-follower
		// authorization on this endpoint (secure mode only proves the fetcher
		// signed the request, not that it may read a followers-only post), so
		// anything narrower than unlisted stays unfetchable.
		if !federatesPublicly(status.Visibility) {
			jsonError(w, http.StatusNotFound, "not found")
			return
		}
		// A browser landed on the object URI (clients such as snac hand out
		// the AP id as the post's link). Send it to the HTML permalink, the
		// same way ActorGet does; otherwise it downloads the AS2 JSON as a
		// file, since nosniff forbids rendering application/activity+json.
		if wantsHTML(r) && status.URL != "" {
			if u, err := url.Parse(status.URL); err == nil && u.User == nil && u.Scheme == "https" &&
				strings.EqualFold(u.Host, h.domain) {
				http.Redirect(w, r, u.String(), http.StatusFound)
				return
			}
		}
		account, err := h.store.GetAccount(r.Context(), status.AccountID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "error fetching account")
			return
		}
		if status.BoostOfID != nil {
			boosted, err := h.store.GetStatus(r.Context(), *status.BoostOfID)
			if err != nil {
				jsonError(w, http.StatusNotFound, "not found")
				return
			}
			writeJSON(w, http.StatusOK, BuildAnnounceAS2(h.domain, status, account, boosted.URI))
			return
		}
		writeJSON(w, http.StatusOK, h.statusToNoteAS2(status, account))
		return
	}

	// Not a status - check for an account
	account, err2 := h.store.GetAccount(r.Context(), id)
	if err2 == nil {
		if wantsHTML(r) {
			http.Redirect(w, r, "/@"+account.Username, http.StatusFound)
			return
		}
		writeJSON(w, http.StatusOK, h.actorToAS2(r.Context(), account))
		return
	}

	// Neither status nor account found - check for a tombstone
	// The tombstone is indexed by the full URI, so reconstruct it from the domain + id
	objectURI := fmt.Sprintf("https://%s/objects/%s", h.domain, id)
	tombstone, err := h.store.GetTombstoneByURI(r.Context(), objectURI)
	if err == nil {
		h.serveTombstone(w, tombstone)
		return
	}

	jsonError(w, http.StatusNotFound, "not found")
}

// QuoteAuthorizationGet serves a QuoteAuthorization (FEP-044f) we granted, so a
// remote server can dereference the quoteAuthorization referenced by a quote
// post and verify the quote was approved.
func (h *Handler) QuoteAuthorizationGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	authID := chi.URLParam(r, "authID")
	authURI := fmt.Sprintf("https://%s/objects/%s/quote_authorizations/%s", h.domain, id, authID)

	qa, err := h.store.GetQuoteAuthorizationByURI(r.Context(), authURI)
	if err != nil {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	quoted, err := h.store.GetStatus(r.Context(), qa.QuotedStatusID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	author, err := h.store.GetAccount(r.Context(), quoted.AccountID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "error fetching author")
		return
	}
	writeJSON(w, http.StatusOK, buildQuoteAuthorizationAS2(qa.URI, author.ActorURI, qa.QuotingURI, quoted.URI))
}

// ObjectLikesGet exposes the public Likes known for a local status. Items are
// embedded so remote servers can identify the actors without dereferencing
// littleFedi's internal activity IDs.
func (h *Handler) ObjectLikesGet(w http.ResponseWriter, r *http.Request) {
	h.objectInteractionsGet(w, r, false)
}

// ObjectSharesGet exposes the public Announces known for a local status.
func (h *Handler) ObjectSharesGet(w http.ResponseWriter, r *http.Request) {
	h.objectInteractionsGet(w, r, true)
}

func (h *Handler) ObjectEmojiReactionsGet(w http.ResponseWriter, r *http.Request) {
	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil || !federatesPublicly(status.Visibility) {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	reactions, err := h.store.ListEmojiReactions(r.Context(), status.ID, maxInteractionActors)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "error fetching reactions")
		return
	}
	accountIDs := make([]string, 0, len(reactions))
	for _, reaction := range reactions {
		accountIDs = append(accountIDs, reaction.AccountID)
	}
	accounts, _ := h.store.GetAccountsByIDs(r.Context(), accountIDs)
	summaries, _ := h.store.GetEmojiReactionSummaries(r.Context(), []string{status.ID}, "", maxInteractionActors)
	byKey := make(map[string]models.EmojiReactionSummary, len(summaries[status.ID]))
	total := 0
	for _, summary := range summaries[status.ID] {
		byKey[summary.Key] = summary
		total += summary.Count
	}
	items := make([]map[string]interface{}, 0, len(reactions))
	for _, reaction := range reactions {
		account := accounts[reaction.AccountID]
		if account == nil {
			continue
		}
		item := map[string]interface{}{"id": reaction.URI, "type": "EmojiReact", "actor": account.ActorURI, "object": status.URI, "content": reaction.Content}
		if custom := byKey[reaction.Key].Emoji; custom != nil {
			customURL := custom.RemoteURL
			if customURL == "" {
				customURL = custom.URL
			}
			item["tag"] = []map[string]interface{}{{"id": customURL, "type": "Emoji", "name": reaction.Content, "icon": map[string]interface{}{"type": "Image", "url": customURL}}}
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context": []interface{}{"https://www.w3.org/ns/activitystreams", map[string]string{"litepub": "http://litepub.social/ns#", "EmojiReact": "litepub:EmojiReact", "toot": "http://joinmastodon.org/ns#", "Emoji": "toot:Emoji"}},
		"id":       fmt.Sprintf("https://%s/objects/%s/emoji_reactions", h.domain, status.ID), "type": "OrderedCollection",
		"totalItems": total, "orderedItems": items,
	})
}

func (h *Handler) objectInteractionsGet(w http.ResponseWriter, r *http.Request, shares bool) {
	status, err := h.store.GetStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil || (status.Visibility != models.VisibilityPublic && status.Visibility != models.VisibilityUnlisted) {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	var accounts []*models.Account
	collectionName := "likes"
	activityType := "Like"
	if shares {
		accounts, err = h.store.ListReblogAccounts(r.Context(), status.ID, maxInteractionActors)
		collectionName = "shares"
		activityType = "Announce"
	} else {
		accounts, err = h.store.ListFavouriteAccounts(r.Context(), status.ID, maxInteractionActors)
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "error fetching interactions")
		return
	}
	items := make([]map[string]interface{}, 0, len(accounts))
	for _, account := range accounts {
		items = append(items, map[string]interface{}{
			"id":     fmt.Sprintf("%s#%s/%s", account.ActorURI, collectionName, status.ID),
			"type":   activityType,
			"actor":  account.ActorURI,
			"object": status.URI,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           fmt.Sprintf("https://%s/objects/%s/%s", h.domain, status.ID, collectionName),
		"type":         "OrderedCollection",
		"totalItems":   len(items),
		"orderedItems": items,
	})
}

// Well-known / Discovery

// WebFinger handles WebFinger lookups.
func (h *Handler) WebFinger(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")
	if resource == "" {
		jsonError(w, http.StatusBadRequest, "missing resource parameter")
		return
	}

	if !strings.HasPrefix(resource, "acct:") {
		jsonError(w, http.StatusBadRequest, "unsupported resource format")
		return
	}

	acct := strings.TrimPrefix(resource, "acct:")
	parts := strings.SplitN(acct, "@", 2)
	if len(parts) != 2 {
		jsonError(w, http.StatusBadRequest, "invalid acct format")
		return
	}

	username := parts[0]
	domain := parts[1]

	if !strings.EqualFold(domain, h.localDomain) && !strings.EqualFold(domain, h.domain) {
		jsonError(w, http.StatusNotFound, "unknown domain")
		return
	}

	_, err := h.store.GetAccountByUsernameDomain(r.Context(), username, nil)
	if err != nil {
		slog.Warn("webfinger: account not found", "username", username)
		jsonError(w, http.StatusNotFound, "account not found")
		return
	}

	profileURL := fmt.Sprintf("https://%s/users/%s", h.domain, username)
	jrd := map[string]interface{}{
		"subject": "acct:" + username + "@" + h.localDomain,
		"aliases": []string{
			profileURL,
			fmt.Sprintf("https://%s/@%s", h.domain, username),
		},
		"links": []map[string]interface{}{
			{
				"rel":  "http://webfinger.net/rel/profile-page",
				"type": "text/html",
				"href": fmt.Sprintf("https://%s/@%s", h.domain, username),
			},
			{
				"rel":  "self",
				"type": "application/activity+json",
				"href": profileURL,
			},
		},
	}

	writeJRD(w, http.StatusOK, jrd)
}

// NodeInfoWellKnown serves the nodeinfo well-known endpoint.
func (h *Handler) NodeInfoWellKnown(w http.ResponseWriter, r *http.Request) {
	writePlainJSON(w, http.StatusOK, map[string]interface{}{
		"links": []map[string]interface{}{
			{
				"rel":  "http://nodeinfo.diaspora.software/ns/schema/2.1",
				"href": fmt.Sprintf("https://%s/nodeinfo/2.1", h.domain),
			},
		},
	})
}

// HostMeta serves the host-meta XRD document.
func (h *Handler) HostMeta(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xrd+xml")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<XRD xmlns="http://docs.oasis-open.org/ns/xri/xrd-1.0">
  <Link rel="lrdd" template="https://%s/.well-known/webfinger?resource={uri}"/>
</XRD>`, h.domain)
}

// NodeInfo serves the NodeInfo 2.1 document.
func (h *Handler) NodeInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	localUserCount, _ := h.store.CountLocalAccounts(ctx)
	statusCount, _ := h.store.CountLocalStatuses(ctx)
	meta := instancemeta.Load(ctx, h.store, h.metaDefaults)
	nodeName := h.instanceName
	if nodeName == "" {
		nodeName = "littleFedi"
	}

	writePlainJSON(w, http.StatusOK, map[string]interface{}{
		"version": "2.1",
		"software": map[string]interface{}{
			"name":    "littlefedi",
			"version": version.Version,
		},
		"protocols": []string{"activitypub"},
		"services": map[string]interface{}{
			"inbound":  []string{},
			"outbound": []string{},
		},
		"openRegistrations": h.openRegistrations,
		"usage": map[string]interface{}{
			"users": map[string]interface{}{
				"total": localUserCount,
			},
			"localPosts": statusCount,
		},
		"metadata": map[string]interface{}{
			"nodeName":        nodeName,
			"nodeDescription": meta.ShortDescription,
			"features":        []string{"pleroma_emoji_reactions"},
		},
	})
}

// AS2 Conversion Helpers

func (h *Handler) actorToAS2(ctx context.Context, account *models.Account) map[string]interface{} {
	username := account.Username

	// Build the @context array separately to avoid complex literal nesting issues
	contextMap := make(map[string]interface{})
	contextMap["manuallyApprovesFollowers"] = "as:manuallyApprovesFollowers"
	contextMap["toot"] = "http://joinmastodon.org/ns#"

	featuredMap := make(map[string]interface{})
	featuredMap["@id"] = "toot:featured"
	featuredMap["@type"] = "@id"
	contextMap["featured"] = featuredMap

	featuredTagsMap := make(map[string]interface{})
	featuredTagsMap["@id"] = "toot:featuredTags"
	featuredTagsMap["@type"] = "@id"
	contextMap["featuredTags"] = featuredTagsMap

	alsoKnownAsMap := make(map[string]interface{})
	alsoKnownAsMap["@id"] = "as:alsoKnownAs"
	alsoKnownAsMap["@type"] = "@id"
	contextMap["alsoKnownAs"] = alsoKnownAsMap

	movedToMap := make(map[string]interface{})
	movedToMap["@id"] = "as:movedTo"
	movedToMap["@type"] = "@id"
	contextMap["movedTo"] = movedToMap

	contextMap["schema"] = "http://schema.org#"
	contextMap["PropertyValue"] = "schema:PropertyValue"
	contextMap["value"] = "schema:value"

	actor := make(map[string]interface{})
	actor["@context"] = []interface{}{
		"https://www.w3.org/ns/activitystreams",
		"https://w3id.org/security/v1",
		"https://w3id.org/security/multikey/v1",
		contextMap,
		"https://purl.archive.org/socialweb/webfinger",
	}
	actor["id"] = fmt.Sprintf("https://%s/users/%s", h.domain, username)
	if account.IsBot {
		actor["type"] = "Service"
	} else {
		actor["type"] = "Person"
	}
	actor["following"] = fmt.Sprintf("https://%s/users/%s/following", h.domain, username)
	actor["followers"] = fmt.Sprintf("https://%s/users/%s/followers", h.domain, username)
	actor["inbox"] = fmt.Sprintf("https://%s/users/%s/inbox", h.domain, username)
	actor["outbox"] = fmt.Sprintf("https://%s/users/%s/outbox", h.domain, username)
	actor["featured"] = fmt.Sprintf("https://%s/users/%s/collections/featured", h.domain, username)
	actor["featuredTags"] = fmt.Sprintf("https://%s/users/%s/collections/tags", h.domain, username)
	actor["preferredUsername"] = username
	actor["webfinger"] = username + "@" + h.localDomain
	actor["name"] = account.DisplayName
	actor["summary"] = contentlib.ResolveSocialLinks(account.Note, "https://"+h.domain, nil)
	actor["url"] = fmt.Sprintf("https://%s/@%s", h.domain, username)
	actor["manuallyApprovesFollowers"] = account.ManuallyApprovesFollowers
	actor["published"] = account.CreatedAt.Format("2006-01-02T15:04:05Z")

	actor["publicKey"] = actorPublicKeyBlock(
		fmt.Sprintf("https://%s/users/%s", h.domain, username),
		account.PublicKeyPem,
	)
	if assertion := actorAssertionMethod(
		fmt.Sprintf("https://%s/users/%s", h.domain, username),
		account.Ed25519PublicKeyPem,
	); assertion != nil {
		actor["assertionMethod"] = assertion
	}
	actor["endpoints"] = map[string]interface{}{
		"sharedInbox": fmt.Sprintf("https://%s/inbox", h.domain),
	}

	// Avatar and header are stored as site-relative paths for local disk
	// storage (e.g. /media/...); remote servers fetching this actor need
	// absolute URLs, matching what the Update activity federates.
	if account.Avatar != "" {
		actor["icon"] = map[string]interface{}{
			"type":      "Image",
			"mediaType": "image/jpeg",
			"url":       h.absoluteMediaURL(account.Avatar),
		}
	}

	if account.Header != "" {
		actor["image"] = map[string]interface{}{
			"type":      "Image",
			"mediaType": "image/jpeg",
			"url":       h.absoluteMediaURL(account.Header),
		}
	}

	// Count fields (consumed by Mastodon and other fedi software for profile stats).
	followersCount, followingCount, statusesCount := h.localActorCounts(ctx, account)
	if account.HideFollowers {
		followersCount = 0
	}
	if account.HideFollowing {
		followingCount = 0
	}
	actor["followersCount"] = followersCount
	actor["followingCount"] = followingCount
	actor["statusesCount"] = statusesCount

	// Migration metadata
	if aliases := accountmigration.AccountAliasesForAP(account); aliases != nil {
		actor["alsoKnownAs"] = aliases
	}
	if account.MovedToURI != "" {
		actor["movedTo"] = account.MovedToURI
	}

	return actor
}

// absoluteMediaURL turns a possibly site-relative media path into an absolute
// URL rooted at the web domain. URLs that are already absolute (e.g. an S3
// backend) are returned unchanged.
func (h *Handler) absoluteMediaURL(pathOrURL string) string {
	return absoluteMediaURL(h.domain, pathOrURL)
}

func absoluteMediaURL(domain, pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	return "https://" + domain + pathOrURL
}

func (h *Handler) localActorCounts(ctx context.Context, account *models.Account) (int, int, int) {
	if account.Domain != nil {
		return account.FollowersCount, account.FollowingCount, account.StatusesCount
	}
	followersCount, err := h.store.CountFollowers(ctx, account.ID)
	if err != nil {
		followersCount = account.FollowersCount
	}
	followingCount, err := h.store.CountFollowing(ctx, account.ID)
	if err != nil {
		followingCount = account.FollowingCount
	}
	statusesCount, err := h.store.CountAccountStatuses(ctx, account.ID)
	if err != nil {
		statusesCount = account.StatusesCount
	}
	return followersCount, followingCount, statusesCount
}

func (h *Handler) statusToNoteAS2(status *models.Status, account *models.Account) map[string]interface{} {
	// Storage status reads leave relations unloaded. Object and
	// collection endpoints must nevertheless expose the same tag and media
	// metadata as the Create activity sent at publication time: a remote server
	// that resolves the object by URI (relay Announce, thread crawl, boost)
	// would otherwise render the post as if it had no attachments at all.
	if media, err := h.store.GetMediaForStatus(context.Background(), status.ID); err == nil {
		status.MediaAttachments = convertMediaAttachments(media)
	}
	if polls, err := h.store.GetPollsForStatuses(context.Background(), []string{status.ID}, ""); err == nil {
		status.Poll = polls[status.ID]
	}
	mentions, _ := h.store.GetMentionsForStatus(context.Background(), status.ID)
	status.Mentions = make([]models.Mention, len(mentions))
	mentionURLs := make(map[string]string, len(mentions))
	for i, m := range mentions {
		status.Mentions[i] = *m
		mentionURLs[m.Acct] = m.URL
	}
	tags, _ := h.store.GetTagsForStatus(context.Background(), status.ID)
	status.Tags = make([]models.Tag, len(tags))
	for i, tag := range tags {
		status.Tags[i] = *tag
	}
	attachStoredPreview(context.Background(), h.store, status)
	status.ContentHTML = contentlib.ResolveSocialLinks(status.ContentHTML, "https://"+h.domain, mentionURLs)
	return buildNoteAS2(h.domain, status, account)
}

// federatesPublicly reports whether a status may be handed to any server that
// asks for it. Local visibilities never leave the instance at all; direct and
// followers-only posts reach their audience through delivery, not through
// unauthenticated object and collection reads.
func federatesPublicly(v models.Visibility) bool {
	return v == models.VisibilityPublic || v == models.VisibilityUnlisted
}

// noteAudience derives the AS2 to/cc addressing from the status visibility.
// Addressing every served Note as Public would tell the fetching server that a
// followers-only post is world-readable, so it must track the real visibility.
func noteAudience(domain string, status *models.Status, account *models.Account) (to, cc []string) {
	followers := account.FollowersURI
	if followers == "" {
		followers = fmt.Sprintf("https://%s/users/%s/followers", domain, account.Username)
	}
	mentioned := make([]string, 0, len(status.Mentions))
	for _, mention := range status.Mentions {
		if mention.URL != "" {
			mentioned = append(mentioned, mention.URL)
		}
	}
	switch status.Visibility {
	case models.VisibilityUnlisted, models.VisibilityLocalUnlisted:
		return []string{}, append([]string{followers, publicURI}, mentioned...)
	case models.VisibilityPrivate:
		return append([]string{followers}, mentioned...), []string{}
	case models.VisibilityDirect:
		if len(mentioned) == 0 {
			mentioned = []string{account.ActorURI}
		}
		return mentioned, []string{}
	default:
		return []string{publicURI}, append([]string{followers}, mentioned...)
	}
}

// BuildAnnounceAS2 renders the activity represented by a stored boost.
func BuildAnnounceAS2(domain string, boost *models.Status, account *models.Account, objectURI string) map[string]interface{} {
	to, cc := noteAudience(domain, boost, account)
	return map[string]interface{}{
		"@context":  "https://www.w3.org/ns/activitystreams",
		"id":        boost.URI,
		"type":      "Announce",
		"actor":     account.ActorURI,
		"object":    objectURI,
		"published": boost.CreatedAt.UTC().Format(time.RFC3339),
		"to":        to,
		"cc":        cc,
	}
}

// buildNoteAS2 renders a local status as an AS2 Note. It is a package function
// (not just a Handler method) so the inbox processor can reuse it when sending
// an Update after a quote authorization is granted.
func buildNoteAS2(domain string, status *models.Status, account *models.Account) map[string]interface{} {
	note := make(map[string]interface{})
	note["@context"] = "https://www.w3.org/ns/activitystreams"
	note["id"] = status.URI
	note["type"] = "Note"
	note["published"] = status.CreatedAt.Format("2006-01-02T15:04:05Z")
	note["url"] = status.URL
	note["attributedTo"] = account.ActorURI
	note["content"] = status.ContentHTML
	if contentMap := languageMap(status.Language, status.ContentHTML); contentMap != nil {
		note["contentMap"] = contentMap
	}
	note["to"], note["cc"] = noteAudience(domain, status, account)
	note["likes"] = fmt.Sprintf("https://%s/objects/%s/likes", domain, status.ID)
	note["shares"] = fmt.Sprintf("https://%s/objects/%s/shares", domain, status.ID)
	if status.Poll != nil {
		note["type"] = "Question"
		key := "oneOf"
		if status.Poll.Multiple {
			key = "anyOf"
		}
		options := make([]map[string]any, 0, len(status.Poll.Options))
		for _, option := range status.Poll.Options {
			options = append(options, map[string]any{
				"type": "Note",
				"name": option.Title,
				"replies": map[string]any{
					"type":       "Collection",
					"totalItems": option.VotesCount,
				},
			})
		}
		note[key] = options
		if status.Poll.ExpiresAt != nil {
			note["endTime"] = status.Poll.ExpiresAt.Format(time.RFC3339)
		}
	}
	if status.EditedAt != nil {
		note["updated"] = status.EditedAt.Format(time.RFC3339)
	}
	if status.Visibility == models.VisibilityPublic || status.Visibility == models.VisibilityUnlisted {
		note["@context"] = []interface{}{"https://www.w3.org/ns/activitystreams", map[string]string{"emojiReactions": "http://fedibird.com/ns#emojiReactions"}}
		note["emojiReactions"] = fmt.Sprintf("https://%s/objects/%s/emoji_reactions", domain, status.ID)
	}

	if status.Summary != "" {
		note["summary"] = status.Summary
		if summaryMap := languageMap(status.Language, status.Summary); summaryMap != nil {
			note["summaryMap"] = summaryMap
		}
	}
	if status.Sensitive {
		note["sensitive"] = true
	}
	// Advertise the original source so Markdown-aware servers can show it.
	if status.ContentSource != "" {
		mediaType := status.ContentType
		if mediaType != contentlib.ContentTypeMarkdown {
			mediaType = contentlib.ContentTypePlain
		}
		note["source"] = map[string]interface{}{
			"content":   status.ContentSource,
			"mediaType": mediaType,
		}
	}
	if status.InReplyToURI != nil && *status.InReplyToURI != "" {
		note["inReplyTo"] = *status.InReplyToURI
	}

	// Quote post (FEP-044f) and this author's quote interaction policy. Setting
	// interactionPolicy requires the extended @context, applied unconditionally.
	note["@context"] = QuoteJSONLDContext()
	if status.QuoteOfURI != nil && *status.QuoteOfURI != "" {
		ApplyQuoteFields(note, *status.QuoteOfURI, status.QuoteApprovalURI)
	}
	note["interactionPolicy"] = InteractionPolicyFor(account)

	attachments := make([]map[string]interface{}, 0, len(status.MediaAttachments)+1)
	for _, m := range status.MediaAttachments {
		attachments = append(attachments, map[string]interface{}{
			"type":      "Document",
			"mediaType": m.MIMEType,
			"url":       absoluteMediaURL(domain, m.URL),
			"name":      m.Description,
		})
	}
	if link := opengraph.LinkAttachment(status.Preview); link != nil {
		attachments = append(attachments, link)
	}
	if len(attachments) > 0 {
		note["attachment"] = attachments
	}

	tags := make([]map[string]interface{}, 0)
	for _, m := range status.Mentions {
		tags = append(tags, map[string]interface{}{
			"type": "Mention",
			"href": m.URL,
			"name": "@" + strings.TrimPrefix(m.Acct, "@"),
		})
	}
	for _, t := range status.Tags {
		tagURL := t.URL
		if strings.HasPrefix(tagURL, "/") {
			tagURL = "https://" + domain + tagURL
		}
		tags = append(tags, map[string]interface{}{
			"type": "Hashtag",
			"href": tagURL,
			"name": "#" + t.Name,
		})
	}
	if len(tags) > 0 {
		note["tag"] = tags
	}

	return note
}

func (h *Handler) statusToCreateAS2(status *models.Status, account *models.Account) map[string]interface{} {
	object := h.statusToNoteAS2(status, account)
	activity := make(map[string]interface{})
	activity["@context"] = "https://www.w3.org/ns/activitystreams"
	activity["id"] = status.URI + "/activity"
	activity["type"] = "Create"
	activity["actor"] = account.ActorURI
	activity["published"] = status.CreatedAt.Format("2006-01-02T15:04:05Z")
	activity["to"] = object["to"]
	activity["cc"] = object["cc"]
	activity["object"] = object
	return activity
}

// Helpers

// serveTombstone writes an ActivityPub Tombstone response with HTTP 410 Gone.
func (h *Handler) serveTombstone(w http.ResponseWriter, t *models.Tombstone) {
	w.Header().Set("Content-Type", "application/activity+json; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"@context":   "https://www.w3.org/ns/activitystreams",
		"id":         t.URI,
		"type":       "Tombstone",
		"formerType": t.FormerType,
		"deleted":    t.DeletedAt.Format(time.RFC3339),
	})
}

// acceptsAP checks if the request accepts ActivityPub JSON.
func acceptsAP(r *http.Request) bool {
	return httpneg.ActivityPubPreferred(strings.Join(r.Header.Values("Accept"), ","))
}

// wantsHTML reports whether the client explicitly asked for a web page. Only
// an explicit text/html counts: fetchers that send no Accept header at all
// must keep getting the ActivityPub representation.
func wantsHTML(r *http.Request) bool {
	return httpneg.HTMLPreferred(strings.Join(r.Header.Values("Accept"), ","))
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/activity+json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeJRD writes a WebFinger JSON Resource Descriptor with the correct
// media type. WebFinger (RFC 7033 §10.2) mandates application/jrd+json;
// servers that perform authorized-fetch account resolution (e.g. GoToSocial)
// reject WebFinger responses served as application/activity+json, which breaks
// inbound signed requests from us with a generic 401.
func writeJRD(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/jrd+json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writePlainJSON writes a generic application/json document. Used for
// non-ActivityPub discovery endpoints such as NodeInfo.
func writePlainJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// readBody reads the request body and resets it for downstream use.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// truncateForLog caps a byte slice for log output so a full-size (up to
// maxActivitySize) payload from an untrusted sender can't flood the log.
func truncateForLog(body []byte, max int) string {
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "...(truncated)"
}

// tryObjectIntegrityProofFallback attempts to authenticate an activity via an
// embedded Object Integrity Proof (FEP-8b32, see oip.go). It is used when the
// HTTP signer is absent or differs from the declared actor. On success it
// returns the proof's verificationMethod, whose owner matches the actor.
// Returns an error (never nil on failure) so the caller can log why.
func (h *Handler) tryObjectIntegrityProofFallback(ctx context.Context, activity map[string]interface{}) (string, error) {
	rawProof, ok := activity["proof"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no proof block")
	}
	verificationMethod, _ := rawProof["verificationMethod"].(string)
	if verificationMethod == "" {
		return "", fmt.Errorf("proof has no verificationMethod")
	}
	creator := ExtractKeyOwner(verificationMethod)

	// Blocked-domain gate before spending CPU on JCS hashing: a blocked or
	// suspended domain shouldn't get a free crypto verification attempt just
	// because it forwarded someone else's activity.
	if u, err := url.Parse(creator); err == nil {
		if hostInboundBlocked(ctx, h.store, strings.ToLower(u.Hostname()), h.localDomain, h.domain) {
			return "", fmt.Errorf("proof creator domain is blocked")
		}
	}

	account, err := h.store.GetAccountByURI(ctx, creator)
	if err != nil {
		if h.federator == nil {
			return "", fmt.Errorf("fetch proof creator: no federator")
		}
		account, err = h.federator.FetchActorLight(ctx, creator)
		if err != nil {
			return "", fmt.Errorf("fetch proof creator: %w", err)
		}
	}
	key, err := PublicKeyPEMForID(account, verificationMethod)
	if err != nil && account.Domain != nil && h.federator != nil {
		if refreshed, refreshErr := h.federator.RefreshActor(ctx, creator, ActorRefreshOptions{Force: true}); refreshErr == nil {
			key, err = PublicKeyPEMForID(refreshed, verificationMethod)
		}
	}
	if err != nil {
		return "", err
	}
	if err := VerifyObjectIntegrityProof(activity, key); err != nil {
		return "", err
	}

	// A valid proof only proves the verification method's owner signed this
	// exact document; it says nothing about whether that owner is who the
	// activity claims to be. Require them to match, as with LD signatures.
	declaredActor, _ := activity["actor"].(string)
	if creator != declaredActor {
		return "", fmt.Errorf("proof creator %q does not match activity actor %q", creator, declaredActor)
	}
	return verificationMethod, nil
}

// tryLinkedDataSignatureFallback attempts to authenticate an activity whose
// HTTP signer doesn't match its declared actor via an embedded Linked Data
// Signature (see ldsig.go). Returns an error (never nil on failure) so the
// caller can log why the fallback didn't apply.
func (h *Handler) tryLinkedDataSignatureFallback(ctx context.Context, activity map[string]interface{}) error {
	if h.verifier == nil {
		return fmt.Errorf("no verifier configured")
	}
	rawSig, _ := activity["signature"].(map[string]interface{})
	if rawSig == nil {
		return fmt.Errorf("no signature block")
	}
	// Check the signature creator's domain before spending CPU on
	// canonicalization/RSA: a blocked/suspended domain shouldn't get a free
	// crypto verification attempt just because it forwarded someone else's
	// activity.
	if creator, _ := rawSig["creator"].(string); creator != "" {
		if u, err := url.Parse(ExtractKeyOwner(creator)); err == nil {
			if hostInboundBlocked(ctx, h.store, strings.ToLower(u.Hostname()), h.localDomain, h.domain) {
				return fmt.Errorf("ld-signature creator domain is blocked")
			}
		}
	}
	return h.verifier.VerifyLinkedDataSignature(activity)
}

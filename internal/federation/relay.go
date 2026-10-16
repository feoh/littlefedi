package federation

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// RelayManager owns the CLI-managed LitePub follow handshake.
type RelayManager struct {
	store     storage.Storage
	federator *Federator
	webDomain string
}

type RelayDetails struct {
	Relay   *models.Relay
	Account *models.Account
	State   models.RelayState
}

func NewRelayManager(store storage.Storage, federator *Federator, webDomain string) *RelayManager {
	return &RelayManager{store: store, federator: federator, webDomain: webDomain}
}

func (m *RelayManager) Add(ctx context.Context, actorURL string) (*RelayDetails, error) {
	if err := m.validateActorURL(actorURL); err != nil {
		return nil, err
	}
	account, err := m.federator.FetchActorLight(ctx, actorURL)
	if err != nil {
		return nil, fmt.Errorf("fetch relay actor: %w", err)
	}
	if err := m.validateAccount(ctx, account); err != nil {
		return nil, err
	}
	relay, err := m.store.GetRelayByAccountID(ctx, account.ID)
	if err != nil {
		relay = &models.Relay{AccountID: account.ID}
		if err := m.store.CreateRelay(ctx, relay); err != nil {
			return nil, fmt.Errorf("save relay: %w", err)
		}
	}
	if err := m.enable(ctx, relay, account); err != nil {
		return nil, err
	}
	return m.details(ctx, relay)
}

func (m *RelayManager) List(ctx context.Context) ([]*RelayDetails, error) {
	relays, err := m.store.ListRelays(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*RelayDetails, 0, len(relays))
	for _, relay := range relays {
		d, err := m.details(ctx, relay)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (m *RelayManager) Status(ctx context.Context, actorURL string) (*RelayDetails, error) {
	relay, err := m.relayByActor(ctx, actorURL)
	if err != nil {
		return nil, err
	}
	return m.details(ctx, relay)
}

func (m *RelayManager) Enable(ctx context.Context, actorURL string) (*RelayDetails, error) {
	relay, err := m.relayByActor(ctx, actorURL)
	if err != nil {
		return nil, err
	}
	account, err := m.store.GetAccount(ctx, relay.AccountID)
	if err != nil {
		return nil, fmt.Errorf("load relay actor: %w", err)
	}
	if err := m.validateAccount(ctx, account); err != nil {
		return nil, err
	}
	if err := m.enable(ctx, relay, account); err != nil {
		return nil, err
	}
	return m.details(ctx, relay)
}

func (m *RelayManager) Disable(ctx context.Context, actorURL string) (*RelayDetails, error) {
	relay, err := m.relayByActor(ctx, actorURL)
	if err != nil {
		return nil, err
	}
	if err := m.disable(ctx, relay); err != nil {
		return nil, err
	}
	return m.details(ctx, relay)
}

func (m *RelayManager) Remove(ctx context.Context, actorURL string) error {
	relay, err := m.relayByActor(ctx, actorURL)
	if err != nil {
		return err
	}
	if err := m.disable(ctx, relay); err != nil {
		return err
	}
	if err := m.store.DeleteRelay(ctx, relay.ID); err != nil {
		return fmt.Errorf("delete relay: %w", err)
	}
	return nil
}

func (m *RelayManager) enable(ctx context.Context, relay *models.Relay, remote *models.Account) error {
	service, err := m.serviceAccount(ctx)
	if err != nil {
		return err
	}
	state, _ := m.store.GetRelayState(ctx, relay.ID)
	if relay.Enabled && state == models.RelayActive {
		return nil
	}
	if relay.Enabled && relay.FollowID != nil {
		if follow, err := m.store.GetFollow(ctx, *relay.FollowID); err == nil &&
			(follow.State == models.FollowPending || follow.State == models.FollowAccepted) {
			return m.queueFollow(ctx, service, remote, follow.URI)
		}
	}
	if relay.FollowID != nil {
		if follow, err := m.store.GetFollow(ctx, *relay.FollowID); err == nil && follow.State == models.FollowRejected {
			if err := m.store.DeleteFollow(ctx, follow.ID); err != nil {
				return fmt.Errorf("delete rejected relay follow: %w", err)
			}
			if reverse, err := m.store.GetFollowByPair(ctx, remote.ID, service.ID); err == nil {
				_ = m.store.DeleteFollow(ctx, reverse.ID)
			}
		} else if err := m.cleanupFollows(ctx, relay, service, remote); err != nil {
			return err
		}
	}
	follow := &models.Follow{
		AccountID:       service.ID,
		TargetAccountID: remote.ID,
		URI:             service.ActorURI + "#follows/" + uuid.NewString(),
		State:           models.FollowPending,
	}
	if err := m.store.CreateFollow(ctx, follow); err != nil {
		return fmt.Errorf("create relay follow: %w", err)
	}
	relay.Enabled = true
	relay.FollowID = &follow.ID
	if err := m.store.UpdateRelay(ctx, relay); err != nil {
		_ = m.store.DeleteFollow(ctx, follow.ID)
		return fmt.Errorf("enable relay: %w", err)
	}
	if err := m.queueFollow(ctx, service, remote, follow.URI); err != nil {
		return fmt.Errorf("queue relay follow (run enable again to retry): %w", err)
	}
	return nil
}

func (m *RelayManager) disable(ctx context.Context, relay *models.Relay) error {
	service, err := m.serviceAccount(ctx)
	if err != nil {
		return err
	}
	remote, err := m.store.GetAccount(ctx, relay.AccountID)
	if err != nil {
		return fmt.Errorf("load relay actor: %w", err)
	}
	if relay.Enabled {
		relay.Enabled = false
		if err := m.store.UpdateRelay(ctx, relay); err != nil {
			return fmt.Errorf("disable relay: %w", err)
		}
	}
	if err := m.cleanupFollows(ctx, relay, service, remote); err != nil {
		return err
	}
	relay.FollowID = nil
	if err := m.store.UpdateRelay(ctx, relay); err != nil {
		return fmt.Errorf("clear relay follow: %w", err)
	}
	return nil
}

func (m *RelayManager) cleanupFollows(ctx context.Context, relay *models.Relay, service, remote *models.Account) error {
	if relay.FollowID != nil {
		if follow, err := m.store.GetFollow(ctx, *relay.FollowID); err == nil {
			undo := map[string]interface{}{
				"@context": "https://www.w3.org/ns/activitystreams",
				"id":       follow.URI + "/undo",
				"type":     "Undo",
				"actor":    service.ActorURI,
				"object": map[string]interface{}{
					"id": follow.URI, "type": "Follow", "actor": service.ActorURI, "object": remote.ActorURI,
				},
			}
			if err := EnqueueDelivery(ctx, m.store, remote.InboxURI, undo); err != nil {
				return fmt.Errorf("queue relay undo (run disable/remove again to retry): %w", err)
			}
			if err := m.store.DeleteFollow(ctx, follow.ID); err != nil {
				return fmt.Errorf("delete outgoing relay follow: %w", err)
			}
		}
	}
	if reverse, err := m.store.GetFollowByPair(ctx, remote.ID, service.ID); err == nil {
		if err := m.store.DeleteFollow(ctx, reverse.ID); err != nil {
			return fmt.Errorf("delete reverse relay follow: %w", err)
		}
	}
	return nil
}

func (m *RelayManager) queueFollow(ctx context.Context, service, remote *models.Account, followURI string) error {
	return EnqueueDelivery(ctx, m.store, remote.InboxURI, map[string]interface{}{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       followURI, "type": "Follow", "actor": service.ActorURI, "object": remote.ActorURI,
	})
}

func (m *RelayManager) relayByActor(ctx context.Context, actorURL string) (*models.Relay, error) {
	if err := m.validateActorURL(actorURL); err != nil {
		return nil, err
	}
	relay, err := m.store.GetRelayByActorURI(ctx, actorURL)
	if err != nil {
		return nil, fmt.Errorf("relay %q not found", actorURL)
	}
	return relay, nil
}

func (m *RelayManager) details(ctx context.Context, relay *models.Relay) (*RelayDetails, error) {
	account, err := m.store.GetAccount(ctx, relay.AccountID)
	if err != nil {
		return nil, fmt.Errorf("load relay actor: %w", err)
	}
	state, err := m.store.GetRelayState(ctx, relay.ID)
	if err != nil {
		return nil, err
	}
	return &RelayDetails{Relay: relay, Account: account, State: state}, nil
}

func (m *RelayManager) serviceAccount(ctx context.Context) (*models.Account, error) {
	account, err := m.store.GetAccountByUsernameDomain(ctx, m.webDomain, nil)
	if err != nil || account == nil || !account.IsServiceActor {
		return nil, fmt.Errorf("instance service actor is not configured")
	}
	return account, nil
}

func (m *RelayManager) validateActorURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("relay actor must be an HTTPS actor URL")
	}
	if m.federator.isLocalDomain(u.Hostname()) {
		return fmt.Errorf("relay actor must be remote")
	}
	return nil
}

func (m *RelayManager) validateAccount(ctx context.Context, account *models.Account) error {
	if account == nil || account.Domain == nil || account.InboxURI == "" {
		return fmt.Errorf("relay actor is not a remote actor with an inbox")
	}
	if err := m.validateActorURL(account.ActorURI); err != nil {
		return fmt.Errorf("invalid canonical relay actor: %w", err)
	}
	u, _ := url.Parse(account.ActorURI)
	if hostInboundBlocked(ctx, m.store, strings.ToLower(u.Hostname())) || account.SuspendedAt != nil || account.ModerationState == "suspended" {
		return fmt.Errorf("relay actor is blocked or suspended")
	}
	return nil
}

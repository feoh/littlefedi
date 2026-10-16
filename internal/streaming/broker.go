package streaming

import (
	"context"
	"fmt"
	"log/slog"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

// BrokerEvent is small enough for PostgreSQL NOTIFY's payload
// limit. Object pointers are used only by LocalBroker and are never serialized.
type BrokerEvent struct {
	Kind         string            `json:"kind"`
	ID           string            `json:"id"`
	AccountID    string            `json:"account_id,omitempty"`
	Visibility   models.Visibility `json:"visibility,omitempty"`
	status       *models.Status
	notification *models.Notification
}

// Broker transports domain event references between producers and each
// process-local EventRouter.
type Broker interface {
	Start(context.Context, func(context.Context, BrokerEvent)) error
	Publish(context.Context, BrokerEvent) error
	Close() error
}

// LocalBroker delivers in process, without goroutines or dependencies.
type LocalBroker struct {
	consume func(context.Context, BrokerEvent)
}

func (b *LocalBroker) Start(_ context.Context, consume func(context.Context, BrokerEvent)) error {
	b.consume = consume
	return nil
}
func (b *LocalBroker) Publish(ctx context.Context, event BrokerEvent) error {
	if b.consume != nil {
		b.consume(ctx, event)
	}
	return nil
}
func (b *LocalBroker) Close() error { return nil }

// BrokerPublisher adapts the producer-facing Publisher API to a Broker.
type BrokerPublisher struct{ broker Broker }

func NewBrokerPublisher(b Broker) *BrokerPublisher { return &BrokerPublisher{broker: b} }

func (p *BrokerPublisher) publish(ctx context.Context, event BrokerEvent) {
	if p == nil || p.broker == nil {
		return
	}
	if err := p.broker.Publish(ctx, event); err != nil {
		slog.Warn("streaming: broker publish failed", "kind", event.Kind, "id", event.ID, "error", err)
	}
}
func (p *BrokerPublisher) StatusCreated(ctx context.Context, s *models.Status) {
	if s != nil {
		p.publish(ctx, BrokerEvent{Kind: "status.created", ID: s.ID, AccountID: s.AccountID, Visibility: s.Visibility, status: s})
	}
}
func (p *BrokerPublisher) StatusUpdated(ctx context.Context, s *models.Status) {
	if s != nil {
		p.publish(ctx, BrokerEvent{Kind: "status.updated", ID: s.ID, AccountID: s.AccountID, Visibility: s.Visibility, status: s})
	}
}
func (p *BrokerPublisher) StatusDeleted(ctx context.Context, s *models.Status) {
	if s != nil {
		p.publish(ctx, BrokerEvent{Kind: "status.deleted", ID: s.ID, AccountID: s.AccountID, Visibility: s.Visibility, status: s})
	}
}
func (p *BrokerPublisher) NotificationCreated(ctx context.Context, n *models.Notification) {
	if n != nil {
		p.publish(ctx, BrokerEvent{Kind: "notification.created", ID: n.ID, AccountID: n.AccountID, notification: n})
	}
}
func (p *BrokerPublisher) CacheInvalidated(ctx context.Context) {
	p.publish(ctx, BrokerEvent{Kind: "cache.invalidate"})
}

func NewBroker(backend, dsn string) (Broker, error) {
	switch backend {
	case "", "local":
		return &LocalBroker{}, nil
	case "postgres":
		return newPostgresBroker(dsn)
	default:
		return nil, fmt.Errorf("streaming: unknown broker backend %q", backend)
	}
}

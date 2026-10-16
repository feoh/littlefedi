//go:build postgres

package streaming

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const brokerChannel = "littlefedi_streaming"

type postgresBroker struct {
	dsn    string
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	wg     sync.WaitGroup
	pub    *pgx.Conn
	listen *pgx.Conn
}

func newPostgresBroker(dsn string) (Broker, error) {
	if !isPostgresURL(dsn) {
		return nil, fmt.Errorf("streaming: postgres broker requires a postgres DSN")
	}
	return &postgresBroker{dsn: dsn}, nil
}
func isPostgresURL(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

func (b *postgresBroker) Start(ctx context.Context, consume func(context.Context, BrokerEvent)) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	pub, err := pgx.Connect(b.ctx, b.dsn)
	if err != nil {
		return fmt.Errorf("streaming broker publisher: %w", err)
	}
	listen, err := pgx.Connect(b.ctx, b.dsn)
	if err != nil {
		pub.Close(context.Background())
		return fmt.Errorf("streaming broker listener: %w", err)
	}
	if _, err = listen.Exec(b.ctx, "LISTEN "+brokerChannel); err != nil {
		pub.Close(context.Background())
		listen.Close(context.Background())
		return err
	}
	b.pub, b.listen = pub, listen
	b.wg.Add(1)
	go b.listenLoop(consume)
	return nil
}

func (b *postgresBroker) listenLoop(consume func(context.Context, BrokerEvent)) {
	defer b.wg.Done()
	for b.ctx.Err() == nil {
		n, err := b.listen.WaitForNotification(b.ctx)
		if err != nil {
			if b.ctx.Err() != nil {
				return
			}
			slog.Warn("streaming: postgres listener disconnected", "error", err)
			if !b.reconnectListener() {
				return
			}
			continue
		}
		var event BrokerEvent
		if json.Unmarshal([]byte(n.Payload), &event) == nil {
			consume(b.ctx, event)
		}
	}
}
func (b *postgresBroker) reconnectListener() bool {
	for delay := time.Second; b.ctx.Err() == nil; delay = min(delay*2, 30*time.Second) {
		conn, err := pgx.Connect(b.ctx, b.dsn)
		if err == nil {
			_, err = conn.Exec(b.ctx, "LISTEN "+brokerChannel)
		}
		if err == nil {
			b.listen = conn
			return true
		}
		if conn != nil {
			conn.Close(context.Background())
		}
		select {
		case <-b.ctx.Done():
			return false
		case <-time.After(delay):
		}
	}
	return false
}
func (b *postgresBroker) Publish(ctx context.Context, event BrokerEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pub == nil {
		return fmt.Errorf("broker not started")
	}
	_, err = b.pub.Exec(ctx, "SELECT pg_notify($1, $2)", brokerChannel, string(payload))
	if err != nil && b.ctx != nil && b.ctx.Err() == nil && ctx.Err() == nil {
		// A publisher connection may be severed independently of the listener.
		// Reconnect once so a transient database restart does not disable streams
		// until the application itself is restarted.
		conn, connectErr := pgx.Connect(b.ctx, b.dsn)
		if connectErr == nil {
			_ = b.pub.Close(context.Background())
			b.pub = conn
			_, err = b.pub.Exec(ctx, "SELECT pg_notify($1, $2)", brokerChannel, string(payload))
		}
	}
	return err
}
func (b *postgresBroker) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if b.listen != nil {
		_ = b.listen.Close(ctx)
	}
	if b.pub != nil {
		return b.pub.Close(ctx)
	}
	return nil
}

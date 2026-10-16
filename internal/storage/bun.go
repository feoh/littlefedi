package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/uptrace/bun/migrate"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	accountmigration "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/migration"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage/migrations"
)

// bunStorage implements Storage using bun ORM (SQLite or PostgreSQL).
type bunStorage struct {
	db              *bun.DB
	sqldb           *sql.DB
	dialect         string // "sqlite" or "pg"
	dsn             string // original connection string for backups
	rels            relationshipCache
	reach           instanceReachCache
	policy          federationPolicyCache
	criticalErrorMu sync.RWMutex
	criticalError   func(error)

	// cursorCreatedAt memoises status id -> created_at for pagination cursors
	// (statusCursorCreatedAt). created_at is immutable for a status, so entries
	// never need invalidation; lives here because decorators can't intercept
	// internal calls (same reason as rels).
	cursorMu        sync.Mutex
	cursorCreatedAt map[string]time.Time
}

func (s *bunStorage) DBStats() sql.DBStats { return s.sqldb.Stats() }

// NewBunStorage creates a new bun-backed Storage.
// The default build supports SQLite. Build with -tags postgres for the
// PostgreSQL-only variant.
func NewBunStorage(dsn string, maxOpenConns, maxIdleConns int, logQueries bool) (Storage, error) {
	sqldb, db, dialect, err := openStorageDB(dsn)
	if err != nil {
		return nil, err
	}

	if capped := SQLiteMaxPoolSize(maxOpenConns); capped != maxOpenConns {
		// Worth saying out loud. Every connection past the cap would have
		// served an interactive read while a background write held the others,
		// so the operator should know the configured value is not in effect.
		// A non-positive request lands here too: database/sql would read it as
		// an unlimited pool, which on this driver is an unlimited number of
		// arenas, so it is raised to the cap rather than lowered to it.
		slog.Warn("storage: SQLite driver overrides the configured connection pool size",
			"requested", maxOpenConns, "effective", capped)
		maxOpenConns = capped
	}
	if maxIdleConns > maxOpenConns {
		maxIdleConns = maxOpenConns
	}
	sqldb.SetMaxOpenConns(maxOpenConns)
	sqldb.SetMaxIdleConns(maxIdleConns)
	store := &bunStorage{db: db, sqldb: sqldb, dialect: dialect, dsn: dsn}
	db.AddQueryHook(databaseErrorHook{store: store})
	if dialect == "sqlite" {
		// SQLite connections are local file handles: recycling them only
		// re-runs every DSN pragma and throws away the per-connection page
		// cache. Keep them for the life of the process.
		sqldb.SetConnMaxLifetime(0)
		db.AddQueryHook(sqliteContentionHook{pool: sqldb})
	} else {
		sqldb.SetConnMaxLifetime(5 * time.Minute)
	}

	if logQueries {
		db.AddQueryHook(bundebug.NewQueryHook(
			bundebug.WithVerbose(true),
		))
	}

	return store, nil
}

const (
	sqliteRetryAttempts = 3
	sqliteSlowQuery     = 2 * time.Second
)

// sqliteContentionHook reports statements that took a long time end to end.
// bun starts the clock in BeforeQuery, which runs before database/sql hands out
// a connection, so the duration covers the wait for the pool as well as the
// work. That distinction matters on small hosts: when the database fits in the
// page cache, a statement logged here at several seconds spent nearly all of
// them queued behind another caller, and the fix is a free connection rather
// than a faster query. The pool counters come along so the two can be told
// apart without guessing.
type sqliteContentionHook struct{ pool *sql.DB }

type databaseErrorHook struct{ store *bunStorage }

func (databaseErrorHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h databaseErrorHook) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if !isCriticalDatabaseError(event.Err) {
		return
	}
	h.store.criticalErrorMu.RLock()
	handler := h.store.criticalError
	h.store.criticalErrorMu.RUnlock()
	if handler != nil {
		handler(event.Err)
	}
}

// SetCriticalErrorHandler installs the out-of-band database alert callback.
func (s *bunStorage) SetCriticalErrorHandler(handler func(error)) {
	s.criticalErrorMu.Lock()
	s.criticalError = handler
	s.criticalErrorMu.Unlock()
}

func isCriticalDatabaseError(err error) bool {
	if err == nil || errors.Is(err, sql.ErrNoRows) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || isSQLiteBusy(err) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var sqliteError interface{ Code() int }
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() & 0xff {
		case 2, 7, 8, 10, 11, 13, 14, 15, 26: // internal, memory, read-only, I/O, corrupt, full, open, protocol, not-a-db
			return true
		}
	}
	var postgresError interface{ SQLState() string }
	if errors.As(err, &postgresError) {
		state := postgresError.SQLState()
		return len(state) >= 2 && (state[:2] == "08" || state[:2] == "53" || state[:2] == "57" || state[:2] == "58" || state[:2] == "XX")
	}
	message := strings.ToLower(err.Error())
	for _, text := range []string{
		"database disk image is malformed", "file is not a database", "disk i/o error",
		"database or disk is full", "attempt to write a readonly database", "unable to open database file",
		"connection refused", "connection reset", "broken pipe", "server closed the connection",
		"terminating connection", "database system is starting up", "database system is in recovery mode",
	} {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

func (sqliteContentionHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h sqliteContentionHook) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	duration := time.Since(event.StartTime)
	if duration < sqliteSlowQuery {
		return
	}
	stats := h.pool.Stats()
	slog.WarnContext(ctx, "sqlite statement slow, including wait for a connection",
		"operation", event.Operation(), "duration", duration,
		"conns_in_use", stats.InUse, "conns_max", stats.MaxOpenConnections,
		"pool_waits", stats.WaitCount, "pool_wait_total", stats.WaitDuration,
		"error", event.Err)
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked") ||
		strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked")
}

func (s *bunStorage) retrySQLiteBusy(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		started := time.Now()
		err := fn()
		if s.dialect != "sqlite" || !isSQLiteBusy(err) || attempt+1 >= sqliteRetryAttempts || time.Since(started) >= time.Second {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *bunStorage) runInTx(ctx context.Context, fn func(context.Context, bun.Tx) error) error {
	return s.retrySQLiteBusy(ctx, func() error { return s.db.RunInTx(ctx, nil, fn) })
}

func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func (s *bunStorage) Close() error {
	return s.sqldb.Close()
}

func (s *bunStorage) Ping(ctx context.Context) error {
	return s.sqldb.PingContext(ctx)
}

func (s *bunStorage) ExportDatabase(ctx context.Context, w io.Writer) error {
	switch s.dialect {
	case "sqlite":
		return s.exportSQLite(ctx, w)
	case "pg":
		return s.exportPostgres(ctx, w)
	default:
		return fmt.Errorf("storage: unsupported dialect %q for database export", s.dialect)
	}
}

// ExportDatabaseToFile publishes a complete backup atomically at path.
func (s *bunStorage) ExportDatabaseToFile(ctx context.Context, path string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, filepath.Base(path))
	if s.dialect == "sqlite" {
		if err := sqliteBackup(ctx, s.sqldb, s.dsn, tmpPath); err != nil {
			return fmt.Errorf("storage: SQLite backup failed: %w", err)
		}
	} else {
		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		err = s.exportPostgres(ctx, f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *bunStorage) ImportDatabase(ctx context.Context, r io.Reader, dialect string) error {
	switch dialect {
	case "sqlite":
		return s.importSQLite(ctx, r)
	case "pg":
		return s.importPostgres(ctx, r)
	default:
		return fmt.Errorf("storage: unsupported dialect %q for database import", dialect)
	}
}

func (s *bunStorage) RunMigrations(ctx context.Context) error {
	migrator := migrate.NewMigrator(s.db, migrations.Migrations, migrate.WithMarkAppliedOnSuccess(true))
	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("storage: init migrations: %w", err)
	}

	group, err := migrator.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("storage: run migrations: %w", err)
	}
	if s.dialect == "sqlite" {
		if err := secureSQLiteDatabase(s.dsn); err != nil {
			return err
		}
	}

	if group.IsZero() {
		// Already up to date
		return nil
	}

	return nil
}

func secureSQLiteDatabase(dsn string) error {
	path := strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], "file:")
	if path == "" || path == ":memory:" {
		return nil
	}
	decoded := path
	if strings.HasPrefix(dsn, "file:") {
		var err error
		decoded, err = url.PathUnescape(path)
		if err != nil {
			return fmt.Errorf("storage: decode sqlite path: %w", err)
		}
	}
	for _, candidate := range []string{decoded, decoded + "-wal", decoded + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("storage: secure sqlite file %s: %w", candidate, err)
		}
	}
	return nil
}

func (s *bunStorage) exportSQLite(ctx context.Context, w io.Writer) error {
	tmpDir, err := os.MkdirTemp("", "littlefedi-backup-*")
	if err != nil {
		return fmt.Errorf("storage: creating temp directory for SQLite backup: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, "database.db")
	if err := sqliteBackup(ctx, s.sqldb, s.dsn, tmpPath); err != nil {
		return fmt.Errorf("storage: SQLite backup failed: %w", err)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("storage: opening temp backup file: %w", err)
	}
	defer f.Close()

	_, err = io.Copy(w, f)
	if err != nil {
		return fmt.Errorf("storage: streaming SQLite backup: %w", err)
	}
	return nil
}

func (s *bunStorage) exportPostgres(ctx context.Context, w io.Writer) error {
	commandDSN, password, err := postgresCommandDSN(s.dsn)
	if err != nil {
		return fmt.Errorf("storage: PostgreSQL backup DSN: %w", err)
	}

	cmd := exec.CommandContext(ctx, "pg_dump", "--dbname="+commandDSN, "--no-password", "--no-owner", "--no-acl", "--clean", "--if-exists")
	cmd.Env = postgresCommandEnv(password)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("storage: pg_dump failed: %w", err)
	}
	return nil
}

func postgresCommandDSN(dsn string) (string, string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Opaque != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("invalid PostgreSQL URL")
	}
	password := ""
	if u.User != nil {
		username := u.User.Username()
		password, _ = u.User.Password()
		u.User = url.User(username)
	}
	query := u.Query()
	if values, ok := query["password"]; ok {
		if len(values) != 1 || (password != "" && values[0] != password) {
			return "", "", fmt.Errorf("ambiguous PostgreSQL password")
		}
		password = values[0]
		query.Del("password")
		u.RawQuery = query.Encode()
	}
	return u.String(), password, nil
}

func postgresCommandEnv(password string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, "PGPASSWORD=") {
			out = append(out, entry)
		}
	}
	if password != "" {
		out = append(out, "PGPASSWORD="+password)
	}
	return out
}

func (s *bunStorage) importSQLite(ctx context.Context, r io.Reader) error {
	dbPath := dsnToSQLitePath(s.dsn)
	if dbPath == "" {
		return fmt.Errorf("storage: cannot determine SQLite file path from DSN")
	}

	if err := s.Close(); err != nil {
		return fmt.Errorf("storage: closing current database connection: %w", err)
	}

	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("storage: creating database file %s: %w", dbPath, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("storage: writing database file: %w", err)
	}

	return nil
}

func (s *bunStorage) importPostgres(ctx context.Context, r io.Reader) error {
	commandDSN, password, err := postgresCommandDSN(s.dsn)
	if err != nil {
		return fmt.Errorf("storage: PostgreSQL restore DSN: %w", err)
	}
	cmd := exec.CommandContext(ctx, "psql", "--dbname="+commandDSN, "--no-password", "--quiet", "-v", "ON_ERROR_STOP=1")
	cmd.Env = postgresCommandEnv(password)
	cmd.Stdin = r
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("storage: psql restore failed: %w", err)
	}
	return nil
}

func dsnToSQLitePath(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if idx := strings.Index(dsn, "?"); idx >= 0 {
		return dsn[:idx]
	}
	return dsn
}

// ID Generation

// newID returns a UUIDv7: time-ordered, so the string sorts chronologically.
// Mastodon clients compare entity IDs to their saved markers/cursors by
// ordering (real Mastodon IDs are snowflakes); with random v4 IDs roughly
// half of all notifications compared "newer" than any marker forever, so
// apps kept showing them as unread. v4 falls back only if v7 fails.
func newID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.New().String()
}

func NewStatusID(createdAt time.Time) string {
	id := uuid.New()
	ms := uint64(createdAt.UnixMilli())
	id[0] = byte(ms >> 40)
	id[1] = byte(ms >> 32)
	id[2] = byte(ms >> 24)
	id[3] = byte(ms >> 16)
	id[4] = byte(ms >> 8)
	id[5] = byte(ms)
	id[6] = id[6]&0x0f | 0x70
	return id.String()
}

var lastNotificationAPIID atomic.Int64

// newNotificationAPIID returns a positive, numeric, time-ordered ID. Mastodon
// clients treat notification IDs as integer snowflakes; UUIDv7 is sortable but
// is rejected by clients whose v2 model decodes most_recent_notification_id as
// an integer (notably IceCubes).
func newNotificationAPIID() int64 {
	for {
		now := time.Now().UnixNano()
		last := lastNotificationAPIID.Load()
		if now <= last {
			now = last + 1
		}
		if lastNotificationAPIID.CompareAndSwap(last, now) {
			return now
		}
	}
}

// Accounts

func (s *bunStorage) CreateAccount(ctx context.Context, account *models.Account) error {
	if account.Domain == nil {
		account.BlogRepliesEnabled = true
	}
	if account.ActorURI != "" {
		deleted, err := s.db.NewSelect().Model((*models.Tombstone)(nil)).Where("uri = ?", account.ActorURI).Exists(ctx)
		if err != nil {
			return err
		}
		if deleted {
			return fmt.Errorf("account actor URI %s is tombstoned", account.ActorURI)
		}
	}
	if account.ID == "" {
		account.ID = newID()
	}
	if account.CreatedAt.IsZero() {
		account.CreatedAt = time.Now()
	}
	if account.UpdatedAt.IsZero() {
		account.UpdatedAt = time.Now()
	}
	// Stamped here so every creation path gets it, whatever CreatedAt says.
	if account.FirstSeenAt.IsZero() {
		account.FirstSeenAt = time.Now().UTC()
	}
	_, err := s.db.NewInsert().Model(account).Exec(ctx)
	return err
}

func (s *bunStorage) GetAccount(ctx context.Context, id string) (*models.Account, error) {
	account := new(models.Account)
	err := s.db.NewSelect().Model(account).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return account, nil
}

func (s *bunStorage) GetAccountByURI(ctx context.Context, uri string) (*models.Account, error) {
	account := new(models.Account)
	err := s.db.NewSelect().Model(account).Where("actor_uri = ?", uri).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return account, nil
}

func (s *bunStorage) GetAccountByUsernameDomain(ctx context.Context, username string, domain *string) (*models.Account, error) {
	account := new(models.Account)
	q := s.db.NewSelect().Model(account)
	if domain == nil {
		q = q.Where("LOWER(username) = LOWER(?)", username).Where("domain IS NULL")
	} else {
		q = q.Where("username = ?", username).Where("domain = ?", *domain)
	}
	err := q.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return account, nil
}

func (s *bunStorage) UpdateAccount(ctx context.Context, account *models.Account) error {
	previousUpdate := account.UpdatedAt
	now := time.Now()
	if account.Domain == nil && account.ID != "" {
		if followersCount, err := s.CountFollowers(ctx, account.ID); err == nil {
			account.FollowersCount = followersCount
		}
		if followingCount, err := s.CountFollowing(ctx, account.ID); err == nil {
			account.FollowingCount = followingCount
		}
		if statusesCount, err := s.CountAccountStatuses(ctx, account.ID); err == nil {
			account.StatusesCount = statusesCount
		}
	}
	updated := *account
	updated.UpdatedAt = now
	update := func(db bun.IDB) error {
		q := db.NewUpdate().Model(&updated).WherePK().Where("updated_at = ?", previousUpdate)
		if account.Domain != nil {
			q = q.Where("domain IS NOT NULL")
		}
		result, err := q.Exec(ctx)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return sql.ErrNoRows
		}
		return nil
	}
	var err error
	if account.Domain != nil {
		// Remote profiles need only the version-checked write, avoiding a
		// read-to-write transaction upgrade while housekeeping holds the writer.
		err = s.retrySQLiteBusy(ctx, func() error { return update(s.db) })
	} else {
		// Keep password changes and credential revocation in one transaction.
		err = s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			var passwordHash string
			if err := tx.NewSelect().Model((*models.Account)(nil)).Column("password_hash").
				Where("id = ?", account.ID).Where("updated_at = ?", previousUpdate).Scan(ctx, &passwordHash); err != nil {
				return err
			}
			if err := update(tx); err != nil {
				return err
			}
			if passwordHash != account.PasswordHash {
				return revokeAccountCredentials(ctx, tx, account.ID)
			}

			return nil
		})
	}
	if err == nil {
		account.UpdatedAt = now
		s.relsAccountWritten(account)
	}
	return err
}

func (s *bunStorage) DeleteAccount(ctx context.Context, id string) error {
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		// Include boost wrappers of the deleted actor's posts: a boost without
		// its object is not a valid status and would leave counters/feed rows.
		affected := `(SELECT id FROM statuses WHERE account_id = ? OR boost_of_id IN (SELECT id FROM statuses WHERE account_id = ?))`
		var targets []struct {
			ID          string  `bun:"id"`
			AccountID   string  `bun:"account_id"`
			InReplyToID *string `bun:"in_reply_to_id"`
			BoostOfID   *string `bun:"boost_of_id"`
		}
		if err := tx.NewSelect().Table("statuses").
			Column("id", "account_id", "in_reply_to_id", "boost_of_id").
			Where("account_id = ? OR boost_of_id IN (SELECT id FROM statuses WHERE account_id = ?)", id, id).
			Scan(ctx, &targets); err != nil {
			return err
		}
		statusIDs := make([]string, 0, len(targets))
		statusCountSet := make(map[string]struct{})
		accountCountSet := map[string]struct{}{id: {}}
		for _, target := range targets {
			statusIDs = append(statusIDs, target.ID)
			accountCountSet[target.AccountID] = struct{}{}
			if target.InReplyToID != nil {
				statusCountSet[*target.InReplyToID] = struct{}{}
			}
			if target.BoostOfID != nil {
				statusCountSet[*target.BoostOfID] = struct{}{}
			}
		}
		var tagIDs []string
		if len(statusIDs) > 0 {
			if err := tx.NewSelect().Model((*models.StatusTag)(nil)).
				Column("tag_id").Where("status_id IN (?)", bun.In(statusIDs)).
				Scan(ctx, &tagIDs); err != nil {
				return err
			}
		}
		var favouriteStatusIDs []string
		if err := tx.NewSelect().Model((*models.Favourite)(nil)).
			Column("status_id").Where("account_id = ?", id).
			Scan(ctx, &favouriteStatusIDs); err != nil {
			return err
		}
		for _, statusID := range favouriteStatusIDs {
			statusCountSet[statusID] = struct{}{}
		}
		var relatedAccountIDs []string
		if err := tx.NewSelect().Model((*models.Follow)(nil)).
			Column("account_id").Where("target_account_id = ?", id).
			Scan(ctx, &relatedAccountIDs); err != nil {
			return err
		}
		var followedAccountIDs []string
		if err := tx.NewSelect().Model((*models.Follow)(nil)).
			Column("target_account_id").Where("account_id = ?", id).
			Scan(ctx, &followedAccountIDs); err != nil {
			return err
		}
		for _, accountID := range append(relatedAccountIDs, followedAccountIDs...) {
			accountCountSet[accountID] = struct{}{}
		}
		queries := []struct {
			query string
			args  []any
		}{
			{`DELETE FROM poll_votes WHERE account_id = ? OR poll_id IN (SELECT id FROM polls WHERE status_id IN ` + affected + `)`, []any{id, id, id}},
			{`DELETE FROM poll_options WHERE poll_id IN (SELECT id FROM polls WHERE status_id IN ` + affected + `)`, []any{id, id}},
			{`DELETE FROM polls WHERE status_id IN ` + affected, []any{id, id}},
			{`DELETE FROM quote_authorizations WHERE quoted_status_id IN ` + affected + ` OR quoting_uri IN (SELECT uri FROM statuses WHERE id IN ` + affected + `)`, []any{id, id, id, id}},
			{`DELETE FROM notifications WHERE account_id = ? OR from_account_id = ? OR status_id IN ` + affected, []any{id, id, id, id}},
			{`DELETE FROM status_tags WHERE status_id IN ` + affected, []any{id, id}},
			{`DELETE FROM mentions WHERE account_id = ? OR status_id IN ` + affected, []any{id, id, id}},
			{`DELETE FROM favourites WHERE account_id = ? OR status_id IN ` + affected, []any{id, id, id}},
			{`DELETE FROM emoji_reactions WHERE account_id = ? OR status_id IN ` + affected, []any{id, id, id}},
			{`DELETE FROM bookmarks WHERE account_id = ? OR status_id IN ` + affected, []any{id, id, id}},
			{`DELETE FROM status_previews WHERE status_id IN ` + affected, []any{id, id}},
			{`DELETE FROM home_feed WHERE account_id = ? OR author_account_id = ? OR status_id IN ` + affected, []any{id, id, id, id}},
			{`DELETE FROM media_attachments WHERE account_id = ? OR status_id IN ` + affected, []any{id, id, id}},
			{`UPDATE statuses SET in_reply_to_id = NULL WHERE in_reply_to_id IN ` + affected, []any{id, id}},
			{`UPDATE statuses SET quote_of_id = NULL WHERE quote_of_id IN ` + affected, []any{id, id}},
			{`DELETE FROM statuses WHERE id IN ` + affected, []any{id, id}},
			{`DELETE FROM filter_keywords WHERE filter_id IN (SELECT id FROM filters WHERE account_id = ?)`, []any{id}},
			{`DELETE FROM filters WHERE account_id = ?`, []any{id}},
			{`DELETE FROM scheduled_statuses WHERE account_id = ?`, []any{id}},
			{`DELETE FROM push_subscriptions WHERE account_id = ?`, []any{id}},
			{`DELETE FROM oauth_tokens WHERE account_id = ?`, []any{id}},
			{`DELETE FROM sessions WHERE account_id = ?`, []any{id}},
			{`DELETE FROM list_accounts WHERE account_id = ? OR list_id IN (SELECT id FROM lists WHERE account_id = ?)`, []any{id, id}},
			{`DELETE FROM lists WHERE account_id = ?`, []any{id}},
			{`DELETE FROM followed_tags WHERE account_id = ?`, []any{id}},
			{`DELETE FROM featured_tags WHERE account_id = ?`, []any{id}},
			{`DELETE FROM blocks WHERE account_id = ? OR target_account_id = ?`, []any{id, id}},
			{`DELETE FROM mutes WHERE account_id = ? OR target_account_id = ?`, []any{id, id}},
			{`DELETE FROM follows WHERE account_id = ? OR target_account_id = ?`, []any{id, id}},
			{`DELETE FROM relays WHERE account_id = ?`, []any{id}},
			{`DELETE FROM markers WHERE account_id = ?`, []any{id}},
			{`DELETE FROM account_domain_blocks WHERE account_id = ?`, []any{id}},
			{`DELETE FROM password_resets WHERE account_id = ?`, []any{id}},
			{`DELETE FROM email_verifications WHERE account_id = ?`, []any{id}},
			{`DELETE FROM email_digest_state WHERE account_id = ?`, []any{id}},
			{`DELETE FROM mfa_recovery_codes WHERE account_id = ?`, []any{id}},
			{`DELETE FROM account_exports WHERE account_id = ?`, []any{id}},
			{`DELETE FROM account_imports WHERE account_id = ?`, []any{id}},
			{`DELETE FROM reports WHERE account_id = ? OR target_account_id = ?`, []any{id, id}},
			{`DELETE FROM invites WHERE created_by_id = ?`, []any{id}},
			{`DELETE FROM accounts WHERE id = ?`, []any{id}},
		}
		for _, q := range queries {
			if _, err := tx.ExecContext(ctx, q.query, q.args...); err != nil {
				return err
			}
		}
		if err := recountTagCounts(ctx, tx, tagIDs); err != nil {
			return err
		}
		if len(statusCountSet) > 0 {
			statusCountIDs := make([]string, 0, len(statusCountSet))
			for statusID := range statusCountSet {
				statusCountIDs = append(statusCountIDs, statusID)
			}
			if _, err := tx.NewUpdate().Model((*models.Status)(nil)).
				Set("replies_count = (SELECT COUNT(*) FROM statuses AS child WHERE child.in_reply_to_id = status.id AND child.visibility <> 'direct')").
				Set("favourites_count = (SELECT COUNT(*) FROM favourites WHERE favourites.status_id = status.id)").
				Set("reblogs_count = (SELECT COUNT(*) FROM statuses AS boost WHERE boost.boost_of_id = status.id)").
				Where("id IN (?)", bun.In(statusCountIDs)).Exec(ctx); err != nil {
				return err
			}
		}
		accountCountIDs := make([]string, 0, len(accountCountSet))
		for accountID := range accountCountSet {
			accountCountIDs = append(accountCountIDs, accountID)
		}
		_, err := tx.NewUpdate().Model((*models.Account)(nil)).
			Set("followers_count = (SELECT COUNT(*) FROM follows WHERE follows.target_account_id = account.id AND follows.state = ?)", models.FollowAccepted).
			Set("following_count = (SELECT COUNT(*) FROM follows WHERE follows.account_id = account.id AND follows.state = ?)", models.FollowAccepted).
			Set("statuses_count = (SELECT COUNT(*) FROM statuses WHERE statuses.account_id = account.id)").
			Set("updated_at = ?", time.Now()).
			Where("id IN (?)", bun.In(accountCountIDs)).
			Where("domain IS NULL").Exec(ctx)
		return err
	})
	if err == nil {
		s.InvalidateRelationshipCaches()
	}
	return err
}

func (s *bunStorage) PruneExpiredData(ctx context.Context, now, remoteCutoff, metadataCutoff time.Time, limit int) (result PruneExpiredDataResult, err error) {
	if limit <= 0 {
		limit = 500
	}
	err = s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		deleted := func(query string, args ...any) (int64, error) {
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		}
		var n int64
		var err error

		if n, err = deleted(`DELETE FROM push_subscriptions WHERE session_id IN (SELECT id FROM sessions WHERE expires_at <= ? ORDER BY expires_at LIMIT ?)`, now, limit); err != nil {
			return err
		}
		result.PushSubscriptions += n
		if result.Sessions, err = deleted(`DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE expires_at <= ? ORDER BY expires_at LIMIT ?)`, now, limit); err != nil {
			return err
		}

		if n, err = deleted(`DELETE FROM push_subscriptions WHERE oauth_token_id IN (SELECT id FROM oauth_tokens WHERE expires_at <= ? OR revoked_at IS NOT NULL ORDER BY created_at LIMIT ?)`, now, limit); err != nil {
			return err
		}
		result.PushSubscriptions += n
		if result.OAuthTokens, err = deleted(`DELETE FROM oauth_tokens WHERE id IN (SELECT id FROM oauth_tokens WHERE expires_at <= ? OR revoked_at IS NOT NULL ORDER BY created_at LIMIT ?)`, now, limit); err != nil {
			return err
		}

		if result.PasswordResets, err = deleted(`DELETE FROM password_resets WHERE id IN (SELECT id FROM password_resets WHERE expires_at <= ? OR used = TRUE ORDER BY created_at LIMIT ?)`, now, limit); err != nil {
			return err
		}
		if result.EmailVerifications, err = deleted(`DELETE FROM email_verifications WHERE id IN (SELECT id FROM email_verifications WHERE expires_at <= ? OR used = TRUE ORDER BY created_at LIMIT ?)`, now, limit); err != nil {
			return err
		}

		if n, err = deleted(`DELETE FROM filter_keywords WHERE filter_id IN (SELECT id FROM filters WHERE expires_at <= ? ORDER BY expires_at LIMIT ?)`, now, limit); err != nil {
			return err
		}
		if result.Filters, err = deleted(`DELETE FROM filters WHERE id IN (SELECT id FROM filters WHERE expires_at <= ? ORDER BY expires_at LIMIT ?)`, now, limit); err != nil {
			return err
		}

		if result.AccountExports, err = deleted(`DELETE FROM account_exports WHERE id IN (SELECT id FROM account_exports WHERE updated_at < ? ORDER BY updated_at LIMIT ?)`, metadataCutoff, limit); err != nil {
			return err
		}
		if result.AccountImports, err = deleted(`DELETE FROM account_imports WHERE id IN (SELECT id FROM account_imports WHERE state NOT IN ('queued', 'running') AND updated_at < ? ORDER BY updated_at LIMIT ?)`, metadataCutoff, limit); err != nil {
			return err
		}
		if result.OrphanAccountImports, err = deleted(`DELETE FROM account_imports WHERE id IN (SELECT ai.id FROM account_imports ai WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id = ai.account_id) LIMIT ?)`, limit); err != nil {
			return err
		}

		if result.Tags, err = deleted(`DELETE FROM tags WHERE id IN (SELECT t.id FROM tags t WHERE NOT EXISTS (SELECT 1 FROM status_tags st WHERE st.tag_id = t.id) AND NOT EXISTS (SELECT 1 FROM followed_tags ft WHERE ft.tag_id = t.id) AND NOT EXISTS (SELECT 1 FROM featured_tags ftt WHERE ftt.tag_id = t.id) LIMIT ?)`, limit); err != nil {
			return err
		}
		if result.Emojis, err = deleted(`DELETE FROM custom_emojis WHERE id IN (SELECT e.id FROM custom_emojis e WHERE e.domain IS NOT NULL AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.domain = e.domain) AND NOT EXISTS (SELECT 1 FROM emoji_reactions r WHERE r.emoji_id = e.id) LIMIT ?)`, limit); err != nil {
			return err
		}
		if result.Instances, err = deleted(`DELETE FROM instances WHERE id IN (SELECT i.id FROM instances i WHERE i.suspended = FALSE AND i.updated_at < ? AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.domain = i.domain) ORDER BY i.updated_at LIMIT ?)`, remoteCutoff, limit); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

// GetOrphanedRemoteAccounts lists cached remote profiles that nothing here
// references any more. seenBefore is matched against first_seen_at (when we
// inserted the row), not created_at (the remote server's self-declared
// registration date, which carries no local meaning).
func (s *bunStorage) GetOrphanedRemoteAccounts(ctx context.Context, seenBefore time.Time, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 500
	}
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("domain IS NOT NULL").
		Where("first_seen_at IS NOT NULL AND first_seen_at < ?", seenBefore).
		Where("NOT EXISTS (SELECT 1 FROM relays WHERE relays.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM statuses WHERE statuses.account_id = account.id)").
		// Each account column gets its own NOT EXISTS: an OR inside the
		// subquery makes SQLite scan the whole table per candidate row.
		Where("NOT EXISTS (SELECT 1 FROM follows WHERE follows.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM follows WHERE follows.target_account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM blocks WHERE blocks.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM blocks WHERE blocks.target_account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM mutes WHERE mutes.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM mutes WHERE mutes.target_account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM favourites WHERE favourites.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM emoji_reactions WHERE emoji_reactions.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM bookmarks WHERE bookmarks.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM mentions WHERE mentions.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM poll_votes WHERE poll_votes.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM list_accounts WHERE list_accounts.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM notifications WHERE notifications.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM notifications WHERE notifications.from_account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM reports WHERE reports.account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM reports WHERE reports.target_account_id = account.id)").
		Where("NOT EXISTS (SELECT 1 FROM media_attachments WHERE media_attachments.account_id = account.id)").
		Order("first_seen_at ASC").Limit(limit).Scan(ctx)
	return accounts, err
}

func (s *bunStorage) ListAccounts(ctx context.Context, offset, limit int) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).Order("created_at DESC").Offset(offset).Limit(limit).Scan(ctx)
	return accounts, err
}

func (s *bunStorage) RepairLocalAccountCounts(ctx context.Context) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
			Set("followers_count = (SELECT COUNT(*) FROM follows WHERE follows.target_account_id = account.id AND follows.state = ?)", models.FollowAccepted).
			Set("following_count = (SELECT COUNT(*) FROM follows WHERE follows.account_id = account.id AND follows.state = ?)", models.FollowAccepted).
			Set("statuses_count = (SELECT COUNT(*) FROM statuses WHERE statuses.account_id = account.id)").
			Set("updated_at = ?", time.Now()).
			Where("domain IS NULL").
			Exec(ctx); err != nil {
			return fmt.Errorf("repair local account counts: %w", err)
		}
		return nil
	})
}

func (s *bunStorage) GetLocalAccount(ctx context.Context) (*models.Account, error) {
	account := new(models.Account)
	err := s.db.NewSelect().Model(account).
		Where("domain IS NULL").
		Where("is_service_actor = ?", false).
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return account, nil
}

// likeEscape escapes the LIKE wildcards (% and _) and the escape character
// itself so user input is matched literally. Pair with `ESCAPE '\'` in the SQL.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (s *bunStorage) SearchAccounts(ctx context.Context, query string, offset, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 20
	}
	// Normalize: drop a leading @ and split a "user@domain" handle so the
	// domain part can be matched against the domain column rather than the
	// username column (where it would never match).
	query = strings.TrimPrefix(strings.TrimSpace(query), "@")
	username, domain := query, ""
	if i := strings.LastIndex(query, "@"); i > 0 {
		username, domain = query[:i], query[i+1:]
	}

	if username == "" && domain == "" {
		return []*models.Account{}, nil
	}

	userLike := "%" + likeEscape(strings.ToLower(username)) + "%"
	userPrefix := likeEscape(strings.ToLower(username)) + "%"

	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts)

	if username != "" {
		q = q.Where(
			"(lower(username) LIKE ? ESCAPE '\\' OR lower(display_name) LIKE ? ESCAPE '\\')",
			userLike, userLike,
		)
	}
	if domain != "" {
		q = q.Where("lower(coalesce(domain, '')) LIKE ? ESCAPE '\\'",
			"%"+likeEscape(strings.ToLower(domain))+"%")
	}

	// Rank: exact username, then username prefix, then everything else. Within
	// a rank, more-followed accounts win. lower()+LIKE keeps this portable and
	// case-insensitive across SQLite and PostgreSQL.
	q = q.OrderExpr(
		"CASE WHEN lower(username) = ? THEN 0 WHEN lower(username) LIKE ? ESCAPE '\\' THEN 1 ELSE 2 END",
		strings.ToLower(username), userPrefix,
	).
		Order("followers_count DESC").
		Order("username ASC").
		Limit(limit).
		Offset(offset)

	err := q.Scan(ctx)
	return accounts, err
}

// SearchAccountPrefixes is the indexable autocomplete counterpart to
// SearchAccounts. Full search keeps its substring/display-name semantics.
func (s *bunStorage) SearchAccountPrefixes(ctx context.Context, query, followingID string, offset, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 20
	}
	query = strings.TrimPrefix(strings.TrimSpace(query), "@")
	username, domain := query, ""
	if i := strings.LastIndex(query, "@"); i > 0 {
		username, domain = query[:i], query[i+1:]
	}
	if username == "" {
		return []*models.Account{}, nil
	}

	userPrefix := likeEscape(strings.ToLower(username)) + "%"
	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts)
	if followingID != "" {
		q = q.Join("JOIN follows ON follows.target_account_id = account.id").
			Where("follows.account_id = ?", followingID).
			Where("follows.state = ?", models.FollowAccepted)
	}
	if s.dialect == "sqlite" {
		if domain == "" {
			q = q.Where("(username LIKE ? ESCAPE '\\' OR display_name LIKE ? ESCAPE '\\')", userPrefix, userPrefix)
		} else {
			q = q.Where("username LIKE ? ESCAPE '\\'", userPrefix)
		}
	} else if domain == "" {
		q = q.Where("(lower(username) LIKE ? ESCAPE '\\' OR lower(display_name) LIKE ? ESCAPE '\\')", userPrefix, userPrefix)
	} else {
		q = q.Where("lower(username) LIKE ? ESCAPE '\\'", userPrefix)
	}
	if domain != "" {
		domainPrefix := likeEscape(strings.ToLower(domain)) + "%"
		if s.dialect == "sqlite" {
			q = q.Where("coalesce(domain, '') LIKE ? ESCAPE '\\'", domainPrefix)
		} else {
			q = q.Where("lower(coalesce(domain, '')) LIKE ? ESCAPE '\\'", domainPrefix)
		}
	}
	err := q.OrderExpr("CASE WHEN lower(username) = ? THEN 0 ELSE 1 END", strings.ToLower(username)).
		Order("followers_count DESC").
		Order("username ASC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	return accounts, err
}

func (s *bunStorage) SearchStatuses(ctx context.Context, query string, accountID *string, viewerID string, offset, limit int) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return []*models.Status{}, nil
	}
	like := "%" + likeEscape(strings.ToLower(query)) + "%"

	var statuses []*models.Status
	q := s.db.NewSelect().Model(&statuses).
		Where("status.context_only = FALSE").
		Where("status.boost_of_id IS NULL").
		Where(
			"(lower(status.content_html) LIKE ? ESCAPE '\\' OR lower(status.content_source) LIKE ? ESCAPE '\\' OR lower(status.summary) LIKE ? ESCAPE '\\')",
			like, like, like,
		)

	// Visibility: public-ish statuses are searchable by everyone; the viewer
	// can additionally find their own non-public posts.
	if viewerID != "" {
		q = q.Where(
			"(status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted') OR status.account_id = ?)",
			viewerID,
		)
	} else {
		q = q.Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted')")
	}

	if accountID != nil && *accountID != "" {
		q = q.Where("status.account_id = ?", *accountID)
	}

	err := q.Order("status.created_at DESC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	return statuses, err
}

func (s *bunStorage) SearchTags(ctx context.Context, query string, offset, limit int) ([]*models.Tag, error) {
	if limit <= 0 {
		limit = 20
	}
	query = strings.TrimPrefix(strings.TrimSpace(query), "#")
	if query == "" {
		return []*models.Tag{}, nil
	}
	lower := strings.ToLower(query)
	like := "%" + likeEscape(lower) + "%"
	prefix := likeEscape(lower) + "%"

	var tags []*models.Tag
	err := s.db.NewSelect().Model(&tags).
		Where("lower(name) LIKE ? ESCAPE '\\'", like).
		OrderExpr(
			"CASE WHEN lower(name) = ? THEN 0 WHEN lower(name) LIKE ? ESCAPE '\\' THEN 1 ELSE 2 END",
			lower, prefix,
		).
		Order("statuses_count DESC").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	return tags, err
}

func (s *bunStorage) SearchTagPrefixes(ctx context.Context, query string, offset, limit int) ([]*models.Tag, error) {
	if limit <= 0 {
		limit = 20
	}
	query = strings.TrimPrefix(strings.TrimSpace(query), "#")
	if query == "" {
		return []*models.Tag{}, nil
	}
	lower := strings.ToLower(query)
	prefix := likeEscape(lower) + "%"

	var tags []*models.Tag
	q := s.db.NewSelect().Model(&tags)
	if s.dialect == "sqlite" {
		q = q.Where("name LIKE ? ESCAPE '\\'", prefix)
	} else {
		q = q.Where("lower(name) LIKE ? ESCAPE '\\'", prefix)
	}
	err := q.OrderExpr("CASE WHEN lower(name) = ? THEN 0 ELSE 1 END", lower).
		Order("statuses_count DESC").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	return tags, err
}

// GetRemoteAccountsStaleSince returns remote accounts whose FetchedAt is older
// than the given time, or NULL, ordered by oldest first, up to the limit.
func (s *bunStorage) GetRemoteAccountsStaleSince(ctx context.Context, since time.Time, limit int) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("domain IS NOT NULL").
		Where("(fetched_at IS NULL OR fetched_at < ?)", since).
		Order("fetched_at ASC NULLS FIRST").
		Limit(limit).
		Scan(ctx)
	return accounts, err
}

// ListRemoteAccounts keyset-paginates every known remote actor ordered by id
// ASC, for bulk operations like a full refresh sweep.
func (s *bunStorage) ListRemoteAccounts(ctx context.Context, afterID string, limit int) ([]*models.Account, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts).
		Column("id", "actor_uri", "domain", "suspended_at").
		Where("domain IS NOT NULL")
	if afterID != "" {
		q = q.Where("id > ?", afterID)
	}
	err := q.Order("id ASC").Limit(limit).Scan(ctx)
	return accounts, err
}

// ClaimDueActorRefreshes leases a bounded set of due actors. The conditional
// update makes this safe with concurrent dispatchers on both SQLite and
// PostgreSQL without requiring backend-specific SKIP LOCKED syntax.
func (s *bunStorage) ClaimDueActorRefreshes(ctx context.Context, now, leaseUntil time.Time, limit int) ([]*models.Account, error) {
	if limit <= 0 || limit > 1000 {
		limit = 10
	}
	var candidates []*models.Account
	if err := s.db.NewSelect().Model(&candidates).
		Column("id", "actor_uri", "domain", "actor_refresh_failures").
		Where("domain IS NOT NULL").
		Where("suspended_at IS NULL").
		Where("actor_next_refresh_at IS NULL OR actor_next_refresh_at <= ?", now).
		Where("actor_refresh_lease_until IS NULL OR actor_refresh_lease_until <= ?", now).
		OrderExpr("actor_next_refresh_at ASC NULLS FIRST").
		Order("id ASC").Limit(limit).Scan(ctx); err != nil {
		return nil, err
	}
	claimed := candidates[:0]
	for _, account := range candidates {
		res, err := s.db.NewUpdate().Model((*models.Account)(nil)).
			Set("actor_refresh_lease_until = ?", leaseUntil).
			Where("id = ?", account.ID).
			Where("domain IS NOT NULL").
			Where("suspended_at IS NULL").
			Where("actor_next_refresh_at IS NULL OR actor_next_refresh_at <= ?", now).
			Where("actor_refresh_lease_until IS NULL OR actor_refresh_lease_until <= ?", now).
			Exec(ctx)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			account.ActorRefreshLeaseUntil = &leaseUntil
			claimed = append(claimed, account)
		}
	}
	return claimed, nil
}

// ClaimActorRefresh leases one explicitly requested actor for stale-while-
// revalidate. It shares the same lease as periodic dispatch, so repeated page
// views and multiple processes cannot enqueue duplicate refreshes.
func (s *bunStorage) ClaimActorRefresh(ctx context.Context, accountID string, now, leaseUntil time.Time) (bool, error) {
	res, err := s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("actor_refresh_lease_until = ?", leaseUntil).
		Where("id = ?", accountID).
		Where("domain IS NOT NULL").
		Where("suspended_at IS NULL").
		Where("actor_refresh_lease_until IS NULL OR actor_refresh_lease_until <= ?", now).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *bunStorage) SetActorRefreshSchedule(ctx context.Context, accountID string, next time.Time, failures int) error {
	_, err := s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("actor_next_refresh_at = ?", next).
		Set("actor_refresh_lease_until = NULL").
		Set("actor_refresh_failures = ?", failures).
		Where("id = ?", accountID).Exec(ctx)
	return err
}

func (s *bunStorage) ReleaseActorRefreshClaim(ctx context.Context, accountID string) error {
	_, err := s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("actor_refresh_lease_until = NULL").Where("id = ?", accountID).Exec(ctx)
	return err
}

// Statuses

func (s *bunStorage) CreateStatus(ctx context.Context, status *models.Status) error {
	return s.CreateStatusWithJobs(ctx, status, nil)
}

func (s *bunStorage) CreateStatusWithJobs(ctx context.Context, status *models.Status, jobs []*models.Job) error {
	prepareStatus(status)
	for _, job := range jobs {
		applyJobDefaults(job)
	}
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(status).Exec(ctx); err != nil {
			return err
		}
		return persistStatusRows(ctx, tx, status, jobs)
	})
	if err == nil {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return err
}

func prepareStatus(status *models.Status) {
	if status.CreatedAt.IsZero() {
		status.CreatedAt = time.Now()
	}
	if status.ID == "" {
		status.ID = NewStatusID(status.CreatedAt)
	}
	if status.UpdatedAt.IsZero() {
		status.UpdatedAt = time.Now()
	}
}

func persistStatusRows(ctx context.Context, tx bun.IDB, status *models.Status, jobs []*models.Job) error {
	if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
		Set("statuses_count = statuses_count + 1").
		Set("updated_at = ?", time.Now()).
		Where("id = ?", status.AccountID).
		Where("domain IS NULL").
		Exec(ctx); err != nil {
		return err
	}
	// Persist the hashtag pivot and mentions for callers (notably inbound
	// federation and thread backfill) that populate status.Tags/Mentions but
	// don't call CreateStatusTags/CreateMentions separately. Tag pivots are
	// idempotent so paths that also call CreateStatusTags don't conflict.
	if pivots := statusTagPivots(status); len(pivots) > 0 {
		if _, err := tx.NewInsert().Model(&pivots).
			On("CONFLICT (status_id, tag_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
		if err := incrementTagCounts(ctx, tx, tagIDsOf(status)); err != nil {
			return err
		}
	}
	if rows := statusMentionRows(status); len(rows) > 0 {
		if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return err
		}
	}
	if err := persistStatusMedia(ctx, tx, status); err != nil {
		return err
	}
	if status.Poll != nil {
		if err := insertPoll(ctx, tx, status.ID, status.Poll); err != nil {
			return err
		}
	}
	if preview := status.Preview; preview != nil {
		preview.StatusID = status.ID
		if preview.FetchedAt.IsZero() {
			preview.FetchedAt = time.Now()
		}
		if _, err := tx.NewInsert().Model(preview).
			On("CONFLICT (status_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
	}
	if qa := status.QuoteAuthorization; qa != nil {
		if qa.ID == "" {
			qa.ID = newID()
		}
		if qa.CreatedAt.IsZero() {
			qa.CreatedAt = time.Now()
		}
		if _, err := tx.NewInsert().Model(qa).Exec(ctx); err != nil {
			return err
		}
	}
	if status.Visibility != models.VisibilityDirect && status.InReplyToID != nil && *status.InReplyToID != "" {
		if _, err := tx.NewUpdate().Model((*models.Status)(nil)).
			Set("replies_count = replies_count + 1").
			Where("id = ?", *status.InReplyToID).Exec(ctx); err != nil {
			return err
		}
	}
	if status.BoostOfID != nil && *status.BoostOfID != "" {
		res, err := tx.NewUpdate().Model((*models.Status)(nil)).
			Set("reblogs_count = reblogs_count + 1").
			Where("id = ?", *status.BoostOfID).Exec(ctx)
		if err != nil {
			return err
		}
		if rows, err := res.RowsAffected(); err != nil {
			return err
		} else if rows != 1 {
			return fmt.Errorf("create boost: target status %q does not exist", *status.BoostOfID)
		}
	}
	viewers, err := homeTimelineViewers(ctx, tx, status)
	if err != nil {
		return err
	}
	if viewers == nil {
		viewers = []string{}
	}
	status.HomeFeedViewers = viewers
	for start := 0; start < len(viewers); start += 100 {
		end := min(start+100, len(viewers))
		entries := make([]models.HomeFeedEntry, 0, end-start)
		for _, viewerID := range viewers[start:end] {
			entries = append(entries, models.HomeFeedEntry{
				AccountID: viewerID, StatusID: status.ID,
				AuthorAccountID: status.AccountID, CreatedAt: status.CreatedAt,
			})
		}
		if _, err := tx.NewInsert().Model(&entries).
			On("CONFLICT (account_id, status_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
	}
	if len(jobs) > 0 {
		if _, err := tx.NewInsert().Model(&jobs).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func persistStatusMedia(ctx context.Context, tx bun.IDB, status *models.Status) error {
	if len(status.MediaAttachments) == 0 {
		return nil
	}
	existing := make([]string, 0, len(status.MediaAttachments))
	for i := range status.MediaAttachments {
		media := &status.MediaAttachments[i]
		if media.ID != "" {
			existing = append(existing, media.ID)
			continue
		}
		media.ID = newID()
		media.StatusID = &status.ID
		media.AccountID = status.AccountID
		if media.CreatedAt.IsZero() {
			media.CreatedAt = time.Now()
		}
		if media.UpdatedAt.IsZero() {
			media.UpdatedAt = media.CreatedAt
		}
		if _, err := tx.NewInsert().Model(media).Exec(ctx); err != nil {
			return err
		}
	}
	if len(existing) == 0 {
		return nil
	}
	res, err := tx.NewUpdate().Model((*models.MediaAttachment)(nil)).
		Set("status_id = ?", status.ID).
		Set("updated_at = ?", time.Now()).
		Where("id IN (?)", bun.In(existing)).
		Where("account_id = ?", status.AccountID).
		Where("status_id IS NULL").Exec(ctx)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != int64(len(existing)) {
		return fmt.Errorf("attach media: associated %d of %d requested rows", n, len(existing))
	}
	for i := range status.MediaAttachments {
		status.MediaAttachments[i].StatusID = &status.ID
	}
	return nil
}

func insertPoll(ctx context.Context, tx bun.IDB, statusID string, poll *models.Poll) error {
	if poll.ID == "" {
		poll.ID = newID()
	}
	poll.StatusID = statusID
	if _, err := tx.NewInsert().Model(poll).Exec(ctx); err != nil {
		return err
	}
	for i := range poll.Options {
		if poll.Options[i].ID == "" {
			poll.Options[i].ID = newID()
		}
		poll.Options[i].PollID = poll.ID
		poll.Options[i].Position = i
	}
	if len(poll.Options) == 0 {
		return nil
	}
	_, err := tx.NewInsert().Model(&poll.Options).Exec(ctx)
	return err
}

// Materialized home feed (fan-out on write)
// Every status that can appear in a local user's home timeline is written to
// home_feed at ingest time, in the same transaction as the status itself.

// fanOutStatusToHomeFeeds inserts the status into every eligible local
// viewer's feed. Idempotent (ON CONFLICT DO NOTHING), so callers may re-run it
// when tags are attached after the initial insert.
func (s *bunStorage) fanOutStatusToHomeFeeds(ctx context.Context, status *models.Status) {
	viewers, err := s.HomeTimelineViewers(ctx, status)
	if err != nil {
		slog.Error("storage: home feed audience", "status", status.ID, "error", err)
		return
	}
	if viewers == nil {
		viewers = []string{}
	}
	// Stash the audience so the timeline cache decorator (bumpHomeViewers)
	// doesn't re-run the same query on the write path.
	status.HomeFeedViewers = viewers
	if len(viewers) == 0 {
		return
	}
	entries := make([]models.HomeFeedEntry, 0, len(viewers))
	for _, viewerID := range viewers {
		entries = append(entries, models.HomeFeedEntry{
			AccountID:       viewerID,
			StatusID:        status.ID,
			AuthorAccountID: status.AccountID,
			CreatedAt:       status.CreatedAt,
		})
	}
	if _, err := s.db.NewInsert().Model(&entries).
		On("CONFLICT (account_id, status_id) DO NOTHING").Exec(ctx); err != nil {
		slog.Error("storage: home feed fan-out", "status", status.ID, "viewers", len(viewers), "error", err)
	}
}

// fanOutStatusIDToHomeFeeds re-runs fan-out for an already stored status,
// used when the tag pivot changes after insert.
func (s *bunStorage) fanOutStatusIDToHomeFeeds(ctx context.Context, statusID string) {
	status, err := s.GetStatus(ctx, statusID)
	if err != nil {
		return
	}
	s.fanOutStatusToHomeFeeds(ctx, status)
}

// backfillHomeFeedFromAuthor seeds a local follower's feed with the target's
// recent statuses after a follow is accepted.
func backfillHomeFeedFromAuthor(ctx context.Context, db bun.IDB, followerID, authorID string) error {
	local, err := db.NewSelect().Model((*models.Account)(nil)).
		Where("id = ?", followerID).Where("domain IS NULL").Exists(ctx)
	if err != nil {
		return err
	}
	if !local {
		return nil
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO home_feed (account_id, status_id, author_account_id, created_at)
		SELECT * FROM (
			SELECT ? AS account_id, s.id AS status_id, s.account_id AS author_account_id, s.created_at AS created_at
			FROM statuses s
			WHERE s.account_id = ?
			  AND s.context_only = FALSE
			  AND s.visibility IN ('public', 'unlisted', 'local', 'local_unlisted', 'private')
			ORDER BY s.created_at DESC
			LIMIT 100
		) AS src WHERE true
		ON CONFLICT (account_id, status_id) DO NOTHING`, followerID, authorID)
	return err
}

// backfillHomeFeedFromTag seeds a local follower's feed with recent statuses
// carrying the newly followed tag.
func backfillHomeFeedFromTag(ctx context.Context, db bun.IDB, followerID, tagID string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO home_feed (account_id, status_id, author_account_id, created_at)
		SELECT * FROM (
			SELECT ? AS account_id, s.id AS status_id, s.account_id AS author_account_id, s.created_at AS created_at
			FROM statuses s
			JOIN status_tags st ON st.status_id = s.id
			WHERE st.tag_id = ?
			  AND s.context_only = FALSE
			  AND s.visibility IN ('public', 'unlisted', 'local', 'local_unlisted')
			ORDER BY s.created_at DESC
			LIMIT 100
		) AS src WHERE true
		ON CONFLICT (account_id, status_id) DO NOTHING`, followerID, tagID)
	return err
}

// removeAuthorFromHomeFeed drops the author's statuses from the follower's
// feed on unfollow/block. Statuses that would also qualify via a followed tag
// are re-added lazily as new tagged posts arrive; the trade keeps unfollow a
// single indexed delete.
func (s *bunStorage) removeAuthorFromHomeFeed(ctx context.Context, followerID, authorID string) {
	if _, err := s.db.NewDelete().Model((*models.HomeFeedEntry)(nil)).
		Where("account_id = ?", followerID).
		Where("author_account_id = ?", authorID).
		Exec(ctx); err != nil {
		slog.Error("storage: home feed unfollow cleanup", "follower", followerID, "author", authorID, "error", err)
	}
}

// removeTagFromHomeFeed drops tag-sourced entries on tag unfollow, keeping
// rows whose author the viewer follows (or is).
func (s *bunStorage) removeTagFromHomeFeed(ctx context.Context, followerID, tagID string) {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM home_feed
		WHERE account_id = ?
		  AND status_id IN (SELECT status_id FROM status_tags WHERE tag_id = ?)
		  AND author_account_id != ?
		  AND author_account_id NOT IN (
			SELECT target_account_id FROM follows WHERE account_id = ? AND state = 'accepted'
		  )`, followerID, tagID, followerID, followerID); err != nil {
		slog.Error("storage: home feed tag unfollow cleanup", "follower", followerID, "tag", tagID, "error", err)
	}
}

// Optimize gives the SQLite query planner fresh statistics. Stale stats make
// it pick pathological plans on timeline-sized tables (observed: the public
// timeline head query abandoning its ordered partial index for a
// low-selectivity boolean index plus a full sort). analysis_limit bounds the
// rows sampled per index, and PRAGMA optimize only re-analyzes tables whose
// stats SQLite judges stale, so repeated calls (every boot, periodic
// housekeeping) are near-free instead of a full ANALYZE on SD-card hardware.
// PostgreSQL autovacuum already covers this, so it is a no-op there.
func (s *bunStorage) Optimize(ctx context.Context) error {
	if s.dialect != "sqlite" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA analysis_limit=400"); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "PRAGMA optimize")
	return err
}

// PruneHomeFeeds caps each local user's materialized feed at keepPerUser rows,
// deleting the oldest overflow. Called by housekeeping.
func (s *bunStorage) PruneHomeFeeds(ctx context.Context, keepPerUser int) (int, error) {
	if keepPerUser <= 0 {
		keepPerUser = 800
	}
	// Pre-pass: only feeds actually over the cap need pruning (usually none).
	// GROUP BY walks the (account_id, created_at) index in order - no unbounded
	// sort, so this stays safe under the 32-bit WASM arena cap.
	var accountIDs []string
	if err := s.db.NewSelect().Model((*models.HomeFeedEntry)(nil)).
		Column("account_id").
		Group("account_id").
		Having("COUNT(*) > ?", keepPerUser).
		Scan(ctx, &accountIDs); err != nil {
		return 0, err
	}
	total := 0
	for _, accountID := range accountIDs {
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM home_feed
			WHERE account_id = ?
			  AND status_id NOT IN (
				SELECT status_id FROM home_feed
				WHERE account_id = ?
				ORDER BY created_at DESC, status_id DESC
				LIMIT ?
			  )`, accountID, accountID, keepPerUser)
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += int(n)
		}
	}
	return total, nil
}

// statusTagPivots builds status_tags rows from a status's resolved tags.
func statusTagPivots(status *models.Status) []models.StatusTag {
	if len(status.Tags) == 0 {
		return nil
	}
	pivots := make([]models.StatusTag, 0, len(status.Tags))
	for i := range status.Tags {
		if status.Tags[i].ID != "" {
			pivots = append(pivots, models.StatusTag{StatusID: status.ID, TagID: status.Tags[i].ID})
		}
	}
	return pivots
}

// tagIDsOf returns the non-empty tag IDs attached to a status.
func tagIDsOf(status *models.Status) []string {
	ids := make([]string, 0, len(status.Tags))
	for i := range status.Tags {
		if status.Tags[i].ID != "" {
			ids = append(ids, status.Tags[i].ID)
		}
	}
	return ids
}

// statusMentionRows builds mention rows from a status's resolved mentions,
// assigning IDs and the status ID.
func statusMentionRows(status *models.Status) []models.Mention {
	if len(status.Mentions) == 0 {
		return nil
	}
	rows := make([]models.Mention, 0, len(status.Mentions))
	for i := range status.Mentions {
		m := status.Mentions[i]
		if m.ID == "" {
			m.ID = newID()
		}
		m.StatusID = status.ID
		rows = append(rows, m)
	}
	return rows
}

// incrementTagCounts bumps statuses_count for the given tags.
func incrementTagCounts(ctx context.Context, db bun.IDB, tagIDs []string) error {
	if len(tagIDs) == 0 {
		return nil
	}
	_, err := db.NewUpdate().Model((*models.Tag)(nil)).
		Set("statuses_count = statuses_count + 1").
		Where("id IN (?)", bun.In(tagIDs)).
		Exec(ctx)
	return err
}

func recountTagCounts(ctx context.Context, db bun.IDB, tagIDs []string) error {
	if len(tagIDs) == 0 {
		return nil
	}
	_, err := db.NewUpdate().Model((*models.Tag)(nil)).
		Set("statuses_count = (SELECT COUNT(*) FROM status_tags WHERE status_tags.tag_id = tag.id)").
		Where("id IN (?)", bun.In(tagIDs)).Exec(ctx)
	return err
}

func (s *bunStorage) GetStatus(ctx context.Context, id string) (*models.Status, error) {
	status := new(models.Status)
	err := s.db.NewSelect().Model(status).Relation("Account").Where("status.id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return status, nil
}

func (s *bunStorage) GetStatusByURI(ctx context.Context, uri string) (*models.Status, error) {
	status := new(models.Status)
	err := s.db.NewSelect().Model(status).Relation("Account").Where("status.uri = ?", uri).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return status, nil
}

func (s *bunStorage) ListStatusQuotes(ctx context.Context, statusID string, limit int) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 80
	}
	var statuses []*models.Status
	err := s.db.NewSelect().Model(&statuses).
		Relation("Account").
		Where("status.quote_of_id = ?", statusID).
		Order("status.created_at DESC").
		Limit(limit).
		Scan(ctx)
	return statuses, err
}

func (s *bunStorage) GetReblogByAccountStatus(ctx context.Context, accountID, statusID string) (*models.Status, error) {
	status := new(models.Status)
	err := s.db.NewSelect().Model(status).
		Relation("Account").
		Where("status.account_id = ?", accountID).
		Where("status.boost_of_id = ?", statusID).
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return status, nil
}

func (s *bunStorage) UpdateStatus(ctx context.Context, status *models.Status) error {
	status.UpdatedAt = time.Now()
	_, err := s.db.NewUpdate().Model(status).WherePK().Exec(ctx)
	return err
}

func (s *bunStorage) SetStatusBlog(ctx context.Context, statusID string, isBlogPost bool, slug string) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("is_blog_post = ?", isBlogPost).
		Set("blog_slug = ?", slug).
		Where("id = ?", statusID).Exec(ctx)
	return err
}

func (s *bunStorage) UpdateStatusContent(ctx context.Context, status *models.Status) error {
	status.UpdatedAt = time.Now()
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("content_html = ?", status.ContentHTML).
		Set("content_source = ?", status.ContentSource).
		Set("content_type = ?", status.ContentType).
		Set("summary = ?", status.Summary).
		Set("sensitive = ?", status.Sensitive).
		Set("language = ?", status.Language).
		Set("edited_at = ?", status.EditedAt).
		Set("updated_at = ?", status.UpdatedAt).
		Where("id = ?", status.ID).Exec(ctx)
	return err
}

func (s *bunStorage) UpdateStatusExpiresAt(ctx context.Context, statusID string, expiresAt *time.Time) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("expires_at = ?", expiresAt).
		Where("id = ?", statusID).Exec(ctx)
	return err
}

func (s *bunStorage) UpdateStatusQuoteState(ctx context.Context, statusID, state string, approvalURI *string) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("quote_state = ?", state).
		Set("quote_approval_uri = ?", approvalURI).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", statusID).Exec(ctx)
	return err
}

func (s *bunStorage) AdjustStatusRepliesCount(ctx context.Context, statusID string, delta int) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("replies_count = CASE WHEN replies_count + ? > 0 THEN replies_count + ? ELSE 0 END", delta, delta).
		Where("id = ?", statusID).Exec(ctx)
	return err
}

func (s *bunStorage) SetStatusContextFetchedAt(ctx context.Context, statusID string, fetchedAt time.Time) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("context_fetched_at = ?", fetchedAt).
		Where("id = ?", statusID).
		Exec(ctx)
	return err
}

func (s *bunStorage) SetStatusContextOnly(ctx context.Context, statusID string, contextOnly bool) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("context_only = ?", contextOnly).
		Where("id = ?", statusID).
		Exec(ctx)
	return err
}

func (s *bunStorage) LinkStatusQuotesByURI(ctx context.Context, quotedURI, quotedID string) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("quote_of_id = ?", quotedID).
		Where("quote_of_uri = ?", quotedURI).
		Where("quote_of_id IS NULL OR quote_of_id <> ?", quotedID).
		Exec(ctx)
	return err
}

func (s *bunStorage) LinkStatusRepliesByURI(ctx context.Context, parentURI, parentID string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*models.Status)(nil)).
			Set("in_reply_to_id = ?", parentID).
			Where("in_reply_to_uri = ?", parentURI).
			Where("in_reply_to_id IS NULL OR in_reply_to_id <> ?", parentID).
			Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Model((*models.Status)(nil)).
			Set("replies_count = (SELECT COUNT(*) FROM statuses AS child WHERE child.in_reply_to_id = ? AND child.visibility <> ?)", parentID, models.VisibilityDirect).
			Where("id = ?", parentID).
			Exec(ctx)
		return err
	})
}

func (s *bunStorage) DeleteStatus(ctx context.Context, id string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error { return deleteStatusRows(ctx, tx, id) })
}

func (s *bunStorage) GetStatusPreview(ctx context.Context, statusID string) (*models.StatusPreview, error) {
	preview := new(models.StatusPreview)
	err := s.db.NewSelect().Model(preview).Where("status_id = ?", statusID).Scan(ctx)
	return preview, err
}

// GetStatusPreviewsForStatuses loads the cards for a whole timeline page in
// one query, so serializing a page costs one round trip rather than one per
// post.
func (s *bunStorage) GetStatusPreviewsForStatuses(ctx context.Context, statusIDs []string) (map[string]*models.StatusPreview, error) {
	out := make(map[string]*models.StatusPreview, len(statusIDs))
	if len(statusIDs) == 0 {
		return out, nil
	}
	var previews []*models.StatusPreview
	if err := s.db.NewSelect().Model(&previews).
		Where("status_id IN (?)", bun.In(statusIDs)).Scan(ctx); err != nil {
		return nil, err
	}
	for _, preview := range previews {
		out[preview.StatusID] = preview
	}
	return out, nil
}

func (s *bunStorage) UpsertStatusPreview(ctx context.Context, preview *models.StatusPreview) error {
	_, err := s.db.NewInsert().Model(preview).
		On("CONFLICT (status_id) DO UPDATE").
		Set("source_url = EXCLUDED.source_url").Set("url = EXCLUDED.url").Set("title = EXCLUDED.title").
		Set("description = EXCLUDED.description").Set("image = EXCLUDED.image").
		Set("image_remote = EXCLUDED.image_remote").Set("site_name = EXCLUDED.site_name").
		Set("fetched_at = EXCLUDED.fetched_at").Set("cached_at = EXCLUDED.cached_at").Exec(ctx)
	return err
}

func (s *bunStorage) SyncStatusPreview(ctx context.Context, statusID string, preview *models.StatusPreview) error {
	if preview == nil {
		_, err := s.db.NewDelete().Model((*models.StatusPreview)(nil)).Where("status_id = ?", statusID).Exec(ctx)
		return err
	}
	preview.StatusID = statusID
	if preview.FetchedAt.IsZero() {
		preview.FetchedAt = time.Now()
	}
	return s.UpsertStatusPreview(ctx, preview)
}

func (s *bunStorage) GetCachedStatusPreviewsOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*models.StatusPreview, error) {
	var previews []*models.StatusPreview
	q := s.db.NewSelect().Model(&previews).Where("cached_at IS NOT NULL").Where("cached_at < ?", cutoff).Order("cached_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Scan(ctx)
	return previews, err
}

// statusCursorCreatedAt resolves a pagination cursor to its created_at with a
// single-column point lookup - the full GetStatus (Account JOIN + content
// columns) is wasted work when only the sort key is needed, and this runs on
// every paginated timeline request.
func (s *bunStorage) statusCursorCreatedAt(ctx context.Context, id string) (time.Time, bool) {
	s.cursorMu.Lock()
	if t, ok := s.cursorCreatedAt[id]; ok {
		s.cursorMu.Unlock()
		return t, true
	}
	s.cursorMu.Unlock()
	ref := new(models.Status)
	if err := s.db.NewSelect().Model(ref).Column("created_at").Where("id = ?", id).Scan(ctx); err != nil {
		return time.Time{}, false
	}
	s.cursorMu.Lock()
	// Clear-on-full instead of LRU; cursors are few and immutable.
	if len(s.cursorCreatedAt) >= 4096 {
		s.cursorCreatedAt = nil
	}
	if s.cursorCreatedAt == nil {
		s.cursorCreatedAt = make(map[string]time.Time, 64)
	}
	s.cursorCreatedAt[id] = ref.CreatedAt
	s.cursorMu.Unlock()
	return ref.CreatedAt, true
}

func (s *bunStorage) applyStatusPagination(ctx context.Context, q *bun.SelectQuery, maxID, minID, sinceID *string) *bun.SelectQuery {
	if maxID != nil && *maxID != "" {
		if createdAt, ok := s.statusCursorCreatedAt(ctx, *maxID); ok {
			q = q.Where("(status.created_at < ? OR (status.created_at = ? AND status.id < ?))", createdAt, createdAt, *maxID)
		}
	}
	if minID != nil && *minID != "" {
		if createdAt, ok := s.statusCursorCreatedAt(ctx, *minID); ok {
			q = q.Where("(status.created_at > ? OR (status.created_at = ? AND status.id > ?))", createdAt, createdAt, *minID)
		}
	}
	if sinceID != nil && *sinceID != "" {
		if createdAt, ok := s.statusCursorCreatedAt(ctx, *sinceID); ok {
			q = q.Where("(status.created_at > ? OR (status.created_at = ? AND status.id > ?))", createdAt, createdAt, *sinceID)
		}
	}
	return q
}

func (s *bunStorage) GetHomeTimeline(ctx context.Context, accountID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}

	homeFeedJoin := "JOIN home_feed AS hf ON hf.status_id = status.id"
	if s.dialect == "sqlite" {
		// With reply filtering SQLite otherwise starts from in_reply_to_id and
		// sorts the whole join instead of walking this account's ordered feed.
		homeFeedJoin = "JOIN home_feed AS hf INDEXED BY idx_home_feed_account_created ON hf.status_id = status.id"
	}

	// Read the materialized feed (fan-out on write): an indexed walk of
	// (account_id, created_at DESC) joined to statuses by primary key. The old
	// "statuses from any of N followed authors OR any followed tag" scan took
	// seconds cold on SD-card storage; this is the same page in milliseconds.
	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Join(homeFeedJoin).
		Where("hf.account_id = ?", accountID).
		Where("status.context_only = FALSE").
		// 'private' included: home_feed rows for a followers-only post exist
		// only for its author and their accepted followers, so the join is
		// already the audience check.
		Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted', 'private')").
		OrderExpr("hf.created_at DESC, hf.status_id DESC").
		Limit(limit)

	if excludeReplies {
		q = q.Where("status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')")
	}
	if excludeReblogs {
		q = q.Where("status.boost_of_id IS NULL")
	}

	// Hide blocked/muted/suspended authors. Domain-silenced accounts stay
	// visible in the home timeline because the viewer explicitly follows them.
	// The feed is not rewritten on block/mute, so read-time moderation remains
	// authoritative.
	q, err := s.applyTimelineModeration(ctx, q, accountID, false)
	if err != nil {
		return nil, err
	}

	// Exclusive lists hide their members from home. Enforced read-time like
	// block/mute (the feed is not rewritten on membership changes); the
	// relationship cache keeps the set warm and the empty common case skips
	// the predicate entirely.
	rels, err := s.viewerRelsFor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if len(rels.exclusiveListMemberIDs) > 0 {
		q = q.Where("hf.author_account_id NOT IN (?)", bun.In(rels.exclusiveListMemberIDs))
	}

	q = s.applyHomeFeedCursors(ctx, q, maxID, minID, sinceID)

	var statuses []*models.Status
	err = q.Scan(ctx, &statuses)
	return statuses, err
}

// applyHomeFeedCursors paginates on the home_feed columns, not
// status.created_at: the ORDER BY walks the (account_id, created_at DESC,
// status_id DESC) index, and a cursor on a different table's column pushes the
// planner off it (the incremental head refresh went from milliseconds to
// seconds). Shared by GetHomeTimeline and GetListTimeline.
func (s *bunStorage) applyHomeFeedCursors(ctx context.Context, q *bun.SelectQuery, maxID, minID, sinceID *string) *bun.SelectQuery {
	cursorWhere := func(id string, newer bool) {
		createdAt, ok := s.statusCursorCreatedAt(ctx, id)
		if !ok {
			return
		}
		if newer {
			q = q.Where("(hf.created_at > ? OR (hf.created_at = ? AND hf.status_id > ?))", createdAt, createdAt, id)
		} else {
			q = q.Where("(hf.created_at < ? OR (hf.created_at = ? AND hf.status_id < ?))", createdAt, createdAt, id)
		}
	}
	if maxID != nil && *maxID != "" {
		cursorWhere(*maxID, false)
	}
	if minID != nil && *minID != "" {
		cursorWhere(*minID, true)
	}
	if sinceID != nil && *sinceID != "" {
		cursorWhere(*sinceID, true)
	}
	return q
}

// GetListTimeline walks the owner's materialized home_feed restricted to the
// list's members. Mastodon requires list members to be followed, so members'
// posts are already fanned out into the owner's feed - the list view is a
// filtered home walk with zero extra write cost and no backfill on membership
// changes. Depth is therefore bounded by home_feed retention (PruneHomeFeeds,
// ~800 rows/user): a list cannot paginate past the owner's home history.
func (s *bunStorage) GetListTimeline(ctx context.Context, list *models.List, maxID, minID, sinceID *string, limit int) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}

	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Join("JOIN home_feed AS hf ON hf.status_id = status.id").
		Where("hf.account_id = ?", list.AccountID).
		Where("hf.author_account_id IN (SELECT account_id FROM list_accounts WHERE list_id = ?)", list.ID).
		Where("status.context_only = FALSE").
		Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted', 'private')").
		OrderExpr("hf.created_at DESC, hf.status_id DESC").
		Limit(limit)

	// replies_policy: self-replies are always shown; a reply whose parent is
	// not stored locally is hidden under every policy (Mastodon filters
	// replies whose in_reply_to account is unknown). notReply matches a
	// top-level post; parentClause is the extra author condition (beyond
	// self-reply) that lets a reply through.
	notReply := "status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')"
	switch list.RepliesPolicy {
	case models.ListRepliesNone:
		q = q.Where("((" + notReply + ") OR EXISTS (SELECT 1 FROM statuses AS parent WHERE parent.id = status.in_reply_to_id AND parent.account_id = status.account_id))")
	case models.ListRepliesFollowed:
		q = q.Where("(("+notReply+") OR EXISTS (SELECT 1 FROM statuses AS parent WHERE parent.id = status.in_reply_to_id AND (parent.account_id = status.account_id OR parent.account_id IN (SELECT target_account_id FROM follows WHERE account_id = ? AND state = ?))))",
			list.AccountID, models.FollowAccepted)
	default: // models.ListRepliesList - also the stored default
		q = q.Where("(("+notReply+") OR EXISTS (SELECT 1 FROM statuses AS parent WHERE parent.id = status.in_reply_to_id AND (parent.account_id = status.account_id OR parent.account_id IN (SELECT account_id FROM list_accounts WHERE list_id = ?))))",
			list.ID)
	}

	// Same read-time moderation as home: domain silence off because the owner
	// explicitly curated these accounts.
	q, err := s.applyTimelineModeration(ctx, q, list.AccountID, false)
	if err != nil {
		return nil, err
	}

	q = s.applyHomeFeedCursors(ctx, q, maxID, minID, sinceID)

	var statuses []*models.Status
	err = q.Scan(ctx, &statuses)
	return statuses, err
}

// StatusAffectsHomeTimeline reports whether a newly stored status can belong to
// at least one local user's home timeline. Remote deliveries for actors nobody
// follows are common on shared inboxes; excluding them avoids pointless global
// home-cache generation bumps. A followed hashtag is also a valid home source.
func (s *bunStorage) StatusAffectsHomeTimeline(ctx context.Context, status *models.Status) (bool, error) {
	if status == nil || status.ContextOnly {
		return false, nil
	}
	switch status.Visibility {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityLocal, models.VisibilityLocalUnlisted:
	default:
		return false, nil
	}
	if isLocalStatus(status) {
		return true, nil
	}
	followed, err := s.db.NewSelect().Model((*models.Follow)(nil)).
		Where("target_account_id = ?", status.AccountID).
		Where("state = ?", models.FollowAccepted).
		Exists(ctx)
	if err != nil || followed {
		return followed, err
	}
	return s.db.NewSelect().Model((*models.StatusTag)(nil)).
		Join("JOIN followed_tags AS ft ON ft.tag_id = status_tag.tag_id").
		Where("status_tag.status_id = ?", status.ID).
		Exists(ctx)
}

// HomeTimelineViewers returns the local account IDs whose home timeline can
// include the status: the author (when local), local followers of the author,
// and local followers of any of the status's hashtags. It powers per-viewer
// home-generation bumps so one user's new post does not mark every other
// user's cached home page stale.
func (s *bunStorage) HomeTimelineViewers(ctx context.Context, status *models.Status) ([]string, error) {
	return homeTimelineViewers(ctx, s.db, status)
}

func homeTimelineViewers(ctx context.Context, db bun.IDB, status *models.Status) ([]string, error) {
	if status == nil || status.ContextOnly {
		return nil, nil
	}
	switch status.Visibility {
	case models.VisibilityPublic, models.VisibilityUnlisted, models.VisibilityLocal, models.VisibilityLocalUnlisted, models.VisibilityPrivate:
	default:
		return nil, nil
	}
	seen := make(map[string]struct{})
	var viewers []string
	add := func(ids ...string) {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				viewers = append(viewers, id)
			}
		}
	}
	if isLocalStatus(status) {
		add(status.AccountID)
	}
	var followerIDs []string
	if err := db.NewSelect().Model((*models.Follow)(nil)).
		Column("follow.account_id").
		Join("JOIN accounts AS fa ON fa.id = follow.account_id").
		Where("follow.target_account_id = ?", status.AccountID).
		Where("follow.state = ?", models.FollowAccepted).
		Where("fa.domain IS NULL").
		Scan(ctx, &followerIDs); err != nil {
		return nil, err
	}
	add(followerIDs...)
	// Followers-only posts reach the author and their followers, never the
	// wider tag-follower audience.
	if status.Visibility == models.VisibilityPrivate {
		return viewers, nil
	}
	var tagFollowerIDs []string
	if err := db.NewSelect().Model((*models.FollowedTag)(nil)).
		Column("followed_tag.account_id").
		Join("JOIN status_tags AS st ON st.tag_id = followed_tag.tag_id").
		Where("st.status_id = ?", status.ID).
		Scan(ctx, &tagFollowerIDs); err != nil {
		return nil, err
	}
	add(tagFollowerIDs...)
	return viewers, nil
}

func (s *bunStorage) GetPublicTimeline(ctx context.Context, viewerID string, local bool, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}
	// The local timeline is the instance's own voice: boosts of remote posts
	// would smuggle other instances back in, so reblogs never appear there
	// regardless of the viewer's hide-boosts preference (Mastodon does the same).
	if local {
		excludeReblogs = true
	}

	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.context_only = FALSE").
		Order("status.created_at DESC").
		Limit(limit)

	if s.dialect == "sqlite" {
		if local {
			// Local accounts are few. With reply filtering SQLite otherwise
			// scans the reply index and sorts every status instead of reading
			// just those accounts through the existing account/time index.
			q = q.ModelTableExpr("statuses AS status INDEXED BY idx_statuses_account_id_created_at")
		} else {
			// The portable SQLite planner otherwise prefers the reply index when
			// hide-replies is enabled, then sorts the entire statuses table.
			q = q.ModelTableExpr("statuses AS status INDEXED BY idx_statuses_public_compat")
		}
	}

	if local {
		q = q.Where("status.visibility IN ('public', 'local')")
		q = q.Where("status.account_id IN (SELECT id FROM accounts WHERE domain IS NULL)")
	} else {
		// Literal, not a bound parameter: SQLite can only use the partial
		// index idx_statuses_public_head when the WHERE clause provably
		// implies the index predicate, and a `visibility = ?` parameter
		// defeats that proof (observed: planner falls back to the useless
		// context_only index plus a temp B-tree sort over every status).
		q = q.Where("status.visibility = 'public'")
	}

	if excludeReplies {
		q = q.Where("status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')")
	}
	if excludeReblogs {
		q = q.Where("status.boost_of_id IS NULL")
	}

	q, err := s.applyTimelineModeration(ctx, q, viewerID, true)
	if err != nil {
		return nil, err
	}

	q = s.applyStatusPagination(ctx, q, maxID, minID, sinceID)

	var statuses []*models.Status
	err = q.Scan(ctx, &statuses)
	return statuses, err
}

func (s *bunStorage) GetAccountStatuses(ctx context.Context, viewerID, accountID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs, onlyMedia, pinnedOnly bool) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}

	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.account_id = ?", accountID).
		Order("status.created_at DESC").
		Limit(limit)

	// Owners and accepted followers see followers-only posts on the profile;
	// everyone else (including unauthenticated/federated reads) gets
	// public-ish posts only. Direct posts never appear on a profile.
	if viewerID != "" && viewerID == accountID {
		q = q.Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted', 'private')")
	} else if viewerID != "" {
		q = q.Where(`(
			status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted')
			OR (
				status.visibility = 'private'
				AND EXISTS (
					SELECT 1 FROM follows
					WHERE follows.account_id = ?
					  AND follows.target_account_id = status.account_id
					  AND follows.state = 'accepted'
				)
			)
		)`, viewerID)
	} else {
		q = q.Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted')")
	}

	if pinnedOnly {
		return s.GetPinnedStatuses(ctx, accountID, limit)
	}

	if excludeReplies {
		q = q.Where("status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')")
	}
	if excludeReblogs {
		q = q.Where("status.boost_of_id IS NULL")
	}
	if onlyMedia {
		q = q.Where("EXISTS (SELECT 1 FROM media_attachments WHERE media_attachments.status_id = status.id)")
	}

	q = s.applyStatusPagination(ctx, q, maxID, minID, sinceID)

	var statuses []*models.Status
	err := q.Scan(ctx, &statuses)
	return statuses, err
}

func (s *bunStorage) GetBlogSlugs(ctx context.Context, accountID string) (map[string]string, error) {
	var rows []struct {
		ID       string `bun:"id"`
		BlogSlug string `bun:"blog_slug"`
	}
	if err := s.db.NewSelect().Model((*models.Status)(nil)).
		Column("id", "blog_slug").
		Where("account_id = ?", accountID).
		Where("blog_slug <> ''").
		Scan(ctx, &rows); err != nil {
		return nil, err
	}
	slugs := make(map[string]string, len(rows))
	for _, r := range rows {
		slugs[r.BlogSlug] = r.ID
	}
	return slugs, nil
}

func (s *bunStorage) SetBlogSlug(ctx context.Context, statusID, slug string) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("blog_slug = ?", slug).
		Where("id = ?", statusID).
		Exec(ctx)
	return err
}

func (s *bunStorage) GetBlogStatuses(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error) {
	var statuses []*models.Status
	q := s.db.NewSelect().Model(&statuses).
		Where("status.account_id = ?", accountID).
		Where("status.is_blog_post = TRUE").
		Where("status.visibility = 'public'").
		Where("status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')").
		Where("status.boost_of_id IS NULL").
		OrderExpr("status.created_at DESC, status.id DESC")
	// The site generator wants every post (limit <= 0); the profile's blog tab
	// pages through them with the same cursor as every other status list.
	if limit > 0 {
		q = q.Limit(limit)
	}
	q = s.applyStatusPagination(ctx, q, maxID, nil, nil)
	err := q.Scan(ctx)
	return statuses, err
}

func (s *bunStorage) GetRSSStatuses(ctx context.Context, accountID string) ([]*models.Status, error) {
	var statuses []*models.Status
	err := s.db.NewSelect().Model(&statuses).
		Where("status.account_id = ?", accountID).
		Where("status.visibility IN ('public', 'unlisted')").
		Where("status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')").
		Where("status.boost_of_id IS NULL").
		Where("status.context_only = FALSE").
		OrderExpr("status.created_at DESC, status.id DESC").
		Limit(20).
		Scan(ctx)
	return statuses, err
}

func (s *bunStorage) GetStatusContext(ctx context.Context, statusID string) (ancestors, descendants []*models.Status, err error) {
	// Get the status first to find the thread root
	status, err := s.GetStatus(ctx, statusID)
	if err != nil {
		return nil, nil, err
	}

	// Ancestors: one recursive CTE resolves the whole reply chain (portable
	// SQLite/PostgreSQL), replacing up to 100 sequential per-parent queries
	// each with an Account JOIN - a serialized round-trip chain that was slow
	// on SD-card storage every time a thread was opened. The depth cap bounds
	// pathological cycles; duplicates keep their shallowest depth to mirror
	// the old seen-map walk.
	ancestors = []*models.Status{}
	if status.InReplyToID != nil {
		type ancRow struct {
			ID    string `bun:"id"`
			Depth int    `bun:"depth"`
		}
		var rows []ancRow
		err = s.db.NewRaw(`
			WITH RECURSIVE anc(id, in_reply_to_id, depth) AS (
				SELECT id, in_reply_to_id, 0 FROM statuses WHERE id = ?
				UNION ALL
				SELECT s.id, s.in_reply_to_id, a.depth + 1
				FROM statuses s JOIN anc a ON s.id = a.in_reply_to_id
				WHERE a.depth < 100
			)
			SELECT id, depth FROM anc`, *status.InReplyToID).Scan(ctx, &rows)
		if err != nil {
			return nil, nil, err
		}

		depthByID := make(map[string]int, len(rows))
		for _, row := range rows {
			if row.ID == status.ID {
				continue // a cycle back to the opened status is not an ancestor
			}
			if d, ok := depthByID[row.ID]; !ok || row.Depth < d {
				depthByID[row.ID] = row.Depth
			}
		}
		if len(depthByID) > 0 {
			ids := make([]string, 0, len(depthByID))
			for id := range depthByID {
				ids = append(ids, id)
			}
			var chain []*models.Status
			if err = s.db.NewSelect().Model(&chain).
				Where("status.id IN (?)", bun.In(ids)).
				Scan(ctx); err != nil {
				return nil, nil, err
			}
			// Root first: deepest ancestor down to the immediate parent.
			sort.SliceStable(chain, func(i, j int) bool {
				return depthByID[chain[i].ID] > depthByID[chain[j].ID]
			})
			ancestors = chain
		}
	}

	// Descendants: breadth-first traversal gives stable parent-before-child
	// ordering while remaining portable across SQLite and PostgreSQL.
	descendants = []*models.Status{}
	frontier := []string{statusID}
	seenDescendants := map[string]bool{statusID: true}
	for len(frontier) > 0 && len(descendants) < 100 {
		var level []*models.Status
		err = s.db.NewSelect().Model(&level).
			Where("status.in_reply_to_id IN (?)", bun.In(frontier)).
			Order("status.created_at ASC", "status.id ASC").
			Limit(100 - len(descendants)).
			Scan(ctx)
		if err != nil {
			return nil, nil, err
		}
		frontier = frontier[:0]
		for _, child := range level {
			if seenDescendants[child.ID] {
				continue
			}
			seenDescendants[child.ID] = true
			descendants = append(descendants, child)
			frontier = append(frontier, child.ID)
		}
	}
	// The Mastodon context response is chronological. Breadth-first traversal
	// discovers the tree portably, but can put a newer sibling before an older
	// grandchild, so normalize the flat API result after discovery.
	sort.SliceStable(descendants, func(i, j int) bool {
		return descendants[i].CreatedAt.Before(descendants[j].CreatedAt)
	})

	return ancestors, descendants, nil
}

func (s *bunStorage) CountStatuses(ctx context.Context) (int, error) {
	return s.db.NewSelect().Model((*models.Status)(nil)).Count(ctx)
}

func (s *bunStorage) CountLocalStatuses(ctx context.Context) (int, error) {
	return s.db.NewSelect().Model((*models.Status)(nil)).
		Where("account_id IN (SELECT id FROM accounts WHERE domain IS NULL)").
		Count(ctx)
}

func (s *bunStorage) CountAccountStatuses(ctx context.Context, accountID string) (int, error) {
	return s.db.NewSelect().Model((*models.Status)(nil)).Where("account_id = ?", accountID).Count(ctx)
}

func (s *bunStorage) GetRemoteStatusesOlderThan(ctx context.Context, days int, limit int) ([]*models.Status, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.created_at < ?", cutoff).
		Where("status.account_id IN (SELECT id FROM accounts WHERE domain IS NOT NULL)").
		Where("NOT EXISTS (SELECT 1 FROM statuses AS parent JOIN accounts AS parent_account ON parent_account.id = parent.account_id WHERE parent.id = status.in_reply_to_id AND parent_account.domain IS NULL)")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var statuses []*models.Status
	err := q.Order("status.created_at ASC").Scan(ctx, &statuses)
	return statuses, err
}

func (s *bunStorage) GetLocalStatusesOlderThanForAccount(ctx context.Context, accountID string, days, minLikes, minBoosts, limit int) ([]*models.Status, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.created_at < ?", cutoff).
		Where("status.account_id = ?", accountID).
		Where("status.boost_of_id IS NULL"). // never delete boosts
		Where("NOT EXISTS (SELECT 1 FROM favourites WHERE favourites.account_id = status.account_id AND favourites.status_id = status.id)").
		Where("NOT EXISTS (SELECT 1 FROM bookmarks WHERE bookmarks.account_id = status.account_id AND bookmarks.status_id = status.id)").
		Where("status.pinned = FALSE").
		// Blog posts are the account's permanent archive: age-based pruning
		// (server default or per-user override) never touches them.
		Where("status.is_blog_post = FALSE")
	if minLikes > 0 {
		q = q.Where("status.favourites_count < ?", minLikes)
	}
	if minBoosts > 0 {
		q = q.Where("status.reblogs_count < ?", minBoosts)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	var statuses []*models.Status
	err := q.Order("status.created_at ASC").Scan(ctx, &statuses)
	return statuses, err
}

// Follows

func (s *bunStorage) CreateFollow(ctx context.Context, follow *models.Follow) error {
	prepareFollow(follow)
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return s.createFollowTx(ctx, tx, follow)
	})
	if err == nil {
		s.rels.invalidateViewers(follow.AccountID)
	}
	return err
}

func prepareFollow(follow *models.Follow) {
	if follow.ID == "" {
		follow.ID = newID()
	}
	if follow.CreatedAt.IsZero() {
		follow.CreatedAt = time.Now()
	}
	if follow.UpdatedAt.IsZero() {
		follow.UpdatedAt = time.Now()
	}
	if follow.State == "" {
		follow.State = models.FollowAccepted
	}
}

func (s *bunStorage) createFollowTx(ctx context.Context, tx bun.Tx, follow *models.Follow) error {
	if _, err := tx.NewInsert().Model(follow).Exec(ctx); err != nil {
		return err
	}
	if follow.State == models.FollowAccepted {
		if err := s.adjustLocalFollowCountsTx(ctx, tx, follow.AccountID, follow.TargetAccountID, 1); err != nil {
			return err
		}
		return backfillHomeFeedFromAuthor(ctx, tx, follow.AccountID, follow.TargetAccountID)
	}
	return nil
}

func (s *bunStorage) GetFollow(ctx context.Context, id string) (*models.Follow, error) {
	follow := new(models.Follow)
	err := s.db.NewSelect().Model(follow).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return follow, nil
}

func (s *bunStorage) GetFollowByPair(ctx context.Context, accountID, targetID string) (*models.Follow, error) {
	follow := new(models.Follow)
	err := s.db.NewSelect().Model(follow).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return follow, nil
}

func (s *bunStorage) UpdateFollow(ctx context.Context, follow *models.Follow) error {
	follow.UpdatedAt = time.Now()
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model(follow).WherePK().Exec(ctx); err != nil {
			return err
		}
		if follow.State == models.FollowAccepted {
			return backfillHomeFeedFromAuthor(ctx, tx, follow.AccountID, follow.TargetAccountID)
		}
		return nil
	})
	if err == nil {
		s.rels.invalidateViewers(follow.AccountID)
	}
	return err
}

func (s *bunStorage) DeleteFollow(ctx context.Context, id string) error {
	var followerID, targetID string
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		follow := new(models.Follow)
		err := tx.NewSelect().Model(follow).Where("id = ?", id).Scan(ctx)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		followerID = follow.AccountID
		targetID = follow.TargetAccountID
		res, err := tx.NewDelete().Model((*models.Follow)(nil)).Where("id = ?", id).Exec(ctx)
		if err != nil {
			return err
		}
		rows, _ := res.RowsAffected()
		if rows == 0 || follow.ID == "" || follow.State != models.FollowAccepted {
			return nil
		}
		return s.adjustLocalFollowCountsTx(ctx, tx, follow.AccountID, follow.TargetAccountID, -1)
	})
	if err == nil && followerID != "" {
		s.rels.invalidateViewers(followerID)
		if targetID != "" {
			s.removeAuthorFromHomeFeed(ctx, followerID, targetID)
			// Mastodon parity: unfollowing removes the account from the
			// follower's lists (the derived list timeline assumes members are
			// followed). Best-effort, like the home-feed cleanup above.
			if _, lerr := s.RemoveAccountFromAllLists(ctx, followerID, targetID); lerr != nil {
				slog.Warn("failed to remove unfollowed account from lists",
					"follower", followerID, "target", targetID, "error", lerr)
			}
		}
	}
	return err
}

func (s *bunStorage) GetFollowByURI(ctx context.Context, uri string) (*models.Follow, error) {
	follow := new(models.Follow)
	err := s.db.NewSelect().Model(follow).Where("uri = ?", uri).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return follow, nil
}

func (s *bunStorage) AcceptFollow(ctx context.Context, id string) error {
	var followerID string
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		follow := new(models.Follow)
		if err := tx.NewSelect().Model(follow).Where("id = ?", id).Scan(ctx); err != nil {
			return err
		}
		followerID = follow.AccountID
		if _, err := tx.NewUpdate().Model((*models.Follow)(nil)).
			Set("state = ?", models.FollowAccepted).
			Set("updated_at = ?", time.Now()).
			Where("id = ?", id).
			Exec(ctx); err != nil {
			return err
		}
		if follow.State != models.FollowAccepted {
			if err := s.adjustLocalFollowCountsTx(ctx, tx, follow.AccountID, follow.TargetAccountID, 1); err != nil {
				return err
			}
		}
		return backfillHomeFeedFromAuthor(ctx, tx, follow.AccountID, follow.TargetAccountID)
	})
	if err == nil && followerID != "" {
		s.rels.invalidateViewers(followerID)
	}
	return err
}

func (s *bunStorage) RejectFollow(ctx context.Context, id string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		follow := new(models.Follow)
		if err := tx.NewSelect().Model(follow).Where("id = ?", id).Scan(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*models.Follow)(nil)).
			Set("state = ?", models.FollowRejected).
			Set("updated_at = ?", time.Now()).
			Where("id = ?", id).
			Exec(ctx); err != nil {
			return err
		}
		if follow.State == models.FollowAccepted {
			return s.adjustLocalFollowCountsTx(ctx, tx, follow.AccountID, follow.TargetAccountID, -1)
		}
		return nil
	})
}

func (s *bunStorage) adjustLocalFollowCountsTx(ctx context.Context, tx bun.Tx, accountID, targetAccountID string, delta int) error {
	if delta == 0 {
		return nil
	}

	followingExpr := "following_count + 1"
	followersExpr := "followers_count + 1"
	if delta < 0 {
		followingExpr = "CASE WHEN following_count > 0 THEN following_count - 1 ELSE 0 END"
		followersExpr = "CASE WHEN followers_count > 0 THEN followers_count - 1 ELSE 0 END"
	}

	if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
		Set("following_count = "+followingExpr).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", accountID).
		Where("domain IS NULL").
		Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
		Set("followers_count = "+followersExpr).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", targetAccountID).
		Where("domain IS NULL").
		Exec(ctx); err != nil {
		return err
	}
	return nil
}

func (s *bunStorage) ListFollowers(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 40
	}
	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts).
		Join("JOIN follows ON follows.account_id = account.id").
		Where("follows.target_account_id = ?", accountID).
		Where("follows.state = ?", models.FollowAccepted).
		Order("follows.created_at DESC").
		Limit(limit)
	if maxID != nil && *maxID != "" {
		q = q.Where("follows.id < ?", *maxID)
	}
	err := q.Scan(ctx)
	return accounts, err
}

func (s *bunStorage) ListFollowing(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 40
	}
	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts).
		Join("JOIN follows ON follows.target_account_id = account.id").
		Where("follows.account_id = ?", accountID).
		Where("follows.state = ?", models.FollowAccepted).
		Order("follows.created_at DESC").
		Limit(limit)
	if maxID != nil && *maxID != "" {
		q = q.Where("follows.id < ?", *maxID)
	}
	err := q.Scan(ctx)
	return accounts, err
}

func (s *bunStorage) ListFollowRequests(ctx context.Context, accountID string, limit int) ([]*models.Follow, error) {
	if limit <= 0 {
		limit = 40
	}
	var follows []*models.Follow
	err := s.db.NewSelect().Model(&follows).
		Where("target_account_id = ?", accountID).
		Where("state = ?", models.FollowPending).
		Order("created_at DESC").
		Limit(limit).
		Scan(ctx)
	return follows, err
}

func (s *bunStorage) CountFollowers(ctx context.Context, accountID string) (int, error) {
	return s.db.NewSelect().Model((*models.Follow)(nil)).
		Where("target_account_id = ?", accountID).
		Where("state = ?", models.FollowAccepted).
		Count(ctx)
}

func (s *bunStorage) CountFollowing(ctx context.Context, accountID string) (int, error) {
	return s.db.NewSelect().Model((*models.Follow)(nil)).
		Where("account_id = ?", accountID).
		Where("state = ?", models.FollowAccepted).
		Count(ctx)
}

func (s *bunStorage) IsFollowing(ctx context.Context, accountID, targetID string) (bool, error) {
	return s.db.NewSelect().Model((*models.Follow)(nil)).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Where("state = ?", models.FollowAccepted).
		Exists(ctx)
}

func (s *bunStorage) HasLocalFollowers(ctx context.Context, accountID string) (bool, error) {
	return s.db.NewSelect().Model((*models.Follow)(nil)).
		Join("JOIN accounts AS fa ON fa.id = follow.account_id").
		Where("follow.target_account_id = ?", accountID).
		Where("follow.state = ?", models.FollowAccepted).
		Where("fa.domain IS NULL").
		Exists(ctx)
}

func (s *bunStorage) AnyTagFollowed(ctx context.Context, names []string) (bool, error) {
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "#")))
		if name != "" {
			normalized = append(normalized, name)
		}
	}
	if len(normalized) == 0 {
		return false, nil
	}
	return s.db.NewSelect().Model((*models.FollowedTag)(nil)).
		Join("JOIN tags ON tags.id = followed_tag.tag_id").
		Where("lower(tags.name) IN (?)", bun.In(normalized)).
		Exists(ctx)
}

func (s *bunStorage) SetFollowNotify(ctx context.Context, accountID, targetID string, notify bool) error {
	_, err := s.db.NewUpdate().Model((*models.Follow)(nil)).
		Set("notify = ?", notify).
		Set("updated_at = ?", time.Now()).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Exec(ctx)
	return err
}

func (s *bunStorage) ListNotifyFollowerIDs(ctx context.Context, targetAccountID string) ([]string, error) {
	var ids []string
	err := s.db.NewSelect().Model((*models.Account)(nil)).
		Column("account.id").
		Join("JOIN follows ON follows.account_id = account.id").
		Where("follows.target_account_id = ?", targetAccountID).
		Where("follows.state = ?", models.FollowAccepted).
		Where("follows.notify = ?", true).
		Where("account.domain IS NULL").
		Scan(ctx, &ids)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *bunStorage) GetFollowerDestinations(ctx context.Context, accountID string) ([]string, error) {
	var inboxes []string
	err := s.db.NewSelect().Model((*models.Account)(nil)).
		ColumnExpr("DISTINCT COALESCE(NULLIF(account.shared_inbox_uri, ''), account.inbox_uri)").
		Join("JOIN follows ON follows.account_id = account.id").
		Where("follows.target_account_id = ?", accountID).
		Where("follows.state = ?", models.FollowAccepted).
		Where("account.domain IS NOT NULL").
		Where("COALESCE(NULLIF(account.shared_inbox_uri, ''), account.inbox_uri) <> ''").
		OrderExpr("COALESCE(NULLIF(account.shared_inbox_uri, ''), account.inbox_uri) ASC").
		Scan(ctx, &inboxes)
	return inboxes, err
}

func (s *bunStorage) ListLocalFollowers(ctx context.Context, accountID string) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Join("JOIN follows ON follows.account_id = account.id").
		Where("follows.target_account_id = ?", accountID).
		Where("follows.state = ?", models.FollowAccepted).
		Where("account.domain IS NULL").
		Scan(ctx)
	return accounts, err
}

// LitePub relays

func (s *bunStorage) CreateRelay(ctx context.Context, relay *models.Relay) error {
	if relay.ID == "" {
		relay.ID = newID()
	}
	now := time.Now()
	if relay.CreatedAt.IsZero() {
		relay.CreatedAt = now
	}
	if relay.UpdatedAt.IsZero() {
		relay.UpdatedAt = now
	}
	_, err := s.db.NewInsert().Model(relay).Exec(ctx)
	return err
}

func (s *bunStorage) GetRelay(ctx context.Context, id string) (*models.Relay, error) {
	relay := new(models.Relay)
	err := s.db.NewSelect().Model(relay).Where("id = ?", id).Scan(ctx)
	return relay, err
}

func (s *bunStorage) GetRelayByAccountID(ctx context.Context, accountID string) (*models.Relay, error) {
	relay := new(models.Relay)
	err := s.db.NewSelect().Model(relay).Where("account_id = ?", accountID).Scan(ctx)
	return relay, err
}

func (s *bunStorage) GetRelayByActorURI(ctx context.Context, actorURI string) (*models.Relay, error) {
	relay := new(models.Relay)
	err := s.db.NewSelect().Model(relay).
		Join("JOIN accounts AS account ON account.id = relay.account_id").
		Where("account.actor_uri = ?", actorURI).
		Scan(ctx)
	return relay, err
}

func (s *bunStorage) ListRelays(ctx context.Context) ([]*models.Relay, error) {
	var relays []*models.Relay
	err := s.db.NewSelect().Model(&relays).Order("created_at ASC").Scan(ctx)
	return relays, err
}

func (s *bunStorage) UpdateRelay(ctx context.Context, relay *models.Relay) error {
	relay.UpdatedAt = time.Now()
	_, err := s.db.NewUpdate().Model(relay).WherePK().Exec(ctx)
	return err
}

func (s *bunStorage) DeleteRelay(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.Relay)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) GetRelayState(ctx context.Context, id string) (models.RelayState, error) {
	relay, err := s.GetRelay(ctx, id)
	if err != nil {
		return models.RelayError, err
	}
	if !relay.Enabled {
		return models.RelayDisabled, nil
	}
	remote, err := s.GetAccount(ctx, relay.AccountID)
	if err != nil || remote.Domain == nil || remote.InboxURI == "" || relay.FollowID == nil || *relay.FollowID == "" {
		return models.RelayError, nil
	}
	outgoing, err := s.GetFollow(ctx, *relay.FollowID)
	if err != nil || outgoing.TargetAccountID != remote.ID {
		return models.RelayError, nil
	}
	service, err := s.GetAccount(ctx, outgoing.AccountID)
	if err != nil || !service.IsServiceActor || service.Domain != nil {
		return models.RelayError, nil
	}
	switch outgoing.State {
	case models.FollowPending:
		return models.RelayPending, nil
	case models.FollowRejected:
		return models.RelayRejected, nil
	case models.FollowAccepted:
	default:
		return models.RelayError, nil
	}
	reverse, err := s.GetFollowByPair(ctx, remote.ID, service.ID)
	if err != nil || reverse.State != models.FollowAccepted {
		return models.RelayAwaitingFollowBack, nil
	}
	return models.RelayActive, nil
}

func (s *bunStorage) GetActiveRelayInboxes(ctx context.Context) ([]string, error) {
	var inboxes []string
	err := s.db.NewSelect().
		TableExpr("relays AS r").
		ColumnExpr("a.inbox_uri").
		Join("JOIN accounts AS a ON a.id = r.account_id").
		Join("JOIN follows AS outgoing ON outgoing.id = r.follow_id AND outgoing.target_account_id = r.account_id").
		Join("JOIN accounts AS service ON service.id = outgoing.account_id AND service.is_service_actor = ?", true).
		Join("JOIN follows AS reverse ON reverse.account_id = r.account_id AND reverse.target_account_id = service.id").
		Where("r.enabled = ?", true).
		Where("outgoing.state = ?", models.FollowAccepted).
		Where("reverse.state = ?", models.FollowAccepted).
		Where("a.inbox_uri <> ''").
		OrderExpr("a.inbox_uri ASC").
		Scan(ctx, &inboxes)
	return inboxes, err
}

// AdjustStatusReblogsCount bumps reblogs_count without rewriting the full row
// (inbound Announce/Undo arrive with routine federation traffic; a full-row
// UpdateStatus also triggered a read-before-write and broad cache churn).
func (s *bunStorage) AdjustStatusReblogsCount(ctx context.Context, statusID string, delta int) error {
	// CASE instead of GREATEST: portable to SQLite.
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("reblogs_count = CASE WHEN reblogs_count + ? > 0 THEN reblogs_count + ? ELSE 0 END", delta, delta).
		Where("id = ?", statusID).
		Exec(ctx)
	return err
}

// Favourites

func (s *bunStorage) CreateFavourite(ctx context.Context, fave *models.Favourite) error {
	_, err := s.CreateFavouriteWithNotification(ctx, fave, nil, nil)
	return err
}

func (s *bunStorage) DeleteFavourite(ctx context.Context, accountID, statusID string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewDelete().Model((*models.Favourite)(nil)).
			Where("account_id = ?", accountID).
			Where("status_id = ?", statusID).Exec(ctx)
		if err != nil {
			return err
		}
		deleted, err := res.RowsAffected()
		if err != nil || deleted == 0 {
			return err
		}
		_, err = tx.NewUpdate().Model((*models.Status)(nil)).
			Set("favourites_count = CASE WHEN favourites_count > 0 THEN favourites_count - 1 ELSE 0 END").
			Where("id = ?", statusID).Exec(ctx)
		return err
	})
}

func (s *bunStorage) GetFavourite(ctx context.Context, accountID, statusID string) (*models.Favourite, error) {
	fave := new(models.Favourite)
	err := s.db.NewSelect().Model(fave).
		Where("account_id = ?", accountID).
		Where("status_id = ?", statusID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return fave, nil
}

func (s *bunStorage) GetFavouriteByURI(ctx context.Context, uri string) (*models.Favourite, error) {
	fave := new(models.Favourite)
	if err := s.db.NewSelect().Model(fave).Where("uri = ?", uri).Scan(ctx); err != nil {
		return nil, err
	}
	return fave, nil
}

func (s *bunStorage) ListFavourites(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}
	var statuses []*models.Status
	q := s.db.NewSelect().Model(&statuses).
		Join("JOIN favourites ON favourites.status_id = status.id").
		Where("favourites.account_id = ?", accountID).
		Order("favourites.created_at DESC").
		Order("favourites.id DESC").
		Limit(limit)
	if maxID != nil && *maxID != "" {
		cursor := new(models.Favourite)
		err := s.db.NewSelect().Model(cursor).
			Where("account_id = ?", accountID).
			Where("status_id = ?", *maxID).
			Limit(1).Scan(ctx)
		if err != nil {
			return []*models.Status{}, nil
		}
		q = q.Where("(favourites.created_at < ? OR (favourites.created_at = ? AND favourites.id < ?))",
			cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	err := q.Scan(ctx)
	return statuses, err
}

// ListFavouriteAccounts returns the accounts whose locally-known Like
// activities target statusID, newest first.
func (s *bunStorage) ListFavouriteAccounts(ctx context.Context, statusID string, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 80
	}
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Join("JOIN favourites ON favourites.account_id = account.id").
		Where("favourites.status_id = ?", statusID).
		Order("favourites.created_at DESC").
		Limit(limit).
		Scan(ctx)
	return accounts, err
}

// ListReblogAccounts returns the accounts whose locally-known Announce
// wrappers target statusID, newest first.
func (s *bunStorage) ListReblogAccounts(ctx context.Context, statusID string, limit int) ([]*models.Account, error) {
	if limit == 0 {
		limit = 80
	}
	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts).
		Join("JOIN statuses AS boost ON boost.account_id = account.id").
		Where("boost.boost_of_id = ?", statusID).
		Order("boost.created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Scan(ctx)
	return accounts, err
}

// Emoji reactions

func (s *bunStorage) CreateEmojiReaction(ctx context.Context, reaction *models.EmojiReaction) (bool, error) {
	if reaction.ID == "" {
		reaction.ID = newID()
	}
	if reaction.CreatedAt.IsZero() {
		reaction.CreatedAt = time.Now()
	}
	res, err := s.db.NewInsert().Model(reaction).On("CONFLICT DO NOTHING").Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *bunStorage) GetEmojiReaction(ctx context.Context, accountID, statusID, key string) (*models.EmojiReaction, error) {
	reaction := new(models.EmojiReaction)
	if err := s.db.NewSelect().Model(reaction).
		Where("account_id = ?", accountID).Where("status_id = ?", statusID).Where("reaction_key = ?", key).Scan(ctx); err != nil {
		return nil, err
	}
	return reaction, nil
}

func (s *bunStorage) GetEmojiReactionByURI(ctx context.Context, uri string) (*models.EmojiReaction, error) {
	reaction := new(models.EmojiReaction)
	if err := s.db.NewSelect().Model(reaction).Where("uri = ?", uri).Scan(ctx); err != nil {
		return nil, err
	}
	return reaction, nil
}

func (s *bunStorage) DeleteEmojiReaction(ctx context.Context, accountID, statusID, key string) (*models.EmojiReaction, error) {
	reaction, err := s.GetEmojiReaction(ctx, accountID, statusID, key)
	if err != nil {
		return nil, err
	}
	_, err = s.db.NewDelete().Model(reaction).WherePK().Exec(ctx)
	return reaction, err
}

func (s *bunStorage) DeleteEmojiReactionByURI(ctx context.Context, accountID, uri string) (*models.EmojiReaction, error) {
	reaction, err := s.GetEmojiReactionByURI(ctx, uri)
	if err != nil {
		return nil, err
	}
	if reaction.AccountID != accountID {
		return nil, sql.ErrNoRows
	}
	_, err = s.db.NewDelete().Model(reaction).WherePK().Exec(ctx)
	return reaction, err
}

func (s *bunStorage) GetEmojiReactionSummaries(ctx context.Context, statusIDs []string, viewerID string, accountLimit int) (map[string][]models.EmojiReactionSummary, error) {
	result := make(map[string][]models.EmojiReactionSummary, len(statusIDs))
	if len(statusIDs) == 0 {
		return result, nil
	}
	type groupRow struct {
		StatusID, Key, Content                                string
		Count                                                 int
		Me                                                    bool
		EmojiID, Shortcode, Domain, URL, StaticURL, RemoteURL *string
	}
	var groups []groupRow
	err := s.db.NewRaw(`SELECT r.status_id, r.reaction_key AS key, MIN(r.content) AS content,
COUNT(*) AS count, MAX(CASE WHEN r.account_id = ? THEN 1 ELSE 0 END) AS me,
e.id AS emoji_id, e.shortcode, e.domain, e.url, e.static_url, e.remote_url
FROM emoji_reactions r LEFT JOIN custom_emojis e ON e.id = r.emoji_id
WHERE r.status_id IN (?)
GROUP BY r.status_id, r.reaction_key, e.id, e.shortcode, e.domain, e.url, e.static_url, e.remote_url
ORDER BY MIN(r.created_at)`, viewerID, bun.In(statusIDs)).Scan(ctx, &groups)
	if err != nil {
		return nil, err
	}
	positions := make(map[string]map[string]int, len(statusIDs))
	for _, row := range groups {
		summary := models.EmojiReactionSummary{Name: row.Content, Key: row.Key, Count: row.Count, Me: row.Me, AccountIDs: []string{}}
		if row.EmojiID != nil {
			emoji := &models.CustomEmoji{ID: *row.EmojiID, Shortcode: stringValue(row.Shortcode), URL: stringValue(row.URL), StaticURL: stringValue(row.StaticURL), RemoteURL: stringValue(row.RemoteURL)}
			if row.Domain != nil {
				emoji.Domain = row.Domain
				summary.Name = ":" + emoji.Shortcode + "@" + *row.Domain + ":"
			}
			summary.Emoji = emoji
		}
		if positions[row.StatusID] == nil {
			positions[row.StatusID] = make(map[string]int)
		}
		positions[row.StatusID][row.Key] = len(result[row.StatusID])
		result[row.StatusID] = append(result[row.StatusID], summary)
	}
	if accountLimit <= 0 {
		accountLimit = 80
	}
	var actors []struct{ StatusID, Key, AccountID string }
	err = s.db.NewRaw(`SELECT status_id, reaction_key AS key, account_id FROM (
SELECT status_id, reaction_key, account_id,
ROW_NUMBER() OVER (PARTITION BY status_id, reaction_key ORDER BY created_at DESC) AS n
FROM emoji_reactions WHERE status_id IN (?)) ranked WHERE n <= ?`, bun.In(statusIDs), accountLimit).Scan(ctx, &actors)
	if err != nil {
		return nil, err
	}
	for _, actor := range actors {
		if i, ok := positions[actor.StatusID][actor.Key]; ok {
			summaries := result[actor.StatusID]
			summaries[i].AccountIDs = append(summaries[i].AccountIDs, actor.AccountID)
			result[actor.StatusID] = summaries
		}
	}
	return result, nil
}

func (s *bunStorage) ListEmojiReactionAccounts(ctx context.Context, statusID, key string, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 80
	}
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Join("JOIN emoji_reactions AS reaction ON reaction.account_id = account.id").
		Where("reaction.status_id = ?", statusID).Where("reaction.reaction_key = ?", key).
		Order("reaction.created_at DESC").Limit(limit).Scan(ctx)
	return accounts, err
}

func (s *bunStorage) CountEmojiReactions(ctx context.Context, statusIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(statusIDs))
	if len(statusIDs) == 0 {
		return counts, nil
	}
	var rows []struct {
		StatusID string `bun:"status_id"`
		Count    int    `bun:"count"`
	}
	if err := s.db.NewSelect().
		Table("emoji_reactions").
		Column("status_id").
		ColumnExpr("COUNT(*) AS count").
		Where("status_id IN (?)", bun.In(statusIDs)).
		Group("status_id").
		Scan(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		counts[row.StatusID] = row.Count
	}
	return counts, nil
}

func (s *bunStorage) ListEmojiReactions(ctx context.Context, statusID string, limit int) ([]*models.EmojiReaction, error) {
	if limit <= 0 {
		limit = 80
	}
	var reactions []*models.EmojiReaction
	err := s.db.NewSelect().Model(&reactions).Where("status_id = ?", statusID).Order("created_at ASC").Limit(limit).Scan(ctx)
	return reactions, err
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// Media

func (s *bunStorage) CreateMediaAttachment(ctx context.Context, media *models.MediaAttachment) error {
	if media.ID == "" {
		media.ID = newID()
	}
	if media.CreatedAt.IsZero() {
		media.CreatedAt = time.Now()
	}
	if media.UpdatedAt.IsZero() {
		media.UpdatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(media).Exec(ctx)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		// media_attachments' only unique constraint is its primary key, so a
		// collision here means the freshly generated id wasn't actually
		// unique (observed under heavy concurrent write load - cause not
		// fully pinned down). Regenerate once and retry rather than losing
		// the attachment.
		media.ID = newID()
		_, err = s.db.NewInsert().Model(media).Exec(ctx)
	}
	return err
}

func (s *bunStorage) GetMediaAttachment(ctx context.Context, id string) (*models.MediaAttachment, error) {
	media := new(models.MediaAttachment)
	err := s.db.NewSelect().Model(media).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return media, nil
}

func (s *bunStorage) GetMediaForStatus(ctx context.Context, statusID string) ([]*models.MediaAttachment, error) {
	var media []*models.MediaAttachment
	err := s.db.NewSelect().Model(&media).
		Where("status_id = ?", statusID).
		Order("created_at ASC").
		Scan(ctx)
	return media, err
}

func (s *bunStorage) GetMediaForAccount(ctx context.Context, accountID string) ([]*models.MediaAttachment, error) {
	var media []*models.MediaAttachment
	err := s.db.NewSelect().Model(&media).Where("account_id = ?", accountID).Order("created_at ASC").Scan(ctx)
	return media, err
}

func (s *bunStorage) UpdateMediaAttachment(ctx context.Context, media *models.MediaAttachment) error {
	media.UpdatedAt = time.Now()
	_, err := s.db.NewUpdate().Model(media).WherePK().Exec(ctx)
	return err
}

func (s *bunStorage) DeleteMediaAttachment(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.MediaAttachment)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) GetAllMedia(ctx context.Context) ([]*models.MediaAttachment, error) {
	var media []*models.MediaAttachment
	err := s.db.NewSelect().Model(&media).Scan(ctx)
	return media, err
}

func (s *bunStorage) ListOwnedMediaAfter(ctx context.Context, cursor, destinationBackend string, limit int) ([]*models.MediaAttachment, error) {
	var out []*models.MediaAttachment
	q := s.db.NewSelect().Model(&out).Where("remote_url = ''").Where("storage_backend <> ?", destinationBackend).Where("id > ?", cursor).Order("id ASC").Limit(limit)
	return out, q.Scan(ctx)
}

func (s *bunStorage) CreateMediaStorageMigration(ctx context.Context, m *models.MediaStorageMigration) error {
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	return err
}
func (s *bunStorage) GetMediaStorageMigration(ctx context.Context, id string) (*models.MediaStorageMigration, error) {
	m := new(models.MediaStorageMigration)
	err := s.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx)
	return m, err
}
func (s *bunStorage) GetActiveMediaStorageMigration(ctx context.Context) (*models.MediaStorageMigration, error) {
	m := new(models.MediaStorageMigration)
	err := s.db.NewSelect().Model(m).Where("state IN (?)", bun.In([]string{"queued", "running", "paused"})).Order("created_at DESC").Limit(1).Scan(ctx)
	return m, err
}
func (s *bunStorage) UpdateMediaStorageMigration(ctx context.Context, m *models.MediaStorageMigration) error {
	m.UpdatedAt = time.Now().UTC()
	_, err := s.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return err
}
func (s *bunStorage) InvalidateRemoteMediaCache(ctx context.Context) (int64, error) {
	res, err := s.db.NewUpdate().Model((*models.MediaAttachment)(nil)).Set("url = remote_url").Set("preview_url = CASE WHEN preview_storage_key != '' OR preview_url LIKE '/media/cache/%' THEN CASE WHEN type = 'video' THEN '' ELSE remote_url END ELSE preview_url END").Set("cached_at = NULL").Set("storage_key = ''").Set("preview_storage_key = ''").Where("remote_url <> '' AND cached_at IS NOT NULL").Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	for _, q := range []string{
		"UPDATE accounts SET avatar = avatar_remote WHERE avatar_remote <> '' AND avatar <> avatar_remote",
		"UPDATE accounts SET header = header_remote WHERE header_remote <> '' AND header <> header_remote",
		"UPDATE custom_emojis SET url = remote_url, static_url = remote_url, cached_at = NULL WHERE remote_url <> '' AND cached_at IS NOT NULL",
		"UPDATE status_previews SET image = image_remote, cached_at = NULL WHERE image_remote <> '' AND cached_at IS NOT NULL",
	} {
		if _, err = s.db.ExecContext(ctx, q); err != nil {
			return n, err
		}
	}
	return n, nil
}
func (s *bunStorage) UpdateLocalMediaReferences(ctx context.Context, oldURL, newURL string) error {
	if oldURL == "" || oldURL == newURL {
		return nil
	}
	for _, q := range []string{"UPDATE accounts SET avatar = ? WHERE avatar = ? AND domain IS NULL", "UPDATE accounts SET header = ? WHERE header = ? AND domain IS NULL", "UPDATE custom_emojis SET url = ? WHERE url = ? AND domain IS NULL", "UPDATE custom_emojis SET static_url = ? WHERE static_url = ? AND domain IS NULL"} {
		if _, err := s.db.ExecContext(ctx, q, newURL, oldURL); err != nil {
			return err
		}
	}
	return nil
}

func (s *bunStorage) GetOrphanedMediaAttachments(ctx context.Context) ([]*models.MediaAttachment, error) {
	var media []*models.MediaAttachment
	// Find media attachments whose status_id references a status that no longer exists
	// Note: bun aliases the table as the singular form of the table name, so
	// "media_attachments" becomes "media_attachment" in generated queries.
	err := s.db.NewSelect().Model(&media).
		Where("status_id IS NOT NULL").
		Where("NOT EXISTS (SELECT 1 FROM statuses WHERE statuses.id = media_attachment.status_id)").
		Scan(ctx)
	return media, err
}

func (s *bunStorage) GetMediaAttachmentsByRemoteURL(ctx context.Context, remoteURL string) ([]*models.MediaAttachment, error) {
	var media []*models.MediaAttachment
	if remoteURL == "" {
		return media, nil
	}
	err := s.db.NewSelect().Model(&media).Where("remote_url = ?", remoteURL).Scan(ctx)
	return media, err
}

func (s *bunStorage) GetUncachedRemoteMedia(ctx context.Context, limit int) ([]*models.MediaAttachment, error) {
	if limit <= 0 {
		// Backstop: an unbounded sort over media_attachments can blow the
		// 32MB WASM SQLite arena on 32-bit hosts.
		limit = 500
	}
	var media []*models.MediaAttachment
	err := s.db.NewSelect().Model(&media).
		Where("remote_url != ''").
		Where("cached_at IS NULL").
		// Not yet cached: the served URL still points at the remote origin.
		Where("url = remote_url").
		Order("created_at DESC").
		Limit(limit).
		Scan(ctx)
	return media, err
}

func (s *bunStorage) GetCachedMediaOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*models.MediaAttachment, error) {
	var media []*models.MediaAttachment
	q := s.db.NewSelect().Model(&media).
		Where("cached_at IS NOT NULL").
		Where("cached_at < ?", cutoff).
		Order("cached_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Scan(ctx)
	return media, err
}

// CachedMediaBytes totals the recorded size of cached remote media. Owned
// uploads are excluded by the cached_at filter, so the result is the size of
// what a size-bounded cache is allowed to evict, not of the media directory.
// Rows that never recorded a size count as zero.
func (s *bunStorage) CachedMediaBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.NewSelect().Model((*models.MediaAttachment)(nil)).
		ColumnExpr("COALESCE(SUM(file_size), 0)").
		Where("cached_at IS NOT NULL").
		Scan(ctx, &total)
	return total, err
}

func (s *bunStorage) CountMediaAttachmentsByURL(ctx context.Context, url string) (int, error) {
	if url == "" {
		return 0, nil
	}
	return s.db.NewSelect().Model((*models.MediaAttachment)(nil)).Where("url = ?", url).Count(ctx)
}

func (s *bunStorage) GetAccountsByImageURL(ctx context.Context, url string) ([]*models.Account, error) {
	var accounts []*models.Account
	if url == "" {
		return accounts, nil
	}
	err := s.db.NewSelect().Model(&accounts).
		Where("avatar = ? OR header = ?", url, url).Scan(ctx)
	return accounts, err
}

func (s *bunStorage) GetUncachedRemoteAccountImageURLs(ctx context.Context, limit int) ([]string, error) {
	// Remote avatars/headers that have not been cached still hold an absolute
	// http(s) URL; cached ones hold a relative /media/cache/ path.
	seen := make(map[string]struct{})
	var out []string
	collect := func(column string) error {
		var urls []string
		q := s.db.NewSelect().Model((*models.Account)(nil)).
			ColumnExpr("? AS u", bun.Ident(column)).
			Where("? LIKE 'http%'", bun.Ident(column))
		if limit > 0 {
			// Bound in SQL: materializing every remote image URL before
			// truncating in Go can blow the 32MB WASM SQLite arena on
			// 32-bit hosts.
			q = q.Limit(limit)
		}
		err := q.Scan(ctx, &urls)
		if err != nil {
			return err
		}
		for _, u := range urls {
			if u == "" {
				continue
			}
			if _, ok := seen[u]; ok {
				continue
			}
			seen[u] = struct{}{}
			out = append(out, u)
		}
		return nil
	}
	if err := collect("avatar"); err != nil {
		return out, err
	}
	if err := collect("header"); err != nil {
		return out, err
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *bunStorage) ReconcileMissingCacheURL(ctx context.Context, cacheURL string) (string, error) {
	if cacheURL == "" {
		return "", nil
	}

	// Status attachment referencing this cache URL (as the file or its thumbnail).
	var m models.MediaAttachment
	err := s.db.NewSelect().Model(&m).
		Where("(url = ? OR preview_url = ?) AND remote_url != ''", cacheURL, cacheURL).
		Limit(1).Scan(ctx)
	if err == nil && m.RemoteURL != "" {
		_, _ = s.db.NewUpdate().Model((*models.MediaAttachment)(nil)).
			Set("url = CASE WHEN url = ? THEN remote_url ELSE url END", cacheURL).
			Set("preview_url = CASE WHEN preview_storage_key != '' OR preview_url = ? OR preview_url LIKE '/media/cache/%' THEN CASE WHEN type = 'video' THEN '' ELSE remote_url END ELSE preview_url END", cacheURL).
			Set("cached_at = NULL").
			Where("(url = ? OR preview_url = ?) AND remote_url != ''", cacheURL, cacheURL).Exec(ctx)
		return m.RemoteURL, nil
	}

	// Open Graph image whose content-addressed file disappeared.
	var preview models.StatusPreview
	err = s.db.NewSelect().Model(&preview).
		Where("image = ? AND image_remote != ''", cacheURL).Limit(1).Scan(ctx)
	if err == nil && preview.ImageRemote != "" {
		_, _ = s.db.NewUpdate().Model((*models.StatusPreview)(nil)).
			Set("image = image_remote").Set("cached_at = NULL").
			Where("image = ? AND image_remote != ''", cacheURL).Exec(ctx)
		return preview.ImageRemote, nil
	}

	// Account avatar.
	var a models.Account
	err = s.db.NewSelect().Model(&a).
		Where("avatar = ? AND avatar_remote != ''", cacheURL).Limit(1).Scan(ctx)
	if err == nil && a.AvatarRemote != "" {
		_, _ = s.db.NewUpdate().Model((*models.Account)(nil)).
			Set("avatar = avatar_remote").
			Where("avatar = ? AND avatar_remote != ''", cacheURL).Exec(ctx)
		return a.AvatarRemote, nil
	}

	// Account header.
	var h models.Account
	err = s.db.NewSelect().Model(&h).
		Where("header = ? AND header_remote != ''", cacheURL).Limit(1).Scan(ctx)
	if err == nil && h.HeaderRemote != "" {
		_, _ = s.db.NewUpdate().Model((*models.Account)(nil)).
			Set("header = header_remote").
			Where("header = ? AND header_remote != ''", cacheURL).Exec(ctx)
		return h.HeaderRemote, nil
	}

	// Custom emoji image.
	var e models.CustomEmoji
	err = s.db.NewSelect().Model(&e).
		Where("(url = ? OR static_url = ?) AND remote_url != ''", cacheURL, cacheURL).Limit(1).Scan(ctx)
	if err == nil && e.RemoteURL != "" {
		_, _ = s.db.NewUpdate().Model((*models.CustomEmoji)(nil)).
			Set("url = CASE WHEN url = ? THEN remote_url ELSE url END", cacheURL).
			Set("static_url = CASE WHEN static_url = ? THEN remote_url ELSE static_url END", cacheURL).
			Set("cached_at = NULL").
			Where("(url = ? OR static_url = ?) AND remote_url != ''", cacheURL, cacheURL).Exec(ctx)
		return e.RemoteURL, nil
	}

	return "", nil
}

func (s *bunStorage) RecordMediaCacheFailure(ctx context.Context, url, reason string) error {
	if url == "" {
		return nil
	}
	failure := &models.MediaCacheFailure{URL: url, Reason: reason, FailedAt: time.Now()}
	_, err := s.db.NewInsert().Model(failure).
		On("CONFLICT (url) DO UPDATE").
		Set("reason = EXCLUDED.reason").
		Set("failed_at = EXCLUDED.failed_at").
		Exec(ctx)
	return err
}

func (s *bunStorage) MediaCacheFailedSince(ctx context.Context, url string, cutoff time.Time) (bool, error) {
	if url == "" {
		return false, nil
	}
	return s.db.NewSelect().Model((*models.MediaCacheFailure)(nil)).
		Where("url = ?", url).Where("failed_at >= ?", cutoff).Exists(ctx)
}

func (s *bunStorage) PruneMediaCacheFailures(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.NewDelete().Model((*models.MediaCacheFailure)(nil)).
		Where("failed_at < ?", cutoff).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *bunStorage) GetReferencedCachePaths(ctx context.Context) (map[string]struct{}, error) {
	const query = `
		SELECT url AS u FROM media_attachments WHERE url <> ''
		UNION
		SELECT preview_url AS u FROM media_attachments WHERE preview_url <> ''
		UNION
		SELECT avatar AS u FROM accounts WHERE avatar <> ''
		UNION
		SELECT header AS u FROM accounts WHERE header <> ''
		UNION
		SELECT url AS u FROM custom_emojis WHERE url <> ''
		UNION
		SELECT static_url AS u FROM custom_emojis WHERE static_url <> ''
		UNION
		SELECT image AS u FROM status_previews WHERE image <> ''`

	var urls []string
	if err := s.db.NewRaw(query).Scan(ctx, &urls); err != nil {
		return nil, err
	}
	refs := make(map[string]struct{}, len(urls))
	for _, url := range urls {
		if url != "" {
			refs[url] = struct{}{}
		}
	}
	return refs, nil
}

func (s *bunStorage) GetUnattachedMedia(ctx context.Context, accountID string, olderThan time.Duration) ([]*models.MediaAttachment, error) {
	cutoff := time.Now().Add(-olderThan)
	var media []*models.MediaAttachment
	q := s.db.NewSelect().Model(&media).
		Where("status_id IS NULL").
		Where("created_at < ?", cutoff).
		Where("NOT EXISTS (SELECT 1 FROM accounts WHERE accounts.avatar = media_attachment.url OR accounts.header = media_attachment.url OR accounts.avatar = media_attachment.preview_url OR accounts.header = media_attachment.preview_url)")
	// Scheduled uploads have no status_id until publication, including overdue posts.
	if s.dialect == "pg" {
		q = q.Where("NOT EXISTS (SELECT 1 FROM scheduled_statuses WHERE params_json::jsonb -> 'MediaIDs' @> jsonb_build_array(media_attachment.id))")
	} else {
		q = q.Where("NOT EXISTS (SELECT 1 FROM scheduled_statuses, json_each(scheduled_statuses.params_json, '$.MediaIDs') AS media_id WHERE media_id.value = media_attachment.id)")
	}
	if accountID != "" {
		q = q.Where("account_id = ?", accountID)
	}
	err := q.Scan(ctx)
	return media, err
}

// Notifications

func (s *bunStorage) CreateNotification(ctx context.Context, notif *models.Notification) error {
	if notif.ID == "" {
		notif.ID = newID()
	}
	// Every repeated event refreshes notification ordering and unread state.
	notif.CreatedAt = time.Now()
	if notif.APIID == 0 {
		notif.APIID = newNotificationAPIID()
	}
	_, err := s.db.NewInsert().Model(notif).Exec(ctx)
	return err
}

func (s *bunStorage) CreateNotificationOnce(ctx context.Context, notif *models.Notification, jobs []*models.Job) (bool, error) {
	prepareNotification(notif)
	created := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		created, err = createNotificationOnce(ctx, tx, notif, jobs)
		return err
	})
	if err != nil {
		return false, err
	}
	if created {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return created, nil
}

func prepareNotification(notif *models.Notification) {
	if notif == nil {
		return
	}
	if notif.ID == "" {
		notif.ID = newID()
	}
	if notif.CreatedAt.IsZero() {
		notif.CreatedAt = time.Now()
	}
	if notif.APIID == 0 {
		notif.APIID = newNotificationAPIID()
	}
	if notif.StatusID != nil && *notif.StatusID == "" {
		notif.StatusID = nil
	}

}

func createNotificationOnce(ctx context.Context, tx bun.IDB, notif *models.Notification, jobs []*models.Job) (bool, error) {
	if notif == nil {
		return false, nil
	}
	q := tx.NewInsert().Model(notif)
	if notif.StatusID == nil {
		q = q.On("CONFLICT (account_id, from_account_id, type) WHERE status_id IS NULL DO NOTHING")
	} else {
		q = q.On("CONFLICT (account_id, from_account_id, type, status_id) WHERE status_id IS NOT NULL DO NOTHING")
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}

	return true, insertJobs(ctx, tx, jobs)
}

// FindExistingNotification looks for an existing notification matching the
// given deduplication key (account_id, from_account_id, type, status_id).
// For follow/follow_request notifications, statusID is nil.
func (s *bunStorage) FindExistingNotification(ctx context.Context, accountID, fromAccountID, notifType string, statusID *string) (*models.Notification, error) {
	n := new(models.Notification)
	q := s.db.NewSelect().Model(n).
		Where("account_id = ?", accountID).
		Where("from_account_id = ?", fromAccountID).
		Where("type = ?", notifType)

	if statusID != nil && *statusID != "" {
		q = q.Where("status_id = ?", *statusID)
	} else {
		// For follow/follow_request, there is no status; match on null status_id too.
		q = q.Where("status_id IS NULL")
	}

	err := q.Order("created_at DESC").Limit(1).Scan(ctx)
	return n, err
}

// UpsertNotification creates a notification or refreshes an existing one
// (marks it unread and bumps created_at) if a matching notification exists.
func (s *bunStorage) UpsertNotification(ctx context.Context, notif *models.Notification) error {
	return upsertNotification(ctx, s.db, notif)
}

func (s *bunStorage) UpsertNotifications(ctx context.Context, notifs []*models.Notification) error {
	if len(notifs) == 0 {
		return nil
	}
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for _, notif := range notifs {
			if err := upsertNotification(ctx, tx, notif); err != nil {
				return err
			}
		}
		return nil
	})
}

func upsertNotification(ctx context.Context, db bun.IDB, notif *models.Notification) error {
	if notif == nil {
		return nil
	}
	if notif.ID == "" {
		notif.ID = newID()
	}
	if notif.CreatedAt.IsZero() {
		notif.CreatedAt = time.Now()
	}
	if notif.APIID == 0 {
		notif.APIID = newNotificationAPIID()
	}
	if notif.StatusID != nil && *notif.StatusID == "" {
		notif.StatusID = nil
	}
	q := db.NewInsert().Model(notif)
	if notif.StatusID == nil {
		q = q.On("CONFLICT (account_id, from_account_id, type) WHERE status_id IS NULL DO UPDATE")
	} else {
		q = q.On("CONFLICT (account_id, from_account_id, type, status_id) WHERE status_id IS NOT NULL DO UPDATE")
	}
	_, err := q.Set("read = FALSE").
		Set("created_at = EXCLUDED.created_at").
		Set("emoji = EXCLUDED.emoji").
		Returning("id, api_id, created_at").Exec(ctx)
	return err
}

func (s *bunStorage) GetNotification(ctx context.Context, id string) (*models.Notification, error) {
	notif := new(models.Notification)
	q := s.db.NewSelect().Model(notif)
	if apiID, err := strconv.ParseInt(id, 10, 64); err == nil {
		q = q.Where("api_id = ?", apiID)
	} else {
		q = q.Where("id = ?", id)
	}
	err := q.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return notif, nil
}

func (s *bunStorage) notificationCursor(ctx context.Context, id string) (*models.Notification, bool, error) {
	notif := new(models.Notification)
	q := s.db.NewSelect().Model(notif).Column("id", "created_at", "api_id", "account_id")
	if apiID, err := strconv.ParseInt(id, 10, 64); err == nil {
		q = q.Where("api_id = ?", apiID)
	} else {
		q = q.Where("id = ?", id)
	}
	if err := q.Scan(ctx); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	return notif, true, nil
}

// notificationListQuery builds the shared base query for listing a viewer's
// notifications: type filtering plus moderation. Moderation drops
// notifications from accounts the user blocks (either direction), from
// locally suspended accounts, and from accounts the user muted with
// notifications hidden. The blocked/muted set comes from the warm
// relationship cache (mute expiry computed in Go, like the timelines) so the
// common empty case attaches no predicate, and the suspended-authors subquery
// runs only when the instance actually has suspended accounts - instead of
// four correlated subqueries per read.
func (s *bunStorage) notificationListQuery(ctx context.Context, accountID string, excludeTypes []string) (*bun.SelectQuery, error) {
	q := s.db.NewSelect().Model((*models.Notification)(nil)).
		Where("account_id = ?", accountID)

	q = excludeNotificationTypes(q, excludeTypes)

	rels, err := s.viewerRelsFor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if len(rels.notifHiddenIDs) > 0 {
		// A block/unblock notification's from_account_id is, by definition, an
		// account now in notifHiddenIDs - the relation the notification is
		// reporting. Excluding it here would hide the notification the instant
		// it's created, so those two types skip the hidden-account filter.
		q = q.Where("(from_account_id NOT IN (?) OR type IN ('block', 'unblock'))", bun.In(rels.notifHiddenIDs))
	}

	instance, err := s.instanceRelsFor(ctx)
	if err != nil {
		return nil, err
	}
	if instance.anySuspended {
		q = q.Where("from_account_id NOT IN (SELECT id FROM accounts WHERE suspended_at IS NOT NULL)")
	}

	return q, nil
}

func excludeNotificationTypes(q *bun.SelectQuery, types []string) *bun.SelectQuery {
	if len(types) > 0 {
		q = q.Where("type NOT IN (?)", bun.In(types))
	}
	for _, kind := range types {
		if kind == "follow_import.complete" || kind == "follow_import.failed" {
			q = q.Where("type NOT LIKE ? ESCAPE '\\'", likeEscape(kind)+":%")
		}
	}
	return q
}

func (s *bunStorage) ListNotifications(ctx context.Context, accountID string, maxID *string, limit int, excludeTypes []string) ([]*models.Notification, error) {
	if limit <= 0 {
		limit = 20
	}
	q, err := s.notificationListQuery(ctx, accountID, excludeTypes)
	if err != nil {
		return nil, err
	}
	q = q.Order("created_at DESC").Limit(limit)

	if maxID != nil && *maxID != "" {
		// Resolve either the public numeric API ID or the internal UUID, then
		// page on the compound (created_at, internal id) cursor.
		apiID, numeric := strconv.ParseInt(*maxID, 10, 64)
		if ref, ok, err := s.notificationCursor(ctx, *maxID); err != nil {
			return nil, err
		} else if ok {
			q = q.Where("(created_at < ? OR (created_at = ? AND id < ?))", ref.CreatedAt, ref.CreatedAt, ref.ID)
		} else if numeric == nil {
			q = q.Where("api_id < ?", apiID)
		} else {
			q = q.Where("id < ?", *maxID)
		}
	}

	var notifs []*models.Notification
	err = q.Scan(ctx, &notifs)
	return notifs, err
}

func (s *bunStorage) ListNotificationsSince(ctx context.Context, accountID string, sinceID string, limit int, excludeTypes []string) ([]*models.Notification, error) {
	if limit <= 0 {
		limit = 20
	}
	q, err := s.notificationListQuery(ctx, accountID, excludeTypes)
	if err != nil {
		return nil, err
	}
	q = q.Order("created_at DESC").Limit(limit)

	if sinceID != "" {
		// Resolve either public numeric ID or internal UUID before applying the
		// same compound cursor used by backward pagination.
		apiID, numeric := strconv.ParseInt(sinceID, 10, 64)
		if ref, ok, err := s.notificationCursor(ctx, sinceID); err != nil {
			return nil, err
		} else if ok {
			q = q.Where("(created_at > ? OR (created_at = ? AND id > ?))", ref.CreatedAt, ref.CreatedAt, ref.ID)
		} else if numeric == nil {
			q = q.Where("api_id > ?", apiID)
		} else {
			q = q.Where("id > ?", sinceID)
		}
	}

	var notifs []*models.Notification
	err = q.Scan(ctx, &notifs)
	return notifs, err
}

// Markers

func (s *bunStorage) GetMarkers(ctx context.Context, accountID string, timelines []string) ([]*models.Marker, error) {
	var markers []*models.Marker
	q := s.db.NewSelect().Model(&markers).
		Where("account_id = ?", accountID)
	if len(timelines) > 0 {
		q = q.Where("timeline IN (?)", bun.In(timelines))
	}
	err := q.Scan(ctx)
	return markers, err
}

func (s *bunStorage) SetMarker(ctx context.Context, accountID, timeline, lastReadID string) (*models.Marker, error) {
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO markers (account_id, timeline, last_read_id, version, updated_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT (account_id, timeline) DO UPDATE SET
			last_read_id = excluded.last_read_id,
			version = version + 1,
			updated_at = excluded.updated_at`,
		accountID, timeline, lastReadID, now); err != nil {
		return nil, err
	}
	if timeline == "notifications" {
		if err := s.markNotificationsReadThroughMarker(ctx, accountID, lastReadID); err != nil {
			return nil, err
		}
	}
	// Re-read so the caller returns the bumped version.
	out := new(models.Marker)
	if err := s.db.NewSelect().Model(out).
		Where("account_id = ?", accountID).
		Where("timeline = ?", timeline).
		Scan(ctx); err != nil {
		return &models.Marker{AccountID: accountID, Timeline: timeline, LastReadID: lastReadID, Version: 1, UpdatedAt: now}, nil
	}
	return out, nil
}

func (s *bunStorage) markNotificationsReadThroughMarker(ctx context.Context, accountID, lastReadID string) error {
	ref, ok, err := s.notificationCursor(ctx, lastReadID)
	if err != nil {
		return err
	}
	q := s.db.NewUpdate().Model((*models.Notification)(nil)).
		Set("read = ?", true).
		Where("account_id = ?", accountID).
		Where("read = ?", false)
	if ok {
		if ref.AccountID != accountID {
			return nil
		}
		q = q.Where("(created_at < ? OR (created_at = ? AND id <= ?))", ref.CreatedAt, ref.CreatedAt, ref.ID)
	} else if apiID, parseErr := strconv.ParseInt(lastReadID, 10, 64); parseErr == nil {
		q = q.Where("api_id <= ?", apiID)
	} else {
		return nil
	}
	_, err = q.Exec(ctx)
	return err
}

func (s *bunStorage) MarkNotificationRead(ctx context.Context, id string) error {
	_, err := s.db.NewUpdate().Model((*models.Notification)(nil)).
		Set("read = ?", true).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (s *bunStorage) MarkNotificationsRead(ctx context.Context, accountID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.NewUpdate().Model((*models.Notification)(nil)).
		Set("read = ?", true).
		Where("account_id = ?", accountID).
		Where("id IN (?)", bun.In(ids)).
		Where("read = ?", false).
		Exec(ctx)
	return err
}

func (s *bunStorage) DeleteNotification(ctx context.Context, accountID, id string) error {
	q := s.db.NewDelete().Model((*models.Notification)(nil)).Where("account_id = ?", accountID)
	if apiID, err := strconv.ParseInt(id, 10, 64); err == nil {
		q = q.Where("api_id = ?", apiID)
	} else {
		q = q.Where("id = ?", id)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *bunStorage) MarkAllNotificationsRead(ctx context.Context, accountID string) error {
	_, err := s.db.NewUpdate().Model((*models.Notification)(nil)).
		Set("read = ?", true).
		Where("account_id = ?", accountID).
		Where("read = ?", false).
		Exec(ctx)
	return err
}

func (s *bunStorage) CountUnreadNotifications(ctx context.Context, accountID string) (int, error) {
	return s.CountUnreadNotificationsFiltered(ctx, accountID, nil)
}

func (s *bunStorage) CountUnreadNotificationsFiltered(ctx context.Context, accountID string, excludeTypes []string) (int, error) {
	q := s.db.NewSelect().Model((*models.Notification)(nil)).
		Where("account_id = ?", accountID).
		Where("read = ?", false)
	q = excludeNotificationTypes(q, excludeTypes)
	return q.Count(ctx)
}

func (s *bunStorage) DeleteNotificationsForStatus(ctx context.Context, statusID string) error {
	_, err := s.db.NewDelete().Model((*models.Notification)(nil)).
		Where("status_id = ?", statusID).
		Exec(ctx)
	return err
}

func (s *bunStorage) DeleteNotifications(ctx context.Context, accountID, fromAccountID string, notifType string) error {
	_, err := s.db.NewDelete().Model((*models.Notification)(nil)).
		Where("account_id = ?", accountID).
		Where("from_account_id = ?", fromAccountID).
		Where("type = ?", notifType).
		Exec(ctx)
	return err
}

func (s *bunStorage) DeleteAllNotifications(ctx context.Context, accountID string) error {
	_, err := s.db.NewDelete().Model((*models.Notification)(nil)).
		Where("account_id = ?", accountID).
		Exec(ctx)
	return err
}

// Tags

func (s *bunStorage) GetOrCreateTag(ctx context.Context, name string) (*models.Tag, error) {
	// Tag names are normalised to lowercase so lookups are stable regardless of
	// the casing a remote instance used (e.g. #RunBSD vs #runbsd).
	name = strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(name), "#")))
	if name == "" {
		return nil, fmt.Errorf("tag name is empty")
	}

	tag, err := s.GetTagByName(ctx, name)
	if err == nil {
		return tag, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	tag = &models.Tag{
		ID:   newID(),
		Name: name,
		URL:  "/tags/" + name,
	}
	res, err := s.db.NewInsert().Model(tag).
		On("CONFLICT (name) DO NOTHING").Exec(ctx)
	if err != nil {
		return nil, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if inserted == 0 {
		return s.GetTagByName(ctx, name)
	}
	return tag, nil
}

// Custom emojis

// UpsertEmoji inserts a custom emoji or refreshes its image URLs if a row with
// the same (shortcode, domain) already exists. The served URL is only reset to
// the remote origin when the origin actually changed, so a locally cached image
// is preserved across re-ingestion of the same emoji.
func (s *bunStorage) UpsertEmoji(ctx context.Context, emoji *models.CustomEmoji) error {
	if emoji.Shortcode == "" || emoji.RemoteURL == "" {
		return nil
	}
	existing, err := s.getEmojiByShortcodeDomain(ctx, emoji.Shortcode, emoji.Domain)
	if err == nil && existing != nil {
		// Same origin: nothing to do (preserve any cached URL/cached_at).
		if existing.RemoteURL == emoji.RemoteURL {
			return nil
		}
		existing.URL = emoji.RemoteURL
		existing.StaticURL = emoji.StaticURL
		existing.RemoteURL = emoji.RemoteURL
		existing.CachedAt = nil
		existing.UpdatedAt = time.Now()
		_, uerr := s.db.NewUpdate().Model(existing).WherePK().Exec(ctx)
		return uerr
	}

	now := time.Now()
	emoji.ID = newID()
	emoji.CreatedAt = now
	emoji.UpdatedAt = now
	if emoji.URL == "" {
		emoji.URL = emoji.RemoteURL
	}
	_, err = s.db.NewInsert().Model(emoji).
		On("CONFLICT (shortcode, domain) DO NOTHING").Exec(ctx)
	return err
}

func (s *bunStorage) getEmojiByShortcodeDomain(ctx context.Context, shortcode string, domain *string) (*models.CustomEmoji, error) {
	emoji := new(models.CustomEmoji)
	q := s.db.NewSelect().Model(emoji).Where("shortcode = ?", shortcode)
	if domain == nil {
		q = q.Where("domain IS NULL")
	} else {
		q = q.Where("domain = ?", *domain)
	}
	if err := q.Limit(1).Scan(ctx); err != nil {
		return nil, err
	}
	return emoji, nil
}

func (s *bunStorage) GetEmojisByShortcodes(ctx context.Context, shortcodes []string, domain *string) ([]*models.CustomEmoji, error) {
	var emojis []*models.CustomEmoji
	if len(shortcodes) == 0 {
		return emojis, nil
	}
	q := s.db.NewSelect().Model(&emojis).Where("shortcode IN (?)", bun.In(shortcodes))
	if domain == nil {
		q = q.Where("domain IS NULL")
	} else {
		q = q.Where("domain = ?", *domain)
	}
	err := q.Scan(ctx)
	return emojis, err
}

func (s *bunStorage) GetUncachedRemoteEmoji(ctx context.Context, limit int) ([]*models.CustomEmoji, error) {
	if limit <= 0 {
		// Backstop: an unbounded sort over custom_emojis can blow the
		// 32MB WASM SQLite arena on 32-bit hosts.
		limit = 500
	}
	var emojis []*models.CustomEmoji
	err := s.db.NewSelect().Model(&emojis).
		Where("remote_url != ''").
		Where("cached_at IS NULL").
		// Not yet cached: the served URL still points at the remote origin.
		Where("url = remote_url").
		Order("created_at DESC").
		Limit(limit).
		Scan(ctx)
	return emojis, err
}

func (s *bunStorage) GetEmojisByRemoteURL(ctx context.Context, remoteURL string) ([]*models.CustomEmoji, error) {
	var emojis []*models.CustomEmoji
	if remoteURL == "" {
		return emojis, nil
	}
	err := s.db.NewSelect().Model(&emojis).Where("remote_url = ?", remoteURL).Scan(ctx)
	return emojis, err
}

func (s *bunStorage) UpdateEmoji(ctx context.Context, emoji *models.CustomEmoji) error {
	emoji.UpdatedAt = time.Now()
	_, err := s.db.NewUpdate().Model(emoji).WherePK().Exec(ctx)
	return err
}

func (s *bunStorage) SearchEmojis(ctx context.Context, query string, offset, limit int) ([]*models.CustomEmoji, error) {
	if query == "" {
		return nil, nil
	}
	var emojis []*models.CustomEmoji
	err := s.db.NewSelect().
		Model(&emojis).
		Where("shortcode LIKE ?", query+"%").
		OrderExpr("CASE WHEN domain IS NULL THEN 0 ELSE 1 END").
		Order("shortcode ASC").
		Offset(offset).
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return emojis, nil
}

// ListLocalCustomEmojis returns every locally-hosted custom emoji (domain IS
// NULL), for the /api/v1/custom_emojis discovery endpoint.
func (s *bunStorage) ListLocalCustomEmojis(ctx context.Context) ([]*models.CustomEmoji, error) {
	var emojis []*models.CustomEmoji
	err := s.db.NewSelect().
		Model(&emojis).
		Where("domain IS NULL").
		Order("shortcode ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return emojis, nil
}

func (s *bunStorage) GetTag(ctx context.Context, id string) (*models.Tag, error) {
	tag := new(models.Tag)
	err := s.db.NewSelect().Model(tag).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return tag, nil
}

func (s *bunStorage) GetTagByName(ctx context.Context, name string) (*models.Tag, error) {
	// Match case-insensitively so existing mixed-case rows (ingested before tag
	// names were normalised) still resolve from a lowercased lookup.
	name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "#")))
	tag := new(models.Tag)
	err := s.db.NewSelect().Model(tag).Where("lower(name) = ?", name).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return tag, nil
}

func (s *bunStorage) GetTagTimeline(ctx context.Context, viewerID, tag string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs, localOnly bool) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}

	var statuses []*models.Status
	q := s.db.NewSelect().Model(&statuses).
		Join("JOIN status_tags ON status_tags.status_id = status.id").
		Join("JOIN tags ON tags.id = status_tags.tag_id").
		Where("lower(tags.name) = ?", strings.ToLower(strings.TrimSpace(strings.TrimPrefix(tag, "#")))).
		Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted')").
		Where("status.context_only = FALSE").
		Order("status.created_at DESC").
		Limit(limit)
	if localOnly {
		// Restrict to statuses authored by local accounts (remote accounts have a
		// non-NULL domain). Used for the public/unauthenticated hashtag view so we
		// don't surface uncurated remote posts under this instance's tag namespace.
		q = q.Where("status.account_id IN (SELECT id FROM accounts WHERE domain IS NULL)")
	}
	if excludeReplies {
		q = q.Where("status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')")
	}
	if excludeReblogs {
		q = q.Where("status.boost_of_id IS NULL")
	}

	q, err := s.applyTimelineModeration(ctx, q, viewerID, true)
	if err != nil {
		return nil, err
	}

	q = s.applyStatusPagination(ctx, q, maxID, minID, sinceID)

	err = q.Scan(ctx)
	return statuses, err
}

func (s *bunStorage) IncrementTagCount(ctx context.Context, tagID string) error {
	_, err := s.db.NewUpdate().Model((*models.Tag)(nil)).
		Set("statuses_count = statuses_count + 1").
		Where("id = ?", tagID).
		Exec(ctx)
	return err
}

// Followed Tags

func (s *bunStorage) FollowTag(ctx context.Context, accountID, tagID string) error {
	ft := &models.FollowedTag{
		ID:        newID(),
		CreatedAt: time.Now().UTC(),
		AccountID: accountID,
		TagID:     tagID,
	}
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(ft).
			On("CONFLICT (account_id, tag_id) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
		return backfillHomeFeedFromTag(ctx, tx, accountID, tagID)
	})
	if err == nil {
		s.rels.invalidateViewers(accountID)
	}
	return err
}

func (s *bunStorage) UnfollowTag(ctx context.Context, accountID, tagID string) error {
	_, err := s.db.NewDelete().Model((*models.FollowedTag)(nil)).
		Where("account_id = ?", accountID).
		Where("tag_id = ?", tagID).
		Exec(ctx)
	if err == nil {
		s.rels.invalidateViewers(accountID)
		s.removeTagFromHomeFeed(ctx, accountID, tagID)
	}
	return err
}

func (s *bunStorage) IsFollowingTag(ctx context.Context, accountID, tagID string) (bool, error) {
	return s.db.NewSelect().Model((*models.FollowedTag)(nil)).
		Where("account_id = ?", accountID).
		Where("tag_id = ?", tagID).
		Exists(ctx)
}

func (s *bunStorage) ListFollowedTags(ctx context.Context, accountID string) ([]*models.Tag, error) {
	var tags []*models.Tag
	err := s.db.NewSelect().Model(&tags).
		Join("JOIN followed_tags ON followed_tags.tag_id = tag.id").
		Where("followed_tags.account_id = ?", accountID).
		Order("tag.name ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		t.Following = true
	}
	return tags, nil
}

func (s *bunStorage) GetFollowedTagIDs(ctx context.Context, accountID string) ([]string, error) {
	var ids []string
	err := s.db.NewSelect().Model((*models.FollowedTag)(nil)).
		Column("tag_id").
		Where("account_id = ?", accountID).
		Scan(ctx, &ids)
	return ids, err
}

// Featured Tags

func (s *bunStorage) CreateFeaturedTag(ctx context.Context, accountID, tagID string) (*models.FeaturedTag, error) {
	ft := &models.FeaturedTag{
		ID:        newID(),
		CreatedAt: time.Now().UTC(),
		AccountID: accountID,
		TagID:     tagID,
	}
	res, err := s.db.NewInsert().Model(ft).
		On("CONFLICT (account_id, tag_id) DO NOTHING").Exec(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, ErrFeaturedTagExists
	}
	return ft, nil
}

func (s *bunStorage) DeleteFeaturedTag(ctx context.Context, accountID, featuredTagID string) (bool, error) {
	res, err := s.db.NewDelete().Model((*models.FeaturedTag)(nil)).
		Where("id = ?", featuredTagID).
		Where("account_id = ?", accountID).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows > 0, err
}

func (s *bunStorage) ListFeaturedTags(ctx context.Context, accountID string) ([]*models.FeaturedTagView, error) {
	var views []*models.FeaturedTagView
	err := s.db.NewSelect().
		Model((*models.FeaturedTag)(nil)).
		ColumnExpr("featured_tag.id AS id").
		ColumnExpr("tags.name AS name").
		ColumnExpr("tags.url AS url").
		ColumnExpr("(SELECT COUNT(*) FROM status_tags st JOIN statuses s ON s.id = st.status_id WHERE st.tag_id = tags.id AND s.account_id = ?) AS statuses_count", accountID).
		ColumnExpr("(SELECT MAX(s.created_at) FROM status_tags st JOIN statuses s ON s.id = st.status_id WHERE st.tag_id = tags.id AND s.account_id = ?) AS last_status_at", accountID).
		Join("JOIN tags ON tags.id = featured_tag.tag_id").
		Where("featured_tag.account_id = ?", accountID).
		Order("tags.name ASC").
		Scan(ctx, &views)
	if err != nil {
		return nil, err
	}
	return views, nil
}

// Bubble Timeline

func (s *bunStorage) GetBubbleTimeline(ctx context.Context, viewerID string, maxID, minID, sinceID *string, limit int, excludeReplies, excludeReblogs bool) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}

	// Get bubble instance domains
	var domains []string
	err := s.db.NewSelect().
		Model((*models.BubbleInstance)(nil)).
		Column("domain").
		Scan(ctx, &domains)
	if err != nil {
		return nil, err
	}

	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.context_only = FALSE").
		Order("status.created_at DESC").
		Limit(limit)

	if len(domains) > 0 {
		q = q.Where("status.account_id IN (SELECT id FROM accounts WHERE domain IS NULL OR domain IN (?))", bun.In(domains))
	} else {
		q = q.Where("status.account_id IN (SELECT id FROM accounts WHERE domain IS NULL)")
	}

	q = q.Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted')")

	if excludeReplies {
		q = q.Where("status.in_reply_to_id IS NULL AND (status.in_reply_to_uri IS NULL OR status.in_reply_to_uri = '')")
	}
	if excludeReblogs {
		q = q.Where("status.boost_of_id IS NULL")
	}

	q, err = s.applyTimelineModeration(ctx, q, viewerID, true)
	if err != nil {
		return nil, err
	}

	q = s.applyStatusPagination(ctx, q, maxID, minID, sinceID)

	var statuses []*models.Status
	err = q.Scan(ctx, &statuses)
	return statuses, err
}

// Mentions

func (s *bunStorage) CreateMention(ctx context.Context, mention *models.Mention) error {
	if mention.ID == "" {
		mention.ID = newID()
	}
	_, err := s.db.NewInsert().Model(mention).Exec(ctx)
	return err
}

func (s *bunStorage) GetMentionsForStatus(ctx context.Context, statusID string) ([]*models.Mention, error) {
	var mentions []*models.Mention
	err := s.db.NewSelect().Model(&mentions).Where("status_id = ?", statusID).Scan(ctx)
	return mentions, err
}

func (s *bunStorage) GetTagsForStatus(ctx context.Context, statusID string) ([]*models.Tag, error) {
	var tags []*models.Tag
	err := s.db.NewSelect().Model(&tags).
		Join("JOIN status_tags ON status_tags.tag_id = tag.id").
		Where("status_tags.status_id = ?", statusID).
		Scan(ctx)
	return tags, err
}

func (s *bunStorage) CreateMentions(ctx context.Context, mentions []*models.Mention) error {
	if len(mentions) == 0 {
		return nil
	}
	for _, m := range mentions {
		if m.ID == "" {
			m.ID = newID()
		}
	}
	_, err := s.db.NewInsert().Model(&mentions).Exec(ctx)
	return err
}

// Blocks

func (s *bunStorage) CreateBlock(ctx context.Context, block *models.Block) error {
	if block.ID == "" {
		block.ID = newID()
	}
	if block.CreatedAt.IsZero() {
		block.CreatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(block).
		On("CONFLICT (account_id, target_account_id) DO NOTHING").
		Exec(ctx)
	if err == nil {
		s.rels.invalidateViewers(block.AccountID, block.TargetAccountID)
	}
	return err
}

func (s *bunStorage) DeleteBlock(ctx context.Context, accountID, targetID string) error {
	_, err := s.db.NewDelete().Model((*models.Block)(nil)).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Exec(ctx)
	if err == nil {
		s.rels.invalidateViewers(accountID, targetID)
	}
	return err
}

// BlockExists reports whether accountID has blocked targetID (one direction).
func (s *bunStorage) BlockExists(ctx context.Context, accountID, targetID string) (bool, error) {
	count, err := s.db.NewSelect().Model((*models.Block)(nil)).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *bunStorage) ListBlocks(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 40
	}
	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts).
		Join("JOIN blocks ON blocks.target_account_id = account.id").
		Where("blocks.account_id = ?", accountID).
		Order("blocks.created_at DESC").
		Limit(limit)
	if maxID != nil && *maxID != "" {
		q = q.Where("blocks.id < ?", *maxID)
	}
	err := q.Scan(ctx)
	return accounts, err
}

// BlockedAccountIDs returns the IDs of accounts on either side of a block
// relationship with accountID: accounts it blocks and accounts that block it.
// Both directions hide content in Mastodon, so timelines exclude all of them.
func (s *bunStorage) BlockedAccountIDs(ctx context.Context, accountID string) ([]string, error) {
	var forward []string
	if err := s.db.NewSelect().Model((*models.Block)(nil)).
		Column("target_account_id").
		Where("account_id = ?", accountID).
		Scan(ctx, &forward); err != nil {
		return nil, err
	}
	var reverse []string
	if err := s.db.NewSelect().Model((*models.Block)(nil)).
		Column("account_id").
		Where("target_account_id = ?", accountID).
		Scan(ctx, &reverse); err != nil {
		return nil, err
	}
	return append(forward, reverse...), nil
}

// Mutes

func (s *bunStorage) CreateMute(ctx context.Context, mute *models.Mute) error {
	if mute.ID == "" {
		mute.ID = newID()
	}
	if mute.CreatedAt.IsZero() {
		mute.CreatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(mute).Exec(ctx)
	if err == nil {
		s.rels.invalidateViewers(mute.AccountID)
	}
	return err
}

func (s *bunStorage) DeleteMute(ctx context.Context, accountID, targetID string) error {
	_, err := s.db.NewDelete().Model((*models.Mute)(nil)).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Exec(ctx)
	if err == nil {
		s.rels.invalidateViewers(accountID)
	}
	return err
}

func (s *bunStorage) IsMuted(ctx context.Context, accountID, targetID string) (bool, error) {
	count, err := s.db.NewSelect().Model((*models.Mute)(nil)).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *bunStorage) ListMutes(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Account, error) {
	if limit <= 0 {
		limit = 40
	}
	var accounts []*models.Account
	q := s.db.NewSelect().Model(&accounts).
		Join("JOIN mutes ON mutes.target_account_id = account.id").
		Where("mutes.account_id = ?", accountID).
		Order("mutes.created_at DESC").
		Limit(limit)
	if maxID != nil && *maxID != "" {
		q = q.Where("mutes.id < ?", *maxID)
	}
	err := q.Scan(ctx)
	return accounts, err
}

// muteExpired reports whether a duration-limited mute has elapsed. Expiry is
// computed in Go to stay portable across SQLite and PostgreSQL (no DB date math).
func muteExpired(m *models.Mute, now time.Time) bool {
	return m.Duration != nil && m.CreatedAt.Add(time.Duration(*m.Duration)*time.Minute).Before(now)
}

// MutedAccountIDs returns the target IDs of the account's active (unexpired) mutes.
func (s *bunStorage) MutedAccountIDs(ctx context.Context, accountID string) ([]string, error) {
	var mutes []*models.Mute
	if err := s.db.NewSelect().Model(&mutes).
		Where("account_id = ?", accountID).
		Scan(ctx); err != nil {
		return nil, err
	}
	now := time.Now()
	ids := make([]string, 0, len(mutes))
	for _, m := range mutes {
		if muteExpired(m, now) {
			continue
		}
		ids = append(ids, m.TargetAccountID)
	}
	return ids, nil
}

// DeleteExpiredMutes removes duration-limited mutes whose window has elapsed
// and returns the muting accounts' IDs (deduplicated) so callers can scope
// cache invalidation to just those viewers.
func (s *bunStorage) DeleteExpiredMutes(ctx context.Context) ([]string, error) {
	var mutes []*models.Mute
	if err := s.db.NewSelect().Model(&mutes).
		Where("duration IS NOT NULL").
		Scan(ctx); err != nil {
		return nil, err
	}
	now := time.Now()
	var expired []string
	viewerSet := make(map[string]struct{})
	var viewers []string
	for _, m := range mutes {
		if muteExpired(m, now) {
			expired = append(expired, m.ID)
			if _, ok := viewerSet[m.AccountID]; !ok {
				viewerSet[m.AccountID] = struct{}{}
				viewers = append(viewers, m.AccountID)
			}
		}
	}
	if len(expired) == 0 {
		return nil, nil
	}
	_, err := s.db.NewDelete().Model((*models.Mute)(nil)).
		Where("id IN (?)", bun.In(expired)).Exec(ctx)
	if err != nil {
		return nil, err
	}
	s.rels.invalidateViewers(viewers...)
	return viewers, nil
}

// applyTimelineModeration excludes statuses (and boosts of statuses) whose author
// should not be shown to the viewer. It always hides locally suspended authors;
// it hides the viewer's blocked/muted accounts when viewerID is set; and, for
// shared timelines (public/tag/bubble), it also hides accounts on silenced or
// suspended domains. Silenced *domains* are enforced purely here at read time -
// inbound silenced content is still stored.
func (s *bunStorage) applyTimelineModeration(ctx context.Context, q *bun.SelectQuery, viewerID string, includeDomainSilence bool) (*bun.SelectQuery, error) {
	excludeAuthors := func(query *bun.SelectQuery, sub string) *bun.SelectQuery {
		return query.
			Where("status.account_id NOT IN (" + sub + ")").
			Where("(status.boost_of_id IS NULL OR status.boost_of_id NOT IN (SELECT id FROM statuses WHERE account_id IN (" + sub + ")))")
	}

	if viewerID != "" {
		rels, err := s.viewerRelsFor(ctx, viewerID)
		if err != nil {
			return q, err
		}
		if hidden := rels.hiddenAuthorIDs; len(hidden) > 0 {
			q = q.
				Where("status.account_id NOT IN (?)", bun.In(hidden)).
				Where("(status.boost_of_id IS NULL OR status.boost_of_id NOT IN (SELECT id FROM statuses WHERE account_id IN (?)))", bun.In(hidden))
		}
		// Per-user domain blocks: hide every account on a domain the viewer blocked.
		if rels.hasDomainBlocks {
			domainSub := "SELECT a.id FROM accounts AS a JOIN account_domain_blocks AS adb ON a.domain = adb.domain WHERE adb.account_id = ?"
			q = q.
				Where("status.account_id NOT IN ("+domainSub+")", viewerID).
				Where("(status.boost_of_id IS NULL OR status.boost_of_id NOT IN (SELECT id FROM statuses WHERE account_id IN ("+domainSub+")))", viewerID)
		}
	}

	// Instance-wide moderation subqueries only run when the matching sets are
	// non-empty; on an unmoderated instance the timeline query stays minimal.
	inst, err := s.instanceRelsFor(ctx)
	if err != nil {
		return q, err
	}

	// Locally suspended accounts are hidden from everyone, everywhere.
	if inst.anySuspended {
		q = excludeAuthors(q, "SELECT id FROM accounts WHERE suspended_at IS NOT NULL")
	}

	if includeDomainSilence {
		if inst.anySilencedDomains {
			q = excludeAuthors(q, "SELECT a.id FROM accounts AS a JOIN domain_blocks AS d ON a.domain = d.domain WHERE d.severity IN ('silence', 'suspend')")
		}
		// Account-level silence/quiet: keep silent authors out of shared timelines
		// (they remain visible to their own followers' home timelines).
		if inst.anySilencedAccounts {
			q = excludeAuthors(q, "SELECT id FROM accounts WHERE moderation_state IN ('silenced', 'quiet')")
		}
	}
	return q, nil
}

// Domain Blocks

func normalizeDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	u, err := url.Parse("https://" + domain)
	if err != nil || domain == "" || net.ParseIP(domain) != nil || u.User != nil || u.Hostname() != domain || u.Port() != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || len(domain) > 253 {
		return "", fmt.Errorf("%w: %q", ErrInvalidDomainBlock, domain)
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("%w: %q", ErrInvalidDomainBlock, domain)
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", fmt.Errorf("%w: %q", ErrInvalidDomainBlock, domain)
			}
		}
	}
	return domain, nil
}

func normalizeInstanceDomain(domain string) (string, error) {
	u, err := url.Parse("https://" + strings.TrimSpace(domain))
	if err != nil || u.User != nil || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidDomainBlock, domain)
	}
	return normalizeDomain(u.Hostname())
}

// NormalizeFederationDomain validates and canonicalizes an exact DNS hostname.
func NormalizeFederationDomain(domain string) (string, error) { return normalizeDomain(domain) }

func (s *bunStorage) CreateDomainBlock(ctx context.Context, block *models.DomainBlock) error {
	domain, err := normalizeDomain(block.Domain)
	if err != nil {
		return err
	}
	switch block.Severity {
	case "noop", "silence", "suspend":
	default:
		return fmt.Errorf("%w: unknown severity %q", ErrInvalidDomainBlock, block.Severity)
	}
	block.Domain = domain
	if block.ID == "" {
		block.ID = newID()
	}
	if block.CreatedAt.IsZero() {
		block.CreatedAt = time.Now()
	}
	_, err = s.db.NewInsert().Model(block).Exec(ctx)
	if err == nil {
		s.rels.invalidateInstance()
		s.policyChanged(ctx)
	}
	return err
}

func (s *bunStorage) ImportDomainBlocks(ctx context.Context, blocks []*models.DomainBlock) (int, error) {
	seen := make(map[string]struct{}, len(blocks))
	unique := blocks[:0]
	for _, block := range blocks {
		domain, err := normalizeDomain(block.Domain)
		if err != nil {
			return 0, err
		}
		switch block.Severity {
		case "noop", "silence", "suspend":
		default:
			return 0, fmt.Errorf("%w: unknown severity %q", ErrInvalidDomainBlock, block.Severity)
		}
		if _, ok := seen[domain]; ok {
			continue
		}
		seen[domain], block.Domain = struct{}{}, domain
		if block.ID == "" {
			block.ID = newID()
		}
		if block.CreatedAt.IsZero() {
			block.CreatedAt = time.Now()
		}
		unique = append(unique, block)
	}
	inserted := 0
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for _, block := range unique {
			result, err := tx.NewInsert().Model(block).On("CONFLICT (domain) DO NOTHING").Exec(ctx)
			if err != nil {
				return err
			}
			n, _ := result.RowsAffected()
			inserted += int(n)
		}
		return nil
	})
	if err == nil {
		s.rels.invalidateInstance()
		s.policyChanged(ctx)
	}
	return inserted, err
}

func (s *bunStorage) DeleteDomainBlock(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.DomainBlock)(nil)).Where("id = ?", id).Exec(ctx)
	if err == nil {
		s.rels.invalidateInstance()
		s.policyChanged(ctx)
	}
	return err
}

func (s *bunStorage) GetDomainBlock(ctx context.Context, domain string) (*models.DomainBlock, error) {
	domain, err := normalizeDomain(domain)
	if err != nil {
		return nil, err
	}
	block := new(models.DomainBlock)
	err = s.db.NewSelect().Model(block).Where("domain = ?", domain).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return block, nil
}

func (s *bunStorage) GetDomainBlockByID(ctx context.Context, id string) (*models.DomainBlock, error) {
	block := new(models.DomainBlock)
	err := s.db.NewSelect().Model(block).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return block, nil
}

func (s *bunStorage) ListDomainBlocks(ctx context.Context) ([]*models.DomainBlock, error) {
	var blocks []*models.DomainBlock
	err := s.db.NewSelect().Model(&blocks).Order("created_at DESC").Scan(ctx)
	return blocks, err
}

// Per-user Domain Blocks

func (s *bunStorage) CreateAccountDomainBlock(ctx context.Context, accountID, domain string) error {
	block := &models.AccountDomainBlock{
		ID:        newID(),
		CreatedAt: time.Now(),
		AccountID: accountID,
		Domain:    domain,
	}
	_, err := s.db.NewInsert().Model(block).
		On("CONFLICT (account_id, domain) DO NOTHING").
		Exec(ctx)
	if err == nil {
		s.rels.invalidateViewers(accountID)
	}
	return err
}

func (s *bunStorage) DeleteAccountDomainBlock(ctx context.Context, accountID, domain string) error {
	_, err := s.db.NewDelete().Model((*models.AccountDomainBlock)(nil)).
		Where("account_id = ?", accountID).
		Where("domain = ?", domain).
		Exec(ctx)
	if err == nil {
		s.rels.invalidateViewers(accountID)
	}
	return err
}

func (s *bunStorage) ListAccountDomainBlocks(ctx context.Context, accountID string) ([]string, error) {
	var domains []string
	err := s.db.NewSelect().Model((*models.AccountDomainBlock)(nil)).
		Column("domain").
		Where("account_id = ?", accountID).
		Order("domain ASC").
		Scan(ctx, &domains)
	return domains, err
}

// Moderation Audit Log

func (s *bunStorage) CreateModerationLog(ctx context.Context, entry *models.ModerationLog) error {
	if entry.ID == "" {
		entry.ID = newID()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(entry).Exec(ctx)
	return err
}

func (s *bunStorage) ListModerationLog(ctx context.Context, maxID *string, limit int) ([]*models.ModerationLog, error) {
	if limit <= 0 {
		limit = 50
	}
	var entries []*models.ModerationLog
	q := s.db.NewSelect().Model(&entries).Order("created_at DESC").Limit(limit)
	if maxID != nil && *maxID != "" {
		q = q.Where("id < ?", *maxID)
	}
	err := q.Scan(ctx)
	return entries, err
}

// Reports

func (s *bunStorage) CreateReport(ctx context.Context, report *models.Report) error {
	if report.ID == "" {
		report.ID = newID()
	}
	if report.CreatedAt.IsZero() {
		report.CreatedAt = time.Now()
	}
	if report.State == "" {
		report.State = "open"
	}
	if report.Category == "" {
		report.Category = "other"
	}
	_, err := s.db.NewInsert().Model(report).Exec(ctx)
	return err
}

func (s *bunStorage) GetReport(ctx context.Context, id string) (*models.Report, error) {
	report := new(models.Report)
	if err := s.db.NewSelect().Model(report).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return report, nil
}

func (s *bunStorage) ListReports(ctx context.Context, state string, maxID *string, limit int) ([]*models.Report, error) {
	if limit <= 0 {
		limit = 50
	}
	var reports []*models.Report
	q := s.db.NewSelect().Model(&reports).Order("created_at DESC").Limit(limit)
	if state != "" {
		q = q.Where("state = ?", state)
	}
	if maxID != nil && *maxID != "" {
		q = q.Where("id < ?", *maxID)
	}
	err := q.Scan(ctx)
	return reports, err
}

func (s *bunStorage) ResolveReport(ctx context.Context, id, adminID, action string) error {
	now := time.Now()
	_, err := s.db.NewUpdate().Model((*models.Report)(nil)).
		Set("state = ?", "resolved").
		Set("assigned_admin_id = ?", adminID).
		Set("action_taken = ?", action).
		Set("resolved_at = ?", now).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (s *bunStorage) CountOpenReports(ctx context.Context) (int, error) {
	return s.db.NewSelect().Model((*models.Report)(nil)).Where("state = ?", "open").Count(ctx)
}

// Bubble Instances

func (s *bunStorage) CreateBubbleInstance(ctx context.Context, b *models.BubbleInstance) error {
	if b.ID == "" {
		b.ID = newID()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(b).Exec(ctx)
	return err
}

func (s *bunStorage) DeleteBubbleInstance(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.BubbleInstance)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) ListBubbleInstances(ctx context.Context) ([]*models.BubbleInstance, error) {
	var instances []*models.BubbleInstance
	err := s.db.NewSelect().Model(&instances).Order("created_at DESC").Scan(ctx)
	return instances, err
}

// Instances

func (s *bunStorage) CreateInstance(ctx context.Context, instance *models.Instance) error {
	domain, err := normalizeInstanceDomain(instance.Domain)
	if err != nil {
		return err
	}
	instance.Domain = domain
	if instance.ID == "" {
		instance.ID = newID()
	}
	if instance.CreatedAt.IsZero() {
		instance.CreatedAt = time.Now()
	}
	if instance.UpdatedAt.IsZero() {
		instance.UpdatedAt = time.Now()
	}
	_, err = s.db.NewInsert().Model(instance).Exec(ctx)
	return err
}

func (s *bunStorage) GetInstance(ctx context.Context, domain string) (*models.Instance, error) {
	domain, err := normalizeInstanceDomain(domain)
	if err != nil {
		return nil, err
	}
	instance := new(models.Instance)
	err = s.db.NewSelect().Model(instance).Where("domain = ?", domain).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

func (s *bunStorage) GetInstanceByID(ctx context.Context, id string) (*models.Instance, error) {
	instance := new(models.Instance)
	err := s.db.NewSelect().Model(instance).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

func (s *bunStorage) UpdateInstance(ctx context.Context, instance *models.Instance) error {
	domain, err := normalizeInstanceDomain(instance.Domain)
	if err != nil {
		return err
	}
	instance.Domain = domain
	instance.UpdatedAt = time.Now()
	_, err = s.db.NewUpdate().Model(instance).WherePK().Exec(ctx)
	s.reach.invalidate(instance.Domain)
	// The update may flip Suspended, which feeds the cached inbound-gate flags.
	s.rels.invalidateInstance()
	if err == nil {
		s.policyChanged(ctx)
	}
	return err
}

func (s *bunStorage) ListInstances(ctx context.Context) ([]*models.Instance, error) {
	var instances []*models.Instance
	err := s.db.NewSelect().Model(&instances).Order("domain ASC").Scan(ctx)
	return instances, err
}

func (s *bunStorage) CountDomains(ctx context.Context) (int, error) {
	return s.db.NewSelect().Model((*models.Instance)(nil)).Count(ctx)
}

func (s *bunStorage) MarkInstanceUnreachable(ctx context.Context, domain string) (bool, error) {
	s.reach.invalidate(domain)
	now := time.Now()
	// Check if the instance is already unreachable
	instance, getErr := s.GetInstance(ctx, domain)
	wasReachable := getErr != nil || instance.UnreachableSince == nil
	update := func() (int64, error) {
		// Preserve the start of the failure streak so the offline clock measures
		// continuous unreachability, not time since the last failure.
		res, err := s.db.NewUpdate().Model((*models.Instance)(nil)).
			Set("unreachable_since = COALESCE(unreachable_since, ?)", now).
			Set("backoff_count = backoff_count + 1").
			Set("last_failure = ?", now).
			Set("updated_at = ?", now).
			Where("domain = ?", domain).
			Exec(ctx)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	n, err := update()
	if err != nil || n > 0 {
		return wasReachable, err
	}
	// No existing row - e.g. a media CDN host we've never federated with. Create
	// one so the circuit breaker can track it. A concurrent worker may win the
	// insert race, in which case we fall back to incrementing the existing row.
	if err := s.CreateInstance(ctx, &models.Instance{
		Domain:           domain,
		UnreachableSince: &now,
		BackoffCount:     1,
		LastFailure:      &now,
	}); err != nil {
		_, uerr := update()
		return true, uerr
	}
	return true, nil
}

func (s *bunStorage) MarkInstanceReachable(ctx context.Context, domain string) error {
	// Every successful delivery and every inbound activity marks the sender
	// reachable; skip the UPDATE when it was already written recently. Any
	// MarkInstanceUnreachable clears the throttle, so the first success after
	// a failure always clears the streak in the DB immediately.
	if !s.reach.shouldWriteReachable(domain) {
		return nil
	}
	now := time.Now()
	_, err := s.db.NewUpdate().Model((*models.Instance)(nil)).
		Set("unreachable_since = NULL").
		Set("backoff_count = 0").
		Set("last_success = ?", now).
		Set("updated_at = ?", now).
		Where("domain = ?", domain).
		Exec(ctx)
	if err != nil {
		s.reach.undoWriteReachable(domain)
		return err
	}
	s.reach.invalidateMemo(domain)
	return nil
}

// InstanceOfflineWindow is how long an instance must be continuously unreachable
// before we treat it as offline and stop attempting delivery to it (Mastodon's
// model, where it uses a 7-day failure window - we keep trying for 14 days).
// Until then we keep delivering through transient outages; afterwards any inbound
// activity from the instance revives it and resumes all normal federation.
const InstanceOfflineWindow = 14 * 24 * time.Hour

// IsInstanceReachable reports whether we should still attempt delivery/fetch to
// the host. We keep trying through transient failures and only give up once the
// host has been continuously unreachable for InstanceOfflineWindow, at which
// point it is considered offline until inbound activity revives it. Manual
// suspension is always a hard block.
func (s *bunStorage) IsInstanceReachable(ctx context.Context, domain string) (bool, error) {
	// Delivery fan-out asks once per inbox at enqueue time and once per job
	// at dequeue time; memoize briefly instead of a point SELECT each time.
	if reachable, ok := s.reach.getMemo(domain); ok {
		return reachable, nil
	}
	instance, err := s.GetInstance(ctx, domain)
	if err != nil {
		if err == sql.ErrNoRows {
			// No record yet = first contact, assume reachable
			s.reach.setMemo(domain, true)
			return true, nil
		}
		return false, err
	}
	reachable := true
	if instance.Suspended {
		reachable = false
	} else if instance.UnreachableSince != nil && time.Since(*instance.UnreachableSince) >= InstanceOfflineWindow {
		reachable = false
	}
	s.reach.setMemo(domain, reachable)
	return reachable, nil
}

// IsInstanceMediaCacheable reports whether to attempt remote media caching from
// the host. Unlike delivery, media caching is non-critical, so we pause it as
// soon as the host has an open failure streak (it is "not reachable") instead of
// hammering it, and resume once a successful delivery/fetch or inbound activity
// clears the streak via MarkInstanceReachable.
func (s *bunStorage) IsInstanceMediaCacheable(ctx context.Context, domain string) (bool, error) {
	instance, err := s.GetInstance(ctx, domain)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, nil
		}
		return false, err
	}
	if instance.Suspended {
		return false, nil
	}
	if instance.UnreachableSince != nil {
		return false, nil
	}
	return true, nil
}

// CountUnreachableInstances returns the number of instances we have stopped
// contacting: manually suspended, or offline (continuously unreachable past
// InstanceOfflineWindow). Instances merely failing transiently are not counted -
// they are a normal part of federation and require no operator action.
func (s *bunStorage) CountUnreachableInstances(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-InstanceOfflineWindow)
	count, err := s.db.NewSelect().Model((*models.Instance)(nil)).
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("suspended = ?", true).
				WhereOr("unreachable_since IS NOT NULL AND unreachable_since <= ?", cutoff)
		}).
		Count(ctx)
	return count, err
}

// SuspendInstance sets or clears the suspended flag on an instance.
func (s *bunStorage) SuspendInstance(ctx context.Context, id string, suspended bool) error {
	now := time.Now()
	_, err := s.db.NewUpdate().Model((*models.Instance)(nil)).
		Set("suspended = ?", suspended).
		Set("updated_at = ?", now).
		Where("id = ?", id).
		Exec(ctx)
	// Only the ID is known here; suspend is a rare admin action, wipe all.
	s.reach.invalidateAll()
	s.rels.invalidateInstance()
	if err == nil {
		s.policyChanged(ctx)
	}
	return err
}

// DeleteInstanceData removes all database records related to a remote instance.
// This is useful when an instance changes its keys and we want to start fresh.
func (s *bunStorage) DeleteInstanceData(ctx context.Context, id string) error {
	instance, err := s.GetInstanceByID(ctx, id)
	if err != nil {
		return err
	}
	s.reach.invalidate(instance.Domain)
	accountSub := `(SELECT id FROM accounts WHERE domain = ?)`
	statusSub := `(SELECT id FROM statuses WHERE account_id IN ` + accountSub + `)`
	affected := `(SELECT id FROM statuses WHERE account_id IN ` + accountSub + ` OR boost_of_id IN ` + statusSub + `)`
	err = s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		queries := []struct {
			query string
			args  []any
		}{
			{`DELETE FROM poll_votes WHERE account_id IN ` + accountSub + ` OR poll_id IN (SELECT id FROM polls WHERE status_id IN ` + affected + `)`, []any{instance.Domain, instance.Domain, instance.Domain}},
			{`DELETE FROM poll_options WHERE poll_id IN (SELECT id FROM polls WHERE status_id IN ` + affected + `)`, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM polls WHERE status_id IN ` + affected, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM quote_authorizations WHERE quoted_status_id IN ` + affected + ` OR quoting_uri IN (SELECT uri FROM statuses WHERE id IN ` + affected + `)`, []any{instance.Domain, instance.Domain, instance.Domain, instance.Domain}},
			{`DELETE FROM notifications WHERE account_id IN ` + accountSub + ` OR from_account_id IN ` + accountSub + ` OR status_id IN ` + affected, []any{instance.Domain, instance.Domain, instance.Domain, instance.Domain}},
			{`DELETE FROM status_tags WHERE status_id IN ` + affected, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM mentions WHERE account_id IN ` + accountSub + ` OR status_id IN ` + affected, []any{instance.Domain, instance.Domain, instance.Domain}},
			{`DELETE FROM favourites WHERE account_id IN ` + accountSub + ` OR status_id IN ` + affected, []any{instance.Domain, instance.Domain, instance.Domain}},
			{`DELETE FROM bookmarks WHERE account_id IN ` + accountSub + ` OR status_id IN ` + affected, []any{instance.Domain, instance.Domain, instance.Domain}},
			{`DELETE FROM status_previews WHERE status_id IN ` + affected, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM home_feed WHERE account_id IN ` + accountSub + ` OR author_account_id IN ` + accountSub + ` OR status_id IN ` + affected, []any{instance.Domain, instance.Domain, instance.Domain, instance.Domain}},
			{`DELETE FROM media_attachments WHERE account_id IN ` + accountSub + ` OR status_id IN ` + affected, []any{instance.Domain, instance.Domain, instance.Domain}},
			{`UPDATE statuses SET in_reply_to_id = NULL WHERE in_reply_to_id IN ` + affected, []any{instance.Domain, instance.Domain}},
			{`UPDATE statuses SET quote_of_id = NULL WHERE quote_of_id IN ` + affected, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM statuses WHERE id IN ` + affected, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM list_accounts WHERE account_id IN ` + accountSub, []any{instance.Domain}},
			{`DELETE FROM blocks WHERE account_id IN ` + accountSub + ` OR target_account_id IN ` + accountSub, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM mutes WHERE account_id IN ` + accountSub + ` OR target_account_id IN ` + accountSub, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM follows WHERE account_id IN ` + accountSub + ` OR target_account_id IN ` + accountSub, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM relays WHERE account_id IN ` + accountSub, []any{instance.Domain}},
			{`DELETE FROM reports WHERE account_id IN ` + accountSub + ` OR target_account_id IN ` + accountSub, []any{instance.Domain, instance.Domain}},
			{`DELETE FROM accounts WHERE domain = ?`, []any{instance.Domain}},
			{`DELETE FROM instances WHERE id = ?`, []any{id}},
			{`UPDATE tags SET statuses_count = (SELECT COUNT(*) FROM status_tags WHERE status_tags.tag_id = tags.id)`, nil},
			{`UPDATE statuses SET replies_count = (SELECT COUNT(*) FROM statuses child WHERE child.in_reply_to_id = statuses.id AND child.visibility <> 'direct'), favourites_count = (SELECT COUNT(*) FROM favourites WHERE favourites.status_id = statuses.id), reblogs_count = (SELECT COUNT(*) FROM statuses boost WHERE boost.boost_of_id = statuses.id)`, nil},
		}
		for _, q := range queries {
			if _, err := tx.ExecContext(ctx, q.query, q.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.InvalidateRelationshipCaches()
	return s.RepairLocalAccountCounts(ctx)
}

// CountFollowersByDomain returns how many local accounts follow remote accounts on the given domain.
func (s *bunStorage) CountFollowersByDomain(ctx context.Context, domain string) (int, error) {
	return s.db.NewSelect().
		Model((*models.Follow)(nil)).
		Join("JOIN accounts AS target ON target.id = follow.target_account_id").
		Join("JOIN accounts AS follower ON follower.id = follow.account_id").
		Where("target.domain = ?", domain).
		Where("follower.domain IS NULL").
		Count(ctx)
}

// CountFollowingByDomain returns how many remote accounts on the given domain follow local accounts.
func (s *bunStorage) CountFollowingByDomain(ctx context.Context, domain string) (int, error) {
	return s.db.NewSelect().
		Model((*models.Follow)(nil)).
		Join("JOIN accounts AS follower ON follower.id = follow.account_id").
		Join("JOIN accounts AS target ON target.id = follow.target_account_id").
		Where("follower.domain = ?", domain).
		Where("target.domain IS NULL").
		Count(ctx)
}

// OAuth

func (s *bunStorage) CreateOAuthApp(ctx context.Context, app *models.OAuthApplication) error {
	if app.ID == "" {
		app.ID = newID()
	}
	if app.CreatedAt.IsZero() {
		app.CreatedAt = time.Now()
	}
	stored := *app
	stored.ClientSecret = hashBearerSecret(app.ClientSecret)
	_, err := s.db.NewInsert().Model(&stored).Exec(ctx)
	return err
}

func (s *bunStorage) GetOAuthAppByClientID(ctx context.Context, clientID string) (*models.OAuthApplication, error) {
	app := new(models.OAuthApplication)
	err := s.db.NewSelect().Model(app).Where("client_id = ?", clientID).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return app, nil
}

func (s *bunStorage) GetOAuthApp(ctx context.Context, id string) (*models.OAuthApplication, error) {
	app := new(models.OAuthApplication)
	err := s.db.NewSelect().Model(app).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return app, nil
}

func (s *bunStorage) CreateOAuthToken(ctx context.Context, token *models.OAuthToken) error {
	if token.ID == "" {
		token.ID = newID()
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}
	stored := *token
	stored.AccessToken = hashBearerSecret(token.AccessToken)
	if token.RefreshToken != "" {
		stored.RefreshToken = hashBearerSecret(token.RefreshToken)
	}
	_, err := s.db.NewInsert().Model(&stored).Exec(ctx)
	return err
}

func hashBearerSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *bunStorage) GetOAuthTokenByAccess(ctx context.Context, accessToken string) (*models.OAuthToken, error) {
	token := new(models.OAuthToken)
	err := s.db.NewSelect().Model(token).
		Where("access_token = ?", hashBearerSecret(accessToken)).
		Where("revoked_at IS NULL").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return token, nil
}

func (s *bunStorage) GetOAuthTokenByRefresh(ctx context.Context, refreshToken string) (*models.OAuthToken, error) {
	token := new(models.OAuthToken)
	err := s.db.NewSelect().Model(token).
		Where("refresh_token = ?", hashBearerSecret(refreshToken)).
		Where("revoked_at IS NULL").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return token, nil
}

func (s *bunStorage) RotateOAuthToken(ctx context.Context, tokenID, refreshToken, newAccessToken, newRefreshToken string) error {
	result, err := s.db.NewUpdate().Model((*models.OAuthToken)(nil)).
		Set("access_token = ?", hashBearerSecret(newAccessToken)).
		Set("refresh_token = ?", hashBearerSecret(newRefreshToken)).
		Where("id = ?", tokenID).
		Where("refresh_token = ?", hashBearerSecret(refreshToken)).
		Where("revoked_at IS NULL").
		Where("authorization_code = ?", false).
		Where("expires_at IS NULL OR expires_at > ?", time.Now()).
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err == nil && rows != 1 {
		return sql.ErrNoRows
	}
	return err
}

// ConsumeOAuthAuthorizationCode atomically marks a one-time authorization code
// as used. The rows-affected check prevents two concurrent exchanges from both
// succeeding with the same code.
func (s *bunStorage) ConsumeOAuthAuthorizationCode(ctx context.Context, code string) (bool, error) {
	now := time.Now()
	result, err := s.db.NewUpdate().Model((*models.OAuthToken)(nil)).
		Set("revoked_at = ?", now).
		Where("access_token = ?", hashBearerSecret(code)).
		Where("authorization_code = ?", true).
		Where("revoked_at IS NULL").
		Where("expires_at IS NULL OR expires_at > ?", now).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *bunStorage) RevokeOAuthToken(ctx context.Context, accessToken string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		token := new(models.OAuthToken)
		if err := tx.NewSelect().Model(token).Where("access_token = ?", hashBearerSecret(accessToken)).Scan(ctx); err == sql.ErrNoRows {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.PushSubscription)(nil)).
			Where("oauth_token_id = ?", token.ID).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Model((*models.OAuthToken)(nil)).
			Set("revoked_at = ?", time.Now()).
			Where("id = ?", token.ID).
			Exec(ctx)
		return err
	})
}

func (s *bunStorage) RevokeOAuthTokensForAccount(ctx context.Context, accountID string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		tokenIDs := tx.NewSelect().Model((*models.OAuthToken)(nil)).Column("id").
			Where("account_id = ?", accountID)
		if _, err := tx.NewDelete().Model((*models.PushSubscription)(nil)).
			Where("oauth_token_id IN (?)", tokenIDs).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Model((*models.OAuthToken)(nil)).
			Set("revoked_at = ?", time.Now()).
			Where("account_id = ?", accountID).
			Where("revoked_at IS NULL").
			Exec(ctx)
		return err
	})
}

// Jobs / Queue

func applyJobDefaults(job *models.Job) {
	if job.ID == "" {
		job.ID = newID()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = time.Now()
	}
	if job.RunAt.IsZero() {
		job.RunAt = time.Now()
	}
	if job.State == "" {
		job.State = "queued"
	}
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = 10
	}
}

func (s *bunStorage) EnqueueJob(ctx context.Context, job *models.Job) error {
	applyJobDefaults(job)
	err := s.retrySQLiteBusy(ctx, func() error {
		_, err := s.db.NewInsert().Model(job).Exec(ctx)
		return err
	})
	if err == nil {
		metrics.IncrementQueueEnqueued()
	}
	return err
}

// EnqueueJobs inserts a batch of jobs in one statement - delivery fan-out
// enqueues one job per inbox, and a row-per-INSERT was a commit per follower
// host on every outbound activity.
func (s *bunStorage) EnqueueJobs(ctx context.Context, jobs []*models.Job) error {
	if len(jobs) == 0 {
		return nil
	}
	for _, job := range jobs {
		applyJobDefaults(job)
	}
	err := s.retrySQLiteBusy(ctx, func() error {
		_, err := s.db.NewInsert().Model(&jobs).Exec(ctx)
		return err
	})
	if err == nil {
		for range jobs {
			metrics.IncrementQueueEnqueued()
		}
	}
	return err
}

// ScheduleSelfDestruct snapshots local identities and known remote inboxes in
// the same transaction that records the run. The singleton insert makes a
// restart (or a second process) observe the original fan-out instead of adding
// duplicate Delete activities.
func (s *bunStorage) ScheduleSelfDestruct(ctx context.Context) (*models.SelfDestructRun, bool, error) {
	const runID = "self-destruct"
	var run models.SelfDestructRun
	created := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		now := time.Now()
		res, err := tx.ExecContext(ctx, `INSERT INTO self_destruct_runs (id, started_at, identity_count, inbox_count, delivery_count) VALUES (?, ?, 0, 0, 0) ON CONFLICT (id) DO NOTHING`, runID, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return tx.NewSelect().Model(&run).Where("id = ?", runID).Scan(ctx)
		}
		created = true

		var identities, remotes []*models.Account
		if err := tx.NewSelect().Model(&identities).Where("domain IS NULL").Scan(ctx); err != nil {
			return err
		}
		if err := tx.NewSelect().Model(&remotes).Where("domain IS NOT NULL").Scan(ctx); err != nil {
			return err
		}
		// A shared inbox wins for its host; otherwise retain the first direct
		// inbox. This is the known-peer snapshot, not a network crawl.
		inboxes := make(map[string]string)
		shared := make(map[string]bool)
		for _, account := range remotes {
			for _, candidate := range []struct {
				url    string
				shared bool
			}{{account.InboxURI, false}, {func() string {
				if account.SharedInboxURI != nil {
					return *account.SharedInboxURI
				}
				return ""
			}(), true}} {
				u, err := url.Parse(candidate.url)
				if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
					continue
				}
				host := u.Hostname()
				if candidate.shared || !shared[host] && inboxes[host] == "" {
					inboxes[host], shared[host] = candidate.url, candidate.shared
				}
			}
		}
		peers := make([]string, 0, len(inboxes))
		for _, inbox := range inboxes {
			peers = append(peers, inbox)
		}
		sort.Strings(peers)
		jobs := make([]*models.Job, 0, len(identities)*len(peers))
		for _, identity := range identities {
			activity, err := json.Marshal(map[string]any{
				"@context": "https://www.w3.org/ns/activitystreams", "id": identity.ActorURI + "#self-destruct",
				"type": "Delete", "actor": identity.ActorURI, "object": identity.ActorURI,
			})
			if err != nil {
				return err
			}
			for _, inbox := range peers {
				payload, err := json.Marshal(struct {
					Inbox    string          `json:"inbox"`
					Activity json.RawMessage `json:"activity"`
				}{inbox, activity})
				if err != nil {
					return err
				}
				jobs = append(jobs, &models.Job{Kind: models.SelfDestructDeliveryJobKind, Payload: string(payload), RunAt: now, MaxAttempts: models.DeliveryMaxAttempts})
			}
		}
		for start := 0; start < len(jobs); start += 500 {
			end := start + 500
			if end > len(jobs) {
				end = len(jobs)
			}
			batch := jobs[start:end]
			for _, job := range batch {
				applyJobDefaults(job)
			}
			if _, err := tx.NewInsert().Model(&batch).Exec(ctx); err != nil {
				return err
			}
		}
		run = models.SelfDestructRun{ID: runID, StartedAt: now, IdentityCount: len(identities), InboxCount: len(peers), DeliveryCount: len(jobs)}
		_, err = tx.NewUpdate().Model(&run).Where("id = ?", runID).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		for range make([]struct{}, run.DeliveryCount) {
			metrics.IncrementQueueEnqueued()
		}
	}
	return &run, created, nil
}

func (s *bunStorage) GetSelfDestructRun(ctx context.Context) (*models.SelfDestructRun, error) {
	run := new(models.SelfDestructRun)
	if err := s.db.NewSelect().Model(run).Where("id = ?", "self-destruct").Scan(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

// ScheduleAccountDeletion atomically disables authentication, snapshots the
// effective remote follower destinations, and creates durable delivery work.
func (s *bunStorage) ScheduleAccountDeletion(ctx context.Context, accountID string, deadline time.Time) (*models.AccountDeletionRun, bool, error) {
	var run models.AccountDeletionRun
	created := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().Model(&run).Where("account_id = ?", accountID).Scan(ctx); err == nil {
			return nil
		} else if err != sql.ErrNoRows {
			return err
		}

		account := new(models.Account)
		if err := tx.NewSelect().Model(account).Where("id = ?", accountID).Scan(ctx); err != nil {
			return err
		}
		if account.Domain != nil || account.IsServiceActor {
			return fmt.Errorf("only local human accounts can be deleted")
		}
		if account.IsAdmin {
			others, err := tx.NewSelect().Model((*models.Account)(nil)).
				Where("domain IS NULL").Where("is_service_actor = ?", false).Where("is_admin = ?", true).
				Where("id <> ?", account.ID).Where("approved = ?", true).
				Where("suspended_at IS NULL").Where("moderation_state NOT IN (?, ?)", "suspended", "disabled").
				Where("deletion_state = ''").Count(ctx)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrAccountDeletionLastAdmin
			}
		}

		var inboxes []string
		if err := tx.NewSelect().Model((*models.Account)(nil)).
			ColumnExpr("DISTINCT COALESCE(NULLIF(account.shared_inbox_uri, ''), account.inbox_uri)").
			Join("JOIN follows ON follows.account_id = account.id").
			Where("follows.target_account_id = ?", account.ID).
			Where("follows.state = ?", models.FollowAccepted).
			Where("account.domain IS NOT NULL").
			Where("COALESCE(NULLIF(account.shared_inbox_uri, ''), account.inbox_uri) <> ''").
			OrderExpr("COALESCE(NULLIF(account.shared_inbox_uri, ''), account.inbox_uri) ASC").
			Scan(ctx, &inboxes); err != nil {
			return err
		}

		now := time.Now().UTC()
		if deadline.Before(now) {
			deadline = now
		}
		run = models.AccountDeletionRun{
			ID: newID(), AccountID: account.ID, ActorURI: account.ActorURI,
			RequestedAt: now, DeadlineAt: deadline, State: "delivering", DestinationCount: len(inboxes),
		}
		if _, err := tx.NewInsert().Model(&run).Exec(ctx); err != nil {
			return err
		}

		jobs := make([]*models.Job, 0, len(inboxes)+1)
		for _, inbox := range inboxes {
			destination := &models.AccountDeletionDestination{ID: newID(), RunID: run.ID, Inbox: inbox}
			if _, err := tx.NewInsert().Model(destination).Exec(ctx); err != nil {
				return err
			}
			payload, _ := json.Marshal(models.AccountDeletionDeliveryPayload{RunID: run.ID, DestinationID: destination.ID})
			jobs = append(jobs, &models.Job{Kind: models.AccountDeletionDeliveryJobKind, Payload: string(payload), RunAt: now, MaxAttempts: models.DeliveryMaxAttempts})
		}
		finalizePayload, _ := json.Marshal(struct {
			RunID string `json:"run_id"`
		}{run.ID})
		finalizeAt := deadline
		if len(inboxes) == 0 {
			finalizeAt = now
		}
		jobs = append(jobs, &models.Job{Kind: models.AccountDeletionFinalizeJobKind, Payload: string(finalizePayload), RunAt: finalizeAt, MaxAttempts: models.DeliveryMaxAttempts})
		for _, job := range jobs {
			applyJobDefaults(job)
		}
		if _, err := tx.NewInsert().Model(&jobs).Exec(ctx); err != nil {
			return err
		}

		if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
			Set("password_hash = ''").Set("moderation_state = 'suspended'").Set("suspended_at = ?", now).
			Set("deletion_state = 'delivering'").Set("deletion_requested_at = ?", now).Set("updated_at = ?", now).
			Where("id = ?", account.ID).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.PushSubscription)(nil)).Where("account_id = ?", account.ID).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.Session)(nil)).Where("account_id = ?", account.ID).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*models.OAuthToken)(nil)).Set("revoked_at = ?", now).
			Where("account_id = ?", account.ID).Where("revoked_at IS NULL").Exec(ctx); err != nil {
			return err
		}
		tombstone := &models.Tombstone{ID: newID(), URI: account.ActorURI, FormerType: "Person", DeletedAt: now, CreatedAt: now}
		if _, err := tx.NewInsert().Model(tombstone).On("CONFLICT (uri) DO NOTHING").Exec(ctx); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		for i := 0; i < run.DestinationCount+1; i++ {
			metrics.IncrementQueueEnqueued()
		}
	}
	return &run, created, nil
}

func (s *bunStorage) GetAccountDeletionRun(ctx context.Context, id string) (*models.AccountDeletionRun, error) {
	run := new(models.AccountDeletionRun)
	if err := s.db.NewSelect().Model(run).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *bunStorage) GetAccountDeletionDelivery(ctx context.Context, runID, destinationID string) (*models.AccountDeletionRun, *models.AccountDeletionDestination, error) {
	run, err := s.GetAccountDeletionRun(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	destination := new(models.AccountDeletionDestination)
	if err := s.db.NewSelect().Model(destination).Where("id = ?", destinationID).Where("run_id = ?", runID).Scan(ctx); err != nil {
		return nil, nil, err
	}
	return run, destination, nil
}

func (s *bunStorage) MarkAccountDeletionDelivered(ctx context.Context, runID, inbox string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		now := time.Now().UTC()
		result, err := tx.NewUpdate().Model((*models.AccountDeletionDestination)(nil)).
			Set("delivered_at = ?", now).Where("run_id = ?", runID).Where("inbox = ?", inbox).
			Where("delivered_at IS NULL").Exec(ctx)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		_, err = tx.NewUpdate().Model((*models.AccountDeletionRun)(nil)).
			Set("delivered_count = delivered_count + 1").Where("id = ?", runID).Exec(ctx)
		return err
	})
}

func (s *bunStorage) CompleteAccountDeletion(ctx context.Context, runID string) error {
	run, err := s.GetAccountDeletionRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.State == "complete" {
		return nil
	}
	if run.DeliveredCount < run.DestinationCount && time.Now().Before(run.DeadlineAt) {
		return ErrAccountDeletionPending
	}
	if err := s.DeleteAccount(ctx, run.AccountID); err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*models.AccountDeletionDestination)(nil)).Where("run_id = ?", runID).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Model((*models.AccountDeletionRun)(nil)).
			Set("state = 'complete'").Set("completed_at = ?", now).Where("id = ?", runID).Exec(ctx)
		return err
	})
}

func (s *bunStorage) CreateAccountExport(ctx context.Context, export *models.AccountExport) error {
	if export.ID == "" {
		export.ID = newID()
	}
	now := time.Now().UTC()
	if export.CreatedAt.IsZero() {
		export.CreatedAt = now
	}
	export.UpdatedAt = now
	if export.State == "" {
		export.State = "queued"
	}
	_, err := s.db.NewInsert().Model(export).Exec(ctx)
	return err
}

func (s *bunStorage) GetAccountExport(ctx context.Context, id, accountID string) (*models.AccountExport, error) {
	export := new(models.AccountExport)
	if err := s.db.NewSelect().Model(export).Where("id = ?", id).Where("account_id = ?", accountID).Scan(ctx); err != nil {
		return nil, err
	}
	return export, nil
}

func (s *bunStorage) GetLatestAccountExport(ctx context.Context, accountID string) (*models.AccountExport, error) {
	export := new(models.AccountExport)
	if err := s.db.NewSelect().Model(export).Where("account_id = ?", accountID).Order("created_at DESC").Limit(1).Scan(ctx); err != nil {
		return nil, err
	}
	return export, nil
}

func (s *bunStorage) ListAccountExports(ctx context.Context, accountID string) ([]*models.AccountExport, error) {
	var exports []*models.AccountExport
	err := s.db.NewSelect().Model(&exports).Where("account_id = ?", accountID).Order("created_at DESC").Scan(ctx)
	return exports, err
}

// attachBoostURIs fills in the transient Status.BoostOfURI field (not a
// column) with a single batch lookup, so exports can remap boosts by URI on
// import even when the boosted status isn't in this account's own export.
func attachBoostURIs(ctx context.Context, db *bun.DB, statuses []*models.Status) error {
	ids := make([]string, 0, len(statuses))
	seen := make(map[string]bool)
	for _, status := range statuses {
		if status.BoostOfID != nil && *status.BoostOfID != "" && !seen[*status.BoostOfID] {
			seen[*status.BoostOfID] = true
			ids = append(ids, *status.BoostOfID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var targets []*models.Status
	if err := db.NewSelect().Model(&targets).Column("id", "uri").Where("id IN (?)", bun.In(ids)).Scan(ctx); err != nil {
		return err
	}
	uriByID := make(map[string]string, len(targets))
	for _, target := range targets {
		uriByID[target.ID] = target.URI
	}
	for _, status := range statuses {
		if status.BoostOfID == nil {
			continue
		}
		if uri, ok := uriByID[*status.BoostOfID]; ok {
			status.BoostOfURI = &uri
		}
	}
	return nil
}

func exportAccountHandle(account *models.Account) string {
	if account == nil {
		return ""
	}
	if account.Domain != nil && *account.Domain != "" {
		return account.Username + "@" + *account.Domain
	}
	return account.Username
}

func (s *bunStorage) BuildAccountExportSnapshot(ctx context.Context, accountID string) (*models.AccountExportSnapshot, error) {
	account, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	snapshot := &models.AccountExportSnapshot{
		Profile: models.AccountExportProfile{
			Username: account.Username, DisplayName: account.DisplayName, Note: account.Note,
			Email: account.Email, Locale: account.Locale, ActorURI: account.ActorURI,
			Avatar: account.Avatar, Header: account.Header, CreatedAt: account.CreatedAt,
		},
		Preferences: map[string]any{
			"default_post_language": account.DefaultPostLanguage, "default_post_markdown": account.DefaultPostMarkdown,
			"default_post_visibility": account.DefaultPostVisibility, "hide_timeline_composer": account.HideTimelineComposer, "locale": account.Locale,
			"hide_replies": account.HideReplies, "hide_boosts": account.HideBoosts, "data_saver": account.DataSaver,
			"hide_posts_without_alt": account.HidePostsWithoutAlt,
			"hide_favourite_counts":  account.HideFavouriteCounts,
			"hide_boost_counts":      account.HideBoostCounts,
			"hide_follower_counts":   account.HideFollowerCounts,
			"hide_followers":         account.HideFollowers, "hide_following": account.HideFollowing,
			"group_notifications": account.GroupNotifications, "manually_approves_followers": account.ManuallyApprovesFollowers,
			"quote_policy": account.QuotePolicy, "discovery": account.Discovery,
			"rss_feed_enabled":     account.RSSFeedEnabled,
			"blog_replies_enabled": account.BlogRepliesEnabled,
			"theme":                account.Theme,
			"appearance":           account.Appearance,
			"custom_css":           account.CustomCSS,
			"blog_theme":           account.BlogTheme,
			"blog_custom_css":      account.BlogCustomCSS,
		},
		Following: []string{}, Followers: []string{}, Lists: []models.AccountExportList{}, Blocks: []string{},
		Mutes: []models.AccountExportMute{}, DomainBlocks: []string{}, Bookmarks: []string{}, FollowedHashtags: []string{},
	}
	if err := s.db.NewSelect().Model(&snapshot.Statuses).Where("account_id = ?", accountID).Order("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := attachBoostURIs(ctx, s.db, snapshot.Statuses); err != nil {
		return nil, err
	}

	var following, followers, blocked []*models.Account
	if err := s.db.NewSelect().Model(&following).Join("JOIN follows ON follows.target_account_id = account.id").
		Where("follows.account_id = ?", accountID).Where("follows.state = ?", models.FollowAccepted).Order("follows.created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := s.db.NewSelect().Model(&followers).Join("JOIN follows ON follows.account_id = account.id").
		Where("follows.target_account_id = ?", accountID).Where("follows.state = ?", models.FollowAccepted).Order("follows.created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := s.db.NewSelect().Model(&blocked).Join("JOIN blocks ON blocks.target_account_id = account.id").
		Where("blocks.account_id = ?", accountID).Order("blocks.created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, a := range following {
		snapshot.Following = append(snapshot.Following, exportAccountHandle(a))
	}
	for _, a := range followers {
		snapshot.Followers = append(snapshot.Followers, exportAccountHandle(a))
	}
	for _, a := range blocked {
		snapshot.Blocks = append(snapshot.Blocks, exportAccountHandle(a))
	}

	var mutes []*models.Mute
	if err := s.db.NewSelect().Model(&mutes).Where("account_id = ?", accountID).Order("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, mute := range mutes {
		target, err := s.GetAccount(ctx, mute.TargetAccountID)
		if err != nil {
			return nil, err
		}
		snapshot.Mutes = append(snapshot.Mutes, models.AccountExportMute{Account: exportAccountHandle(target), HideNotifications: mute.HideNotifications, DurationMinutes: mute.Duration})
	}

	lists, err := s.ListLists(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for _, list := range lists {
		members, err := s.ListAccountsForList(ctx, list.ID, nil, nil, 0)
		if err != nil {
			return nil, err
		}
		exported := models.AccountExportList{Title: list.Title, RepliesPolicy: list.RepliesPolicy, Exclusive: list.Exclusive, Members: []string{}}
		for _, member := range members {
			exported.Members = append(exported.Members, exportAccountHandle(member))
		}
		snapshot.Lists = append(snapshot.Lists, exported)
	}

	if snapshot.DomainBlocks, err = s.ListAccountDomainBlocks(ctx, accountID); err != nil {
		return nil, err
	}
	var bookmarkURIs []string
	if err := s.db.NewSelect().Model((*models.Bookmark)(nil)).ColumnExpr("status.uri").
		Join("JOIN statuses AS status ON status.id = bookmark.status_id").Where("bookmark.account_id = ?", accountID).
		Order("bookmark.created_at ASC").Scan(ctx, &bookmarkURIs); err != nil {
		return nil, err
	}
	snapshot.Bookmarks = bookmarkURIs
	snapshot.Media, err = s.GetMediaForAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	tags, err := s.ListFollowedTags(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for _, tag := range tags {
		snapshot.FollowedHashtags = append(snapshot.FollowedHashtags, tag.Name)
	}
	return snapshot, nil
}

func (s *bunStorage) CompleteAccountExport(ctx context.Context, id, storageKey string, size int64, expiresAt time.Time) error {
	_, err := s.db.NewUpdate().Model((*models.AccountExport)(nil)).Set("state = 'ready'").Set("storage_key = ?", storageKey).
		Set("size = ?", size).Set("expires_at = ?", expiresAt).Set("last_error = ''").Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).Where("state IN ('queued', 'ready')").Exec(ctx)
	return err
}

func (s *bunStorage) RecordAccountExportError(ctx context.Context, id, message string) error {
	_, err := s.db.NewUpdate().Model((*models.AccountExport)(nil)).Set("last_error = ?", message).Set("updated_at = ?", time.Now().UTC()).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) DeleteAccountExport(ctx context.Context, id, accountID string) error {
	_, err := s.db.NewDelete().Model((*models.AccountExport)(nil)).Where("id = ?", id).Where("account_id = ?", accountID).Exec(ctx)
	return err
}

func (s *bunStorage) SetAccountExportDownloadToken(ctx context.Context, id, accountID, tokenHash string) error {
	result, err := s.db.NewUpdate().Model((*models.AccountExport)(nil)).Set("token_hash = ?", tokenHash).Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).Where("account_id = ?", accountID).Where("state = 'ready'").Where("downloaded_at IS NULL").
		Where("expires_at > ?", time.Now().UTC()).Exec(ctx)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *bunStorage) ConsumeAccountExportDownload(ctx context.Context, id, accountID, tokenHash string, now time.Time) (*models.AccountExport, error) {
	result, err := s.db.NewUpdate().Model((*models.AccountExport)(nil)).Set("downloaded_at = ?", now).Set("token_hash = ''").Set("updated_at = ?", now).
		Where("id = ?", id).Where("account_id = ?", accountID).Where("state = 'ready'").Where("downloaded_at IS NULL").
		Where("token_hash = ?", tokenHash).Where("expires_at > ?", now).Exec(ctx)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, sql.ErrNoRows
	}
	return s.GetAccountExport(ctx, id, accountID)
}

func (s *bunStorage) ExpireAccountExport(ctx context.Context, id string) error {
	_, err := s.db.NewUpdate().Model((*models.AccountExport)(nil)).Set("state = 'expired'").Set("storage_key = ''").
		Set("token_hash = ''").Set("updated_at = ?", time.Now().UTC()).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) CreateAccountImport(ctx context.Context, imp *models.AccountImport) error {
	if imp.ID == "" {
		imp.ID = newID()
	}
	now := time.Now().UTC()
	if imp.CreatedAt.IsZero() {
		imp.CreatedAt = now
	}
	imp.UpdatedAt = now
	if imp.State == "" {
		imp.State = "queued"
	}
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := checkAccountImportTx(ctx, tx, imp.AccountID); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(imp).Exec(ctx)
		return err
	})
}

func (s *bunStorage) GetAccountImport(ctx context.Context, id, accountID string) (*models.AccountImport, error) {
	imp := new(models.AccountImport)
	if err := s.db.NewSelect().Model(imp).Where("id = ?", id).Where("account_id = ?", accountID).Scan(ctx); err != nil {
		return nil, err
	}
	return imp, nil
}

func (s *bunStorage) GetLatestAccountImport(ctx context.Context, accountID string) (*models.AccountImport, error) {
	imp := new(models.AccountImport)
	if err := s.db.NewSelect().Model(imp).Where("account_id = ?", accountID).Order("created_at DESC").Limit(1).Scan(ctx); err != nil {
		return nil, err
	}
	return imp, nil
}

func (s *bunStorage) ListAccountImports(ctx context.Context, accountID string) ([]*models.AccountImport, error) {
	var imports []*models.AccountImport
	err := s.db.NewSelect().Model(&imports).Where("account_id = ?", accountID).Order("created_at DESC").Scan(ctx)
	return imports, err
}

func (s *bunStorage) StartAccountImport(ctx context.Context, id string) error {
	_, err := s.db.NewUpdate().Model((*models.AccountImport)(nil)).Set("state = 'running'").Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) CompleteAccountImport(ctx context.Context, id, resultJSON string) error {
	_, err := s.db.NewUpdate().Model((*models.AccountImport)(nil)).Set("state = 'complete'").Set("result_json = ?", resultJSON).
		Set("last_error = ''").Set("updated_at = ?", time.Now().UTC()).Where("id = ?", id).Exec(ctx)
	return err
}

// RecordAccountImportError only records the error message; it deliberately
// leaves State untouched (mirrors RecordAccountExportError) so a queue retry
// of the same job kind picks the row back up instead of skipping it.
func (s *bunStorage) RecordAccountImportError(ctx context.Context, id, message string) error {
	_, err := s.db.NewUpdate().Model((*models.AccountImport)(nil)).Set("last_error = ?", message).Set("updated_at = ?", time.Now().UTC()).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) DeleteAccountImport(ctx context.Context, id, accountID string) error {
	_, err := s.db.NewDelete().Model((*models.AccountImport)(nil)).Where("id = ?", id).Where("account_id = ?", accountID).Exec(ctx)
	return err
}

func (s *bunStorage) DequeueJob(ctx context.Context, kinds []string) (*models.Job, error) {
	// Select and claim in one UPDATE ... RETURNING round trip (SQLite >= 3.35
	// and PostgreSQL both support it). The repeated state predicate on the
	// outer UPDATE keeps the claim atomic: if another worker claims the row
	// between the subquery and the write, zero rows update and this returns
	// sql.ErrNoRows, which callers already treat as "no job available".
	now := time.Now()

	sub := s.db.NewSelect().Model((*models.Job)(nil)).
		Column("id").
		Where("state = ?", "queued").
		Where("run_at <= ?", now).
		OrderExpr("priority DESC").
		OrderExpr("run_at ASC").
		Limit(1)
	if len(kinds) > 0 {
		sub = sub.Where("kind IN (?)", bun.In(kinds))
	}

	job := new(models.Job)
	err := s.retrySQLiteBusy(ctx, func() error {
		return s.db.NewUpdate().Model(job).
			Set("state = ?", "running").
			Set("taken_at = ?", now).
			Set("attempts = attempts + 1").
			Set("updated_at = ?", now).
			Where("id = (?)", sub).
			Where("state = ?", "queued").
			Returning("*").
			Scan(ctx)
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (s *bunStorage) CompleteJob(ctx context.Context, id string) error {
	var res sql.Result
	err := s.retrySQLiteBusy(ctx, func() error {
		var err error
		res, err = s.db.NewDelete().Model((*models.Job)(nil)).
			Where("id = ?", id).
			Exec(ctx)
		return err
	})
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			metrics.IncrementQueueCompleted()
		}
	}
	return err
}

func (s *bunStorage) FailJob(ctx context.Context, id string, err error) error {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	var res sql.Result
	notificationCreated := false
	err2 := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		notificationCreated = false
		var updateErr error
		res, updateErr = tx.NewUpdate().Model((*models.Job)(nil)).
			Set("state = ?", "failed").
			Set("error = ?", errMsg).
			Set("updated_at = ?", time.Now()).
			Where("id = ?", id).
			Exec(ctx)
		if updateErr != nil {
			return updateErr
		}
		// CSV import jobs use their import ID, keeping terminal failures visible.
		imp := new(models.AccountImport)
		updateErr = tx.NewUpdate().Model(imp).
			Set("state = 'failed'").Set("last_error = ?", errMsg).Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).Where("state IN ('queued', 'running')").
			Where("EXISTS (SELECT 1 FROM jobs WHERE id = ? AND kind = ?)", id, models.FollowImportJobKind).
			Returning("*").Scan(ctx)
		if errors.Is(updateErr, sql.ErrNoRows) {
			return nil
		}
		if updateErr != nil {
			return updateErr
		}
		notificationCreated, updateErr = createFollowImportNotification(ctx, tx, imp)
		return updateErr
	})
	if err2 == nil {
		if notificationCreated {
			metrics.IncrementQueueEnqueued()
		}
		if n, _ := res.RowsAffected(); n > 0 {
			metrics.IncrementQueueFailed()
		}
	}
	return err2
}

func (s *bunStorage) RequeueJob(ctx context.Context, id string, runAt time.Time) error {
	return s.retrySQLiteBusy(ctx, func() error {
		_, err := s.db.NewUpdate().Model((*models.Job)(nil)).
			Set("state = ?", "queued").
			Set("run_at = ?", runAt).
			Set("taken_at = NULL").
			Set("updated_at = ?", time.Now()).
			Where("id = ?", id).
			Where("state = ?", "running").
			Exec(ctx)
		return err
	})
}

func (s *bunStorage) YieldJob(ctx context.Context, id string, runAt time.Time) error {
	return s.retrySQLiteBusy(ctx, func() error {
		_, err := s.db.NewUpdate().Model((*models.Job)(nil)).
			Set("state = ?", "queued").
			Set("run_at = ?", runAt).
			Set("taken_at = NULL").
			Set("attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END").
			Set("updated_at = ?", time.Now()).
			Where("id = ?", id).
			Where("state = ?", "running").
			Exec(ctx)
		return err
	})
}

func (s *bunStorage) CountPendingJobs(ctx context.Context) (int, error) {
	return s.db.NewSelect().Model((*models.Job)(nil)).
		Where("state IN ('queued', 'running')").
		Count(ctx)
}

func (s *bunStorage) GetQueueStats(ctx context.Context, now, staleBefore time.Time) (*models.QueueStats, error) {
	stats := &models.QueueStats{}

	// One conditional-aggregation pass instead of six counts plus two extrema
	// queries: this runs on every admin page render, and each extra query is
	// a table scan competing with the single SQLite writer. SUM(CASE ...) is
	// used instead of FILTER for older-SQLite portability; COALESCE covers
	// the NULL an aggregate returns over an empty set.
	type totals struct {
		Ready         int        `bun:"ready"`
		Scheduled     int        `bun:"scheduled"`
		Running       int        `bun:"running"`
		Failed        int        `bun:"failed"`
		Retrying      int        `bun:"retrying"`
		StaleRunning  int        `bun:"stale_running"`
		OldestReady   *time.Time `bun:"oldest_ready"`
		MaxAttempts   int        `bun:"max_attempts"`
		OldestRunning *time.Time `bun:"oldest_running"`
	}
	var t totals
	if err := s.db.NewSelect().Model((*models.Job)(nil)).
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'queued' AND run_at <= ? THEN 1 ELSE 0 END), 0) AS ready", now).
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'queued' AND run_at > ? THEN 1 ELSE 0 END), 0) AS scheduled", now).
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'running' THEN 1 ELSE 0 END), 0) AS running").
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END), 0) AS failed").
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'queued' AND attempts > 0 THEN 1 ELSE 0 END), 0) AS retrying").
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'running' AND taken_at IS NOT NULL AND taken_at < ? THEN 1 ELSE 0 END), 0) AS stale_running", staleBefore).
		ColumnExpr("MIN(CASE WHEN state = 'queued' AND run_at <= ? THEN run_at END) AS oldest_ready", now).
		ColumnExpr("COALESCE(MAX(CASE WHEN state = 'queued' AND run_at <= ? THEN attempts END), 0) AS max_attempts", now).
		ColumnExpr("MIN(CASE WHEN state = 'running' THEN taken_at END) AS oldest_running").
		Where("state IN (?)", bun.In([]string{"queued", "running", "failed"})).
		Scan(ctx, &t); err != nil {
		return nil, err
	}
	stats.Ready, stats.Scheduled, stats.Running = t.Ready, t.Scheduled, t.Running
	stats.Failed, stats.Retrying, stats.StaleRunning = t.Failed, t.Retrying, t.StaleRunning
	stats.OldestReadyAt, stats.MaxAttemptsObserved = t.OldestReady, t.MaxAttempts
	stats.OldestRunningAt = t.OldestRunning

	// Per-kind breakdown in one grouped pass (the queued split no longer
	// needs a follow-up count per kind).
	type grouped struct {
		Kind      string `bun:"kind"`
		Ready     int    `bun:"ready"`
		Scheduled int    `bun:"scheduled"`
		Running   int    `bun:"running"`
		Failed    int    `bun:"failed"`
	}
	var rows []grouped
	if err := s.db.NewSelect().Model((*models.Job)(nil)).Column("kind").
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'queued' AND run_at <= ? THEN 1 ELSE 0 END), 0) AS ready", now).
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'queued' AND run_at > ? THEN 1 ELSE 0 END), 0) AS scheduled", now).
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'running' THEN 1 ELSE 0 END), 0) AS running").
		ColumnExpr("COALESCE(SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END), 0) AS failed").
		Where("state IN (?)", bun.In([]string{"queued", "running", "failed"})).
		Group("kind").Order("kind ASC").Scan(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		stats.ByKind = append(stats.ByKind, models.QueueKindStats{Kind: row.Kind,
			Ready: row.Ready, Scheduled: row.Scheduled, Running: row.Running, Failed: row.Failed})
	}
	return stats, nil
}

func (s *bunStorage) ListQueueProblemJobs(ctx context.Context, limit int) ([]models.QueueProblemJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var jobs []models.Job
	err := s.db.NewSelect().Model(&jobs).
		Where("state = ? OR (state = ? AND attempts > 0)", "failed", "queued").
		Order("updated_at DESC").Limit(limit).Scan(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]models.QueueProblemJob, 0, len(jobs))
	for _, job := range jobs {
		errText := job.Error
		if len(errText) > 240 {
			errText = errText[:240] + "…"
		}
		result = append(result, models.QueueProblemJob{ID: job.ID, Kind: job.Kind, State: job.State,
			Error: errText, Attempts: job.Attempts, MaxAttempts: job.MaxAttempts,
			CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt, RunAt: job.RunAt, TakenAt: job.TakenAt})
	}
	return result, nil
}

// ListActiveQueueJobs returns a bounded operational view source. Payloads are
// returned only to trusted server code, which must summarize them before they
// reach a template.
func (s *bunStorage) ListActiveQueueJobs(ctx context.Context, limit int) ([]models.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var jobs []models.Job
	err := s.db.NewSelect().Model(&jobs).
		Where("state IN (?)", bun.In([]string{"running", "queued"})).
		OrderExpr("CASE WHEN state = 'running' THEN 0 ELSE 1 END").
		Order("kind ASC").OrderExpr("priority DESC").Order("run_at ASC").
		Limit(limit).Scan(ctx)
	return jobs, err
}

// DeleteQueueProblemJob deletes only terminal failures or queued retries.
// Running jobs and fresh queued jobs are protected.
func (s *bunStorage) DeleteQueueProblemJob(ctx context.Context, id string) (bool, error) {
	res, err := s.db.NewDelete().Model((*models.Job)(nil)).
		Where("id = ?", id).
		Where("state = ? OR (state = ? AND attempts > 0)", "failed", "queued").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	count, err := res.RowsAffected()
	return count > 0, err
}

// RecoverStaleJobs returns jobs abandoned by a crashed worker to the queue.
func (s *bunStorage) RecoverStaleJobs(ctx context.Context, takenBefore time.Time) (int, error) {
	res, err := s.db.NewUpdate().Model((*models.Job)(nil)).
		Set("state = ?", "queued").
		Set("taken_at = NULL").
		Set("run_at = ?", time.Now()).
		Set("updated_at = ?", time.Now()).
		Where("state = ?", "running").
		Where("taken_at IS NOT NULL").
		Where("taken_at < ?", takenBefore).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *bunStorage) CreateJobPayload(ctx context.Context, body string) (string, error) {
	payload := &models.JobPayload{ID: newID(), Body: body, CreatedAt: time.Now()}
	if _, err := s.db.NewInsert().Model(payload).Exec(ctx); err != nil {
		return "", err
	}
	return payload.ID, nil
}

func (s *bunStorage) GetJobPayload(ctx context.Context, id string) (string, error) {
	payload := new(models.JobPayload)
	if err := s.db.NewSelect().Model(payload).Where("id = ?", id).Scan(ctx); err != nil {
		return "", err
	}
	return payload.Body, nil
}

func (s *bunStorage) DeleteJobPayloadsBefore(ctx context.Context, createdBefore time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM job_payloads
		WHERE created_at < ?
		  AND NOT EXISTS (
			SELECT 1 FROM jobs
			WHERE jobs.kind = 'delivery'
			  AND jobs.state IN ('queued', 'running')
			  AND jobs.payload LIKE '%"payload_ref":"' || job_payloads.id || '"%'
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM account_imports ai
			WHERE ai.state IN ('queued', 'running', 'failed')
			  AND job_payloads.id LIKE ai.id || ':%'
		  )`, createdBefore)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteTerminalJobsBefore deletes bounded batches of old completed/failed jobs.
func (s *bunStorage) DeleteTerminalJobsBefore(ctx context.Context, updatedBefore time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 1000
	}
	var ids []string
	if err := s.db.NewSelect().Model((*models.Job)(nil)).Column("id").
		Where("state IN (?)", bun.In([]string{"completed", "failed"})).
		Where("updated_at < ?", updatedBefore).
		Order("updated_at ASC").Limit(limit).Scan(ctx, &ids); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := s.db.NewDelete().Model((*models.Job)(nil)).Where("id IN (?)", bun.In(ids)).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Bookmarks

func (s *bunStorage) CreateBookmark(ctx context.Context, bookmark *models.Bookmark) error {
	if bookmark.ID == "" {
		bookmark.ID = newID()
	}
	if bookmark.CreatedAt.IsZero() {
		bookmark.CreatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(bookmark).Exec(ctx)
	return err
}

func (s *bunStorage) DeleteBookmark(ctx context.Context, accountID, statusID string) error {
	_, err := s.db.NewDelete().Model((*models.Bookmark)(nil)).
		Where("account_id = ?", accountID).
		Where("status_id = ?", statusID).
		Exec(ctx)
	return err
}

func (s *bunStorage) GetBookmark(ctx context.Context, accountID, statusID string) (*models.Bookmark, error) {
	bookmark := new(models.Bookmark)
	err := s.db.NewSelect().Model(bookmark).
		Where("account_id = ?", accountID).
		Where("status_id = ?", statusID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return bookmark, nil
}

func (s *bunStorage) ListBookmarks(ctx context.Context, accountID string, maxID *string, limit int) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}
	var statuses []*models.Status
	q := s.db.NewSelect().Model(&statuses).
		Join("JOIN bookmarks ON bookmarks.status_id = status.id").
		Where("bookmarks.account_id = ?", accountID).
		Order("bookmarks.created_at DESC").
		Order("bookmarks.id DESC").
		Limit(limit)
	if maxID != nil && *maxID != "" {
		// maxID is a status ID (per the Mastodon Link header / web cursor); resolve
		// the corresponding bookmark row so we page by bookmark recency, not by the
		// random UUID of the bookmark or status.
		cursor := new(models.Bookmark)
		err := s.db.NewSelect().Model(cursor).
			Where("account_id = ?", accountID).
			Where("status_id = ?", *maxID).
			Limit(1).Scan(ctx)
		if err != nil {
			// Unknown cursor: return an empty page rather than looping from the top.
			return []*models.Status{}, nil
		}
		q = q.Where("(bookmarks.created_at < ? OR (bookmarks.created_at = ? AND bookmarks.id < ?))",
			cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	err := q.Scan(ctx)
	return statuses, err
}

// GetDirectTimeline returns direct-visibility statuses the account takes part
// in, either as the author or as a mentioned recipient, newest first.
func (s *bunStorage) GetDirectTimeline(ctx context.Context, accountID string, maxID, minID, sinceID *string, limit int) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}

	mentioned := s.db.NewSelect().Model((*models.Mention)(nil)).
		Column("status_id").
		Where("account_id = ?", accountID)

	q := s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.context_only = FALSE").
		Where("status.visibility = ?", string(models.VisibilityDirect)).
		Where("(status.account_id = ? OR status.id IN (?))", accountID, mentioned).
		Order("status.created_at DESC").
		Limit(limit)

	q, err := s.applyTimelineModeration(ctx, q, accountID, false)
	if err != nil {
		return nil, err
	}

	q = s.applyStatusPagination(ctx, q, maxID, minID, sinceID)

	var statuses []*models.Status
	err = q.Scan(ctx, &statuses)
	return statuses, err
}

// Pinned Statuses

func (s *bunStorage) GetPinnedStatuses(ctx context.Context, accountID string, limit int) ([]*models.Status, error) {
	if limit <= 0 {
		limit = 20
	}
	var statuses []*models.Status
	err := s.db.NewSelect().Model((*models.Status)(nil)).
		Where("status.account_id = ?", accountID).
		Where("status.pinned = ?", true).
		Where("status.boost_of_id IS NULL").
		Where("status.visibility IN ('public', 'unlisted', 'local', 'local_unlisted')").
		Order("status.created_at DESC").
		Limit(limit).
		Scan(ctx, &statuses)
	return statuses, err
}

func (s *bunStorage) PinStatus(ctx context.Context, statusID string) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("pinned = ?", true).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", statusID).
		Exec(ctx)
	return err
}

func (s *bunStorage) UnpinStatus(ctx context.Context, statusID string) error {
	_, err := s.db.NewUpdate().Model((*models.Status)(nil)).
		Set("pinned = ?", false).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", statusID).
		Exec(ctx)
	return err
}

// Status Tags

func (s *bunStorage) CreateStatusTags(ctx context.Context, statusTags []*models.StatusTag) error {
	if len(statusTags) == 0 {
		return nil
	}
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewInsert().Model(&statusTags).
			On("CONFLICT (status_id, tag_id) DO NOTHING").Exec(ctx)
		if err != nil {
			return err
		}
		// Only bump usage counts for rows that were actually inserted, so callers
		// that re-run this for an existing status (e.g. thread backfill) don't
		// over-count.
		if inserted, _ := res.RowsAffected(); inserted == 0 {
			return nil
		}
		tagIDs := make([]string, 0, len(statusTags))
		for _, st := range statusTags {
			tagIDs = append(tagIDs, st.TagID)
		}
		return recountTagCounts(ctx, tx, tagIDs)
	})
	if err != nil {
		return err
	}
	// Tags attached after CreateStatus can add tag-followers to the audience;
	// re-run fan-out per distinct status (idempotent).
	seen := make(map[string]struct{}, len(statusTags))
	for _, st := range statusTags {
		if st == nil {
			continue
		}
		if _, ok := seen[st.StatusID]; ok {
			continue
		}
		seen[st.StatusID] = struct{}{}
		s.fanOutStatusIDToHomeFeeds(ctx, st.StatusID)
	}
	return nil
}

func (s *bunStorage) DeleteStatusTags(ctx context.Context, statusID string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var tagIDs []string
		if err := tx.NewSelect().Model((*models.StatusTag)(nil)).
			Column("tag_id").
			Where("status_id = ?", statusID).
			Scan(ctx, &tagIDs); err != nil && err != sql.ErrNoRows {
			return err
		}
		if len(tagIDs) == 0 {
			return nil
		}
		if _, err := tx.NewDelete().Model((*models.StatusTag)(nil)).
			Where("status_id = ?", statusID).
			Exec(ctx); err != nil {
			return err
		}
		return recountTagCounts(ctx, tx, tagIDs)
	})
}

func (s *bunStorage) SyncStatusTags(ctx context.Context, statusID string, tags []*models.Tag) (bool, error) {
	changed := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		changed, err = syncStatusTags(ctx, tx, statusID, tags)
		return err
	})
	if err == nil && changed {
		s.fanOutStatusIDToHomeFeeds(ctx, statusID)
	}
	return changed, err
}

func syncStatusTags(ctx context.Context, tx bun.IDB, statusID string, tags []*models.Tag) (bool, error) {
	// Desired tag set (deduplicated).
	want := make(map[string]bool, len(tags))
	for _, t := range tags {
		if t.ID != "" {
			want[t.ID] = true
		}
	}
	var current []string
	if err := tx.NewSelect().Model((*models.StatusTag)(nil)).
		Column("tag_id").
		Where("status_id = ?", statusID).
		Scan(ctx, &current); err != nil && err != sql.ErrNoRows {
		return false, err
	}
	have := make(map[string]bool, len(current))
	for _, id := range current {
		have[id] = true
	}

	var removed, added []string
	for id := range have {
		if !want[id] {
			removed = append(removed, id)
		}
	}
	for id := range want {
		if !have[id] {
			added = append(added, id)
		}
	}
	changed := len(removed) > 0 || len(added) > 0

	if len(removed) > 0 {
		if _, err := tx.NewDelete().Model((*models.StatusTag)(nil)).
			Where("status_id = ?", statusID).
			Where("tag_id IN (?)", bun.In(removed)).
			Exec(ctx); err != nil {
			return false, err
		}
	}
	if len(added) > 0 {
		pivots := make([]models.StatusTag, 0, len(added))
		for _, id := range added {
			pivots = append(pivots, models.StatusTag{StatusID: statusID, TagID: id})
		}
		if _, err := tx.NewInsert().Model(&pivots).
			On("CONFLICT (status_id, tag_id) DO NOTHING").Exec(ctx); err != nil {
			return false, err
		}
	}
	return changed, recountTagCounts(ctx, tx, append(removed, added...))
}

func (s *bunStorage) SyncStatusMentions(ctx context.Context, statusID string, mentions []*models.Mention) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error { return syncStatusMentions(ctx, tx, statusID, mentions) })
}

func syncStatusMentions(ctx context.Context, tx bun.IDB, statusID string, mentions []*models.Mention) error {
	if _, err := tx.NewDelete().Model((*models.Mention)(nil)).
		Where("status_id = ?", statusID).Exec(ctx); err != nil {
		return err
	}
	if len(mentions) == 0 {
		return nil
	}
	rows := make([]models.Mention, 0, len(mentions))
	for _, m := range mentions {
		mm := *m
		if mm.ID == "" {
			mm.ID = newID()
		}
		mm.StatusID = statusID
		rows = append(rows, mm)
	}
	_, err := tx.NewInsert().Model(&rows).Exec(ctx)
	return err
}

// Lists

func (s *bunStorage) CreateList(ctx context.Context, list *models.List) error {
	if list.ID == "" {
		list.ID = newID()
	}
	if list.CreatedAt.IsZero() {
		list.CreatedAt = time.Now()
	}
	if list.UpdatedAt.IsZero() {
		list.UpdatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(list).Exec(ctx)
	return err
}

func (s *bunStorage) GetList(ctx context.Context, id string) (*models.List, error) {
	list := new(models.List)
	err := s.db.NewSelect().Model(list).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (s *bunStorage) UpdateList(ctx context.Context, list *models.List) error {
	list.UpdatedAt = time.Now()
	_, err := s.db.NewUpdate().Model(list).Where("id = ?", list.ID).Exec(ctx)
	if err == nil {
		// An exclusive flip changes which authors the owner's home hides.
		s.rels.invalidateViewers(list.AccountID)
	}
	return err
}

func (s *bunStorage) DeleteList(ctx context.Context, id string) error {
	owner := s.listOwner(ctx, id)
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*models.ListAccount)(nil)).Where("list_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*models.List)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	})
	if err == nil && owner != "" {
		s.rels.invalidateViewers(owner)
	}
	return err
}

// listOwner returns the owning account ID of a list, or "" when unknown.
func (s *bunStorage) listOwner(ctx context.Context, listID string) string {
	var owner string
	if err := s.db.NewSelect().Model((*models.List)(nil)).
		Column("account_id").
		Where("id = ?", listID).
		Scan(ctx, &owner); err != nil {
		return ""
	}
	return owner
}

func (s *bunStorage) ListLists(ctx context.Context, accountID string) ([]*models.List, error) {
	var lists []*models.List
	err := s.db.NewSelect().Model(&lists).
		Where("account_id = ?", accountID).
		Order("title ASC").
		Scan(ctx)
	return lists, err
}

func (s *bunStorage) ListAccountsForList(ctx context.Context, listID string, maxID, sinceID *string, limit int) ([]*models.Account, error) {
	var accounts []*models.Account
	q := s.db.NewSelect().
		Model(&accounts).
		Join("JOIN list_accounts ON list_accounts.account_id = account.id").
		Where("list_accounts.list_id = ?", listID).
		OrderExpr("account.id DESC")
	if maxID != nil && *maxID != "" {
		q = q.Where("account.id < ?", *maxID)
	}
	if sinceID != nil && *sinceID != "" {
		q = q.Where("account.id > ?", *sinceID)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Scan(ctx)
	return accounts, err
}

func (s *bunStorage) AddAccountToList(ctx context.Context, listID, accountID string) error {
	la := &models.ListAccount{
		ListID:    listID,
		AccountID: accountID,
	}
	_, err := s.db.NewInsert().Model(la).
		On("CONFLICT DO NOTHING").
		Exec(ctx)
	if err == nil {
		if owner := s.listOwner(ctx, listID); owner != "" {
			s.rels.invalidateViewers(owner) // exclusive-member set may change
		}
	}
	return err
}

func (s *bunStorage) RemoveAccountFromList(ctx context.Context, listID, accountID string) error {
	_, err := s.db.NewDelete().Model((*models.ListAccount)(nil)).
		Where("list_id = ?", listID).
		Where("account_id = ?", accountID).
		Exec(ctx)
	if err == nil {
		if owner := s.listOwner(ctx, listID); owner != "" {
			s.rels.invalidateViewers(owner)
		}
	}
	return err
}

// RemoveAccountFromAllLists removes memberID from every list ownerID owns.
// Called from the unfollow path: Mastodon drops list memberships on unfollow,
// and the derived list timeline requires the member-implies-followed invariant.
func (s *bunStorage) RemoveAccountFromAllLists(ctx context.Context, ownerID, memberID string) (bool, error) {
	res, err := s.db.NewDelete().Model((*models.ListAccount)(nil)).
		Where("account_id = ?", memberID).
		Where("list_id IN (SELECT id FROM lists WHERE account_id = ?)", ownerID).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	if rows > 0 {
		s.rels.invalidateViewers(ownerID)
	}
	return rows > 0, nil
}

// ListListsContainingAccount returns the lists owned by ownerID that contain
// memberID (GET /api/v1/accounts/:id/lists).
func (s *bunStorage) ListListsContainingAccount(ctx context.Context, ownerID, memberID string) ([]*models.List, error) {
	var lists []*models.List
	err := s.db.NewSelect().Model(&lists).
		Join("JOIN list_accounts AS la ON la.list_id = list.id").
		Where("list.account_id = ?", ownerID).
		Where("la.account_id = ?", memberID).
		Order("title ASC").
		Scan(ctx)
	return lists, err
}

// ListsContainingAccount returns every list (any owner) containing memberID.
// Bounded by the total memberships of one author; powers streaming fan-out
// and exclusive home suppression.
func (s *bunStorage) ListsContainingAccount(ctx context.Context, memberID string) ([]*models.List, error) {
	var lists []*models.List
	err := s.db.NewSelect().Model(&lists).
		Join("JOIN list_accounts AS la ON la.list_id = list.id").
		Where("la.account_id = ?", memberID).
		Scan(ctx)
	return lists, err
}

// Account Migration

// GetFollowersToMigrate returns followers (accepted follows) of accountID,
// excluding suspended accounts and unreachable instances. Used for
// delivering Move activities during outgoing migration.
func (s *bunStorage) GetFollowersToMigrate(ctx context.Context, accountID string, limit int) ([]*models.Account, error) {
	var accounts []*models.Account
	q := s.db.NewSelect().
		Model(&accounts).
		Join("JOIN follows AS f ON f.account_id = account.id").
		Where("f.target_account_id = ?", accountID).
		Where("f.state = ?", models.FollowAccepted).
		Where("account.suspended_at IS NULL").
		Order("f.created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Scan(ctx)
	return accounts, err
}

// GetFollowingToMigrate returns accounts that the given account follows.
// Used for importing follows after migration to this instance.
func (s *bunStorage) GetFollowingToMigrate(ctx context.Context, accountID string, limit int) ([]*models.Account, error) {
	var accounts []*models.Account
	q := s.db.NewSelect().
		Model(&accounts).
		Join("JOIN follows AS f ON f.target_account_id = account.id").
		Where("f.account_id = ?", accountID).
		Where("f.state = ?", models.FollowAccepted).
		Order("f.created_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Scan(ctx)
	return accounts, err
}

// SetAccountMoved marks an account as moved to the given URI, sets
// moved_at timestamp, and locks the account to prevent new follows.
func (s *bunStorage) SetAccountMoved(ctx context.Context, accountID string, movedToURI string) error {
	now := time.Now()
	_, err := s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("moved_to_uri = ?", movedToURI).
		Set("moved_at = ?", now).
		Set("locked = ?", true).
		Set("manually_approves_followers = ?", true).
		Set("updated_at = ?", now).
		Where("id = ?", accountID).
		Exec(ctx)
	return err
}

// SetAccountAlias sets or appends the alsoKnownAs field on a local account.
// Called when we receive a Move activity targeting one of our accounts.
// If the alias URI is already present, it's a no-op.
func (s *bunStorage) SetAccountAlias(ctx context.Context, accountID string, alsoKnownAs string) error {
	account, err := s.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	updated := accountmigration.AppendAccountAlias(account.AlsoKnownAs, alsoKnownAs)
	if updated == account.AlsoKnownAs {
		return nil
	}
	now := time.Now()
	_, err = s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("also_known_as = ?", updated).
		Set("updated_at = ?", now).
		Where("id = ?", accountID).
		Exec(ctx)
	return err
}

// MigrateFollower replaces a follow from followerID->oldTargetID with a follow
// to followerID->newTargetID. If already following newTarget, just removes the old follow.
func (s *bunStorage) MigrateFollower(ctx context.Context, followerID, oldTargetID, newTargetID string) error {
	// Find the old follow (followerID -> oldTargetID)
	oldFollow, err := s.getFollowByPair(ctx, followerID, oldTargetID)
	if err != nil {
		// Not following old target at all - nothing to migrate
		return nil
	}

	// Check if already following the new target
	existingFollow, err := s.getFollowByPair(ctx, followerID, newTargetID)
	if err == nil {
		_ = existingFollow
		// Already following new target - just delete the old follow
		return s.DeleteFollow(ctx, oldFollow.ID)
	}

	// Delete old follow
	if err := s.DeleteFollow(ctx, oldFollow.ID); err != nil {
		return fmt.Errorf("migrate: delete old follow: %w", err)
	}

	// Create a new follow using a real local activity URI. Remote follows stay
	// pending until the destination accepts the Follow activity sent by the
	// caller; local follows can be accepted immediately.
	follower, err := s.GetAccount(ctx, followerID)
	if err != nil {
		return fmt.Errorf("migrate: get follower: %w", err)
	}
	newTarget, err := s.GetAccount(ctx, newTargetID)
	if err != nil {
		return fmt.Errorf("migrate: get new target: %w", err)
	}
	state := models.FollowAccepted
	if newTarget.Domain != nil {
		state = models.FollowPending
	}
	followID := newID()
	newFollow := &models.Follow{
		ID:              followID,
		AccountID:       followerID,
		TargetAccountID: newTargetID,
		URI:             fmt.Sprintf("%s#follows/%s", follower.ActorURI, followID),
		State:           state,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := s.CreateFollow(ctx, newFollow); err != nil {
		return fmt.Errorf("migrate: create new follow: %w", err)
	}

	return nil
}

// getFollowByPair is an internal helper that returns a follow by account pair.
func (s *bunStorage) getFollowByPair(ctx context.Context, accountID, targetID string) (*models.Follow, error) {
	follow := new(models.Follow)
	err := s.db.NewSelect().Model(follow).
		Where("account_id = ?", accountID).
		Where("target_account_id = ?", targetID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return follow, nil
}

// Tombstones

func (s *bunStorage) CreateQuoteAuthorization(ctx context.Context, qa *models.QuoteAuthorization) error {
	if qa.ID == "" {
		qa.ID = newID()
	}
	if qa.CreatedAt.IsZero() {
		qa.CreatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(qa).
		On("CONFLICT (uri) DO NOTHING").
		Exec(ctx)
	return err
}

func (s *bunStorage) GetQuoteAuthorizationByURI(ctx context.Context, uri string) (*models.QuoteAuthorization, error) {
	qa := new(models.QuoteAuthorization)
	err := s.db.NewSelect().Model(qa).Where("uri = ?", uri).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return qa, nil
}

func (s *bunStorage) GetQuoteAuthorizationForQuoting(ctx context.Context, quotingURI string) (*models.QuoteAuthorization, error) {
	qa := new(models.QuoteAuthorization)
	err := s.db.NewSelect().Model(qa).Where("quoting_uri = ?", quotingURI).Limit(1).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return qa, nil
}

func (s *bunStorage) CreateTombstone(ctx context.Context, t *models.Tombstone) error {
	return s.CreateTombstones(ctx, []*models.Tombstone{t})
}

func (s *bunStorage) CreateTombstones(ctx context.Context, tombstones []*models.Tombstone) error {
	if len(tombstones) == 0 {
		return nil
	}
	now := time.Now()
	for _, t := range tombstones {
		if t.ID == "" {
			t.ID = newID()
		}
		if t.CreatedAt.IsZero() {
			t.CreatedAt = now
		}
	}
	// ON CONFLICT DO NOTHING - if the same URI is deleted twice, no-op
	_, err := s.db.NewInsert().Model(&tombstones).
		On("CONFLICT (uri) DO NOTHING").
		Exec(ctx)
	return err
}

func (s *bunStorage) GetTombstoneByURI(ctx context.Context, uri string) (*models.Tombstone, error) {
	t := new(models.Tombstone)
	err := s.db.NewSelect().Model(t).Where("uri = ?", uri).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *bunStorage) DeleteTombstone(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.Tombstone)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) DeleteTombstoneByURI(ctx context.Context, uri string) error {
	_, err := s.db.NewDelete().Model((*models.Tombstone)(nil)).Where("uri = ?", uri).Exec(ctx)
	return err
}

func (s *bunStorage) ListTombstonesOlderThan(ctx context.Context, since time.Time, limit int) ([]*models.Tombstone, error) {
	var tombstones []*models.Tombstone
	q := s.db.NewSelect().Model(&tombstones).
		Where("created_at < ?", since).
		Order("created_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Scan(ctx)
	return tombstones, err
}

// Sessions

func (s *bunStorage) CreateSession(ctx context.Context, session *models.Session) error {
	if session.TokenHash == "" {
		if session.Token == "" {
			return fmt.Errorf("storage: session token is required")
		}
		session.TokenHash = hashBearerSecret(session.Token)
	}
	if session.ID == "" {
		session.ID = newID()
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = time.Now()
	}
	if session.ExpiresAt.IsZero() {
		session.ExpiresAt = time.Now().Add(30 * 24 * time.Hour) // 30 days default
	}
	_, err := s.db.NewInsert().Model(session).Exec(ctx)
	return err
}

func (s *bunStorage) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*models.Session, error) {
	session := new(models.Session)
	err := s.db.NewSelect().Model(session).
		Where("token_hash = ?", tokenHash).
		Where("expires_at > ?", time.Now()).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (s *bunStorage) UpdateSessionExpiresAt(ctx context.Context, id string, expiresAt time.Time) error {
	_, err := s.db.NewUpdate().Model((*models.Session)(nil)).
		Set("expires_at = ?", expiresAt).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (s *bunStorage) DeleteSession(ctx context.Context, id string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*models.PushSubscription)(nil)).
			Where("session_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*models.Session)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	})
}

func (s *bunStorage) DeleteSessionsForAccount(ctx context.Context, accountID string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		sessionIDs := tx.NewSelect().Model((*models.Session)(nil)).Column("id").
			Where("account_id = ?", accountID)
		if _, err := tx.NewDelete().Model((*models.PushSubscription)(nil)).
			Where("session_id IN (?)", sessionIDs).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*models.Session)(nil)).Where("account_id = ?", accountID).Exec(ctx)
		return err
	})
}

func (s *bunStorage) ListSessionsByAccount(ctx context.Context, accountID string) ([]*models.Session, error) {
	var sessions []*models.Session
	err := s.db.NewSelect().Model(&sessions).
		Where("account_id = ?", accountID).
		Where("expires_at > ?", time.Now()).
		Order("created_at DESC").
		Scan(ctx)
	return sessions, err
}

// Admin / User Management

func (s *bunStorage) SetAdmin(ctx context.Context, accountID string, isAdmin bool) error {
	_, err := s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("is_admin = ?", isAdmin).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", accountID).
		Exec(ctx)
	return err
}

func (s *bunStorage) SetBlogAllowed(ctx context.Context, accountID string, allowed bool) error {
	_, err := s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("blog_allowed = ?", allowed).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", accountID).
		Exec(ctx)
	return err
}

func (s *bunStorage) GetAdmins(ctx context.Context) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("domain IS NULL").
		Where("is_admin = ?", true).
		Order("created_at ASC").
		Scan(ctx)
	return accounts, err
}

func (s *bunStorage) CountLocalAccounts(ctx context.Context) (int, error) {
	return s.db.NewSelect().Model((*models.Account)(nil)).
		Where("domain IS NULL").
		Where("is_service_actor = ?", false).
		Count(ctx)
}

func (s *bunStorage) GetLocalAccounts(ctx context.Context) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("domain IS NULL").
		Where("is_service_actor = ?", false).
		Order("created_at ASC").
		Scan(ctx)
	return accounts, err
}

func (s *bunStorage) GetAllLocalAccounts(ctx context.Context) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("domain IS NULL").
		Order("created_at ASC").
		Scan(ctx)
	return accounts, err
}

// PurgeRetiredPrivateKeyData rewrites storage after plaintext private keys
// have been replaced. This keeps old values out of free database pages and
// backups made from those pages.
func (s *bunStorage) PurgeRetiredPrivateKeyData(ctx context.Context) error {
	switch s.dialect {
	case "sqlite":
		if err := s.sqliteCheckpointTruncate(ctx); err != nil {
			return fmt.Errorf("checkpoint before private key cleanup: %w", err)
		}
		if _, err := s.sqldb.ExecContext(ctx, "VACUUM"); err != nil {
			return fmt.Errorf("compact private key storage: %w", err)
		}
		if err := s.sqliteCheckpointTruncate(ctx); err != nil {
			return fmt.Errorf("checkpoint after private key cleanup: %w", err)
		}
		return nil
	case "pg":
		if _, err := s.sqldb.ExecContext(ctx, "VACUUM (FULL, ANALYZE) accounts"); err != nil {
			return fmt.Errorf("compact private key storage: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("compact private key storage: unsupported dialect %q", s.dialect)
	}
}

func (s *bunStorage) sqliteCheckpointTruncate(ctx context.Context) error {
	var busy, logPages, checkpointedPages int
	if err := s.sqldb.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &checkpointedPages); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("database is busy")
	}
	return nil
}

func (s *bunStorage) GetWelcomeDirectMessage(ctx context.Context) (*models.WelcomeDirectMessage, error) {
	message := new(models.WelcomeDirectMessage)
	err := s.db.NewSelect().Model(message).Where("id = 1").Scan(ctx)
	return message, err
}

func (s *bunStorage) SetWelcomeDirectMessage(ctx context.Context, senderAccountID, content string) error {
	message := &models.WelcomeDirectMessage{ID: 1, SenderAccountID: senderAccountID, Content: content, UpdatedAt: time.Now()}
	_, err := s.db.NewInsert().Model(message).
		On("CONFLICT (id) DO UPDATE").
		Set("sender_account_id = EXCLUDED.sender_account_id").
		Set("content = EXCLUDED.content").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

func (s *bunStorage) DeleteWelcomeDirectMessage(ctx context.Context) error {
	_, err := s.db.NewDelete().Model((*models.WelcomeDirectMessage)(nil)).Where("id = 1").Exec(ctx)
	return err
}

func (s *bunStorage) GetInstanceMeta(ctx context.Context) (*models.InstanceMeta, error) {
	meta := new(models.InstanceMeta)
	err := s.db.NewSelect().Model(meta).Where("id = 1").Scan(ctx)
	if err == sql.ErrNoRows {
		// Not an error: no administrator has saved a profile, so the caller
		// resolves everything from the config file instead.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return meta, nil
}

func (s *bunStorage) SaveInstanceMeta(ctx context.Context, meta *models.InstanceMeta) error {
	meta.ID = 1
	meta.UpdatedAt = time.Now()
	_, err := s.db.NewInsert().Model(meta).
		On("CONFLICT (id) DO UPDATE").
		Set("short_description = EXCLUDED.short_description").
		Set("description = EXCLUDED.description").
		Set("extended_description = EXCLUDED.extended_description").
		Set("terms_of_service = EXCLUDED.terms_of_service").
		Set("contact_email = EXCLUDED.contact_email").
		Set("rules = EXCLUDED.rules").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

func (s *bunStorage) ListLocalIdentities(ctx context.Context) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("domain IS NULL").
		Scan(ctx)
	return accounts, err
}

// Invites

func (s *bunStorage) CreateInvite(ctx context.Context, invite *models.Invite) error {
	if invite.ID == "" {
		invite.ID = newID()
	}
	if invite.CreatedAt.IsZero() {
		invite.CreatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(invite).Exec(ctx)
	return err
}

func (s *bunStorage) GetInviteByCode(ctx context.Context, code string) (*models.Invite, error) {
	invite := new(models.Invite)
	err := s.db.NewSelect().Model(invite).Where("code = ?", code).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return invite, nil
}

func (s *bunStorage) IncrementInviteUse(ctx context.Context, id string) error {
	result, err := s.db.NewUpdate().Model((*models.Invite)(nil)).
		Set("use_count = use_count + 1").
		Where("id = ?", id).
		Where("(max_uses = 0 OR use_count < max_uses)").
		Where("(expires_at IS NULL OR expires_at > ?)", time.Now()).
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrInviteUnavailable
	}
	return nil
}

func (s *bunStorage) DecrementInviteUse(ctx context.Context, id string) error {
	_, err := s.db.NewUpdate().Model((*models.Invite)(nil)).
		Set("use_count = use_count - 1").
		Where("id = ?", id).
		Where("use_count > 0").
		Exec(ctx)
	return err
}

func (s *bunStorage) ListInvites(ctx context.Context) ([]*models.Invite, error) {
	var invites []*models.Invite
	err := s.db.NewSelect().Model(&invites).
		Order("created_at DESC").
		Scan(ctx)
	return invites, err
}

func (s *bunStorage) DeleteInvite(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.Invite)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// Password Resets

func (s *bunStorage) CreatePasswordReset(ctx context.Context, reset *models.PasswordReset) error {
	if reset.ID == "" {
		reset.ID = newID()
	}
	if reset.CreatedAt.IsZero() {
		reset.CreatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(reset).Exec(ctx)
	return err
}

func (s *bunStorage) GetPasswordResetByTokenHash(ctx context.Context, tokenHash string) (*models.PasswordReset, error) {
	reset := new(models.PasswordReset)
	err := s.db.NewSelect().Model(reset).Where("token_hash = ?", tokenHash).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return reset, nil
}

func (s *bunStorage) MarkPasswordResetUsed(ctx context.Context, id string) error {
	res, err := s.db.NewUpdate().Model((*models.PasswordReset)(nil)).
		Set("used = ?", true).
		Where("id = ?", id).
		Where("used = ?", false).
		Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("password reset token already used or not found")
	}
	return nil
}

// Registration / Approval

func (s *bunStorage) GetPendingApprovals(ctx context.Context) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("domain IS NULL").
		Where("approved = ?", false).
		Order("created_at ASC").
		Scan(ctx)
	return accounts, err
}

func (s *bunStorage) ApproveAccount(ctx context.Context, id string) error {
	now := time.Now()
	_, err := s.db.NewUpdate().Model((*models.Account)(nil)).
		Set("approved = ?", true).
		Set("approved_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (s *bunStorage) RejectAccount(ctx context.Context, id string) error {
	// For rejection, delete the pending account
	_, err := s.db.NewDelete().Model((*models.Account)(nil)).
		Where("id = ?", id).
		Where("approved = ?", false).
		Exec(ctx)
	return err
}

// Email

func (s *bunStorage) GetAccountByEmail(ctx context.Context, email string) (*models.Account, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, fmt.Errorf("email is empty")
	}
	account := new(models.Account)
	err := s.db.NewSelect().Model(account).Where("LOWER(email) = ?", email).Where("domain IS NULL").Scan(ctx)
	if err != nil {
		return nil, err
	}
	return account, nil
}

func (s *bunStorage) CreateEmailVerification(ctx context.Context, v *models.EmailVerification) error {
	if v.ID == "" {
		v.ID = newID()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*models.EmailVerification)(nil)).Set("used = ?", true).
			Where("account_id = ?", v.AccountID).Where("used = ?", false).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(v).Exec(ctx)
		return err
	})
}

func (s *bunStorage) VerifyEmail(ctx context.Context, tokenHash string, now time.Time) (*models.Account, error) {
	var account models.Account
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		verification := new(models.EmailVerification)
		if err := tx.NewSelect().Model(verification).Where("token_hash = ?", tokenHash).Scan(ctx); err != nil {
			return ErrEmailVerificationInvalid
		}
		if verification.Used || !now.Before(verification.ExpiresAt) {
			return ErrEmailVerificationInvalid
		}
		if err := tx.NewSelect().Model(&account).Where("id = ?", verification.AccountID).
			Where("email = ?", verification.Email).Scan(ctx); err != nil {
			return ErrEmailVerificationInvalid
		}
		result, err := tx.NewUpdate().Model((*models.EmailVerification)(nil)).Set("used = ?", true).
			Where("id = ?", verification.ID).Where("used = ?", false).Exec(ctx)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrEmailVerificationInvalid
		}
		if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
			Set("email_verified = ?", true).Set("email_verified_at = ?", now).Set("updated_at = ?", now).
			Where("id = ?", account.ID).Exec(ctx); err != nil {
			return err
		}
		account.EmailVerified = true
		account.EmailVerifiedAt = &now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &account, nil
}

func (s *bunStorage) SetAccountMFA(ctx context.Context, accountID, encryptedSecret string, recoveryCodeHashes []string) error {
	if encryptedSecret == "" || len(recoveryCodeHashes) == 0 {
		return fmt.Errorf("MFA secret and recovery codes are required")
	}
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*models.Account)(nil)).
			Set("mfa_enabled = ?", true).Set("totp_secret_encrypted = ?", encryptedSecret).
			Set("updated_at = ?", time.Now()).Where("id = ?", accountID).Exec(ctx); err != nil {
			return err
		}
		if err := replaceMFARecoveryCodes(ctx, tx, accountID, recoveryCodeHashes); err != nil {
			return err
		}
		return revokeAccountCredentials(ctx, tx, accountID)
	})
}

func (s *bunStorage) ReplaceMFARecoveryCodes(ctx context.Context, accountID string, recoveryCodeHashes []string) error {
	if len(recoveryCodeHashes) == 0 {
		return fmt.Errorf("MFA recovery codes are required")
	}
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewUpdate().Model((*models.Account)(nil)).Set("updated_at = ?", time.Now()).Where("id = ?", accountID).Where("mfa_enabled = ?", true).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return sql.ErrNoRows
		}

		if err := replaceMFARecoveryCodes(ctx, tx, accountID, recoveryCodeHashes); err != nil {
			return err
		}
		return revokeAccountCredentials(ctx, tx, accountID)
	})
}

func replaceMFARecoveryCodes(ctx context.Context, tx bun.Tx, accountID string, hashes []string) error {
	if _, err := tx.NewDelete().Model((*models.MFARecoveryCode)(nil)).Where("account_id = ?", accountID).Exec(ctx); err != nil {
		return err
	}
	now := time.Now().UTC()
	codes := make([]*models.MFARecoveryCode, 0, len(hashes))
	for _, hash := range hashes {
		codes = append(codes, &models.MFARecoveryCode{ID: newID(), AccountID: accountID, CodeHash: hash, CreatedAt: now})
	}
	_, err := tx.NewInsert().Model(&codes).Exec(ctx)
	return err
}

func (s *bunStorage) DisableAccountMFA(ctx context.Context, accountID string) error {
	return s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*models.MFARecoveryCode)(nil)).Where("account_id = ?", accountID).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Model((*models.Account)(nil)).
			Set("mfa_enabled = ?", false).Set("totp_secret_encrypted = ''").Set("updated_at = ?", time.Now()).
			Where("id = ?", accountID).Exec(ctx)
		if err != nil {
			return err
		}
		return revokeAccountCredentials(ctx, tx, accountID)
	})
}

func (s *bunStorage) ConsumeMFARecoveryCode(ctx context.Context, accountID, codeHash string) (bool, error) {
	result, err := s.db.NewUpdate().Model((*models.MFARecoveryCode)(nil)).Set("used_at = ?", time.Now().UTC()).
		Where("account_id = ?", accountID).Where("code_hash = ?", codeHash).Where("used_at IS NULL").Exec(ctx)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *bunStorage) UpsertEmailDigestState(ctx context.Context, accountID string, sentAt time.Time) error {
	state := &models.EmailDigestState{
		AccountID:  accountID,
		LastSentAt: sentAt,
	}
	_, err := s.db.NewInsert().Model(state).
		On("CONFLICT (account_id) DO UPDATE").
		Set("last_sent_at = EXCLUDED.last_sent_at").
		Exec(ctx)
	return err
}

func (s *bunStorage) GetEmailDigestDue(ctx context.Context, before time.Time) ([]*models.Account, error) {
	var accounts []*models.Account
	err := s.db.NewSelect().Model(&accounts).
		Where("email IS NOT NULL AND email != ''").
		Where("email_notify_digest = ?", true).
		Where("domain IS NULL").
		Where(`(
			NOT EXISTS (SELECT 1 FROM email_digest_state ds WHERE ds.account_id = account.id)
			OR EXISTS (SELECT 1 FROM email_digest_state ds WHERE ds.account_id = account.id AND ds.last_sent_at < ?)
		)`, before).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

// Push Subscriptions

func (s *bunStorage) CreatePushSubscription(ctx context.Context, sub *models.PushSubscription) error {
	if sub.ID == "" {
		sub.ID = newID()
	}
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = time.Now()
	}
	if sub.UpdatedAt.IsZero() {
		sub.UpdatedAt = time.Now()
	}
	_, err := s.db.NewInsert().Model(sub).Exec(ctx)
	return err
}

func (s *bunStorage) GetPushSubscription(ctx context.Context, id string) (*models.PushSubscription, error) {
	sub := new(models.PushSubscription)
	err := s.db.NewSelect().Model(sub).Where("id = ?", id).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (s *bunStorage) ListPushSubscriptionsByAccountID(ctx context.Context, accountID string) ([]*models.PushSubscription, error) {
	var subs []*models.PushSubscription
	err := s.db.NewSelect().Model(&subs).Where("account_id = ?", accountID).Order("updated_at DESC").Scan(ctx)
	if err != nil {
		return nil, err
	}
	return subs, nil
}

func (s *bunStorage) ListPushSubscriptionsByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]*models.PushSubscription, error) {
	out := make(map[string][]*models.PushSubscription, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	var subs []*models.PushSubscription
	if err := s.db.NewSelect().Model(&subs).
		Where("account_id IN (?)", bun.In(accountIDs)).Order("updated_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, sub := range subs {
		out[sub.AccountID] = append(out[sub.AccountID], sub)
	}
	return out, nil
}

func (s *bunStorage) GetPushSubscriptionByOAuthTokenID(ctx context.Context, oauthTokenID string) (*models.PushSubscription, error) {
	sub := new(models.PushSubscription)
	err := s.db.NewSelect().Model(sub).Where("oauth_token_id = ?", oauthTokenID).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (s *bunStorage) GetPushSubscriptionBySessionID(ctx context.Context, sessionID string) (*models.PushSubscription, error) {
	sub := new(models.PushSubscription)
	err := s.db.NewSelect().Model(sub).Where("session_id = ?", sessionID).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (s *bunStorage) GetPushSubscriptionByEndpoint(ctx context.Context, endpoint string) (*models.PushSubscription, error) {
	sub := new(models.PushSubscription)
	err := s.db.NewSelect().Model(sub).Where("endpoint = ?", endpoint).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (s *bunStorage) UpdatePushSubscription(ctx context.Context, sub *models.PushSubscription) error {
	sub.UpdatedAt = time.Now()
	_, err := s.db.NewUpdate().Model(sub).WherePK().Exec(ctx)
	return err
}

func (s *bunStorage) DeletePushSubscription(ctx context.Context, id string) error {
	_, err := s.db.NewDelete().Model((*models.PushSubscription)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *bunStorage) GetOrCreateVAPIDKeyPair(ctx context.Context, candidate *models.VAPIDKeyPair) (*models.VAPIDKeyPair, error) {
	if candidate.ID == "" {
		candidate.ID = "application"
	}
	if candidate.UpdatedAt.IsZero() {
		candidate.UpdatedAt = time.Now()
	}
	if _, err := s.db.NewInsert().Model(candidate).On("CONFLICT (id) DO NOTHING").Exec(ctx); err != nil {
		return nil, err
	}
	pair := new(models.VAPIDKeyPair)
	if err := s.db.NewSelect().Model(pair).Where("id = ?", candidate.ID).Scan(ctx); err != nil {
		return nil, err
	}
	return pair, nil
}

func (s *bunStorage) GetOrCreateMeshIdentity(ctx context.Context, candidate *models.MeshIdentity) (*models.MeshIdentity, error) {
	if candidate.ID == "" {
		candidate.ID = "node"
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now()
	}
	if _, err := s.db.NewInsert().Model(candidate).On("CONFLICT (id) DO NOTHING").Exec(ctx); err != nil {
		return nil, err
	}
	identity := new(models.MeshIdentity)
	if err := s.db.NewSelect().Model(identity).Where("id = ?", candidate.ID).Scan(ctx); err != nil {
		return nil, err
	}
	return identity, nil
}

func (s *bunStorage) ReplaceVAPIDKeyPair(ctx context.Context, pair *models.VAPIDKeyPair) (bool, error) {
	if pair.ID == "" {
		pair.ID = "application"
	}
	changed := false
	err := s.runInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		current := new(models.VAPIDKeyPair)
		err := tx.NewSelect().Model(current).Where("id = ?", pair.ID).Scan(ctx)
		if err == sql.ErrNoRows {
			pair.UpdatedAt = time.Now()
			_, err = tx.NewInsert().Model(pair).Exec(ctx)
			return err
		}
		if err != nil {
			return err
		}
		if current.PublicKey == pair.PublicKey && current.PrivateKey == pair.PrivateKey {
			return nil
		}
		changed = true
		pair.UpdatedAt = time.Now()
		if _, err := tx.NewUpdate().Model(pair).WherePK().Exec(ctx); err != nil {
			return err
		}
		_, err = tx.NewDelete().Model((*models.PushSubscription)(nil)).Where("1 = 1").Exec(ctx)
		return err
	})
	return changed, err
}

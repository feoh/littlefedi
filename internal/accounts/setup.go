// Package accounts handles local account bootstrap, creation, and management.
package accounts

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/auth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// BootstrapLocalAccount creates a local account if it doesn't exist.
// Returns the existing account if present.
// isAdmin sets the admin flag on the account.
// A non-empty password sets the configured password. An unchanged password
// leaves existing sessions alone.
func BootstrapLocalAccount(ctx context.Context, store storage.Storage, domain, username, password string, isAdmin bool) (*models.Account, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	account, err := store.GetAccountByUsernameDomain(ctx, username, nil)
	if err == nil {
		// Account exists - update password if provided
		if password != "" && !auth.CheckPassword(password, account.PasswordHash) {
			hash, err := auth.HashPassword(password)
			if err == nil {
				account.PasswordHash = hash
				if err := store.UpdateAccount(ctx, account); err != nil {
					slog.Warn("failed to store password hash for existing account", "error", err)
				} else {
					slog.Info("updated password hash from config", "username", username)
				}
			}
		}

		// Ensure admin flag is set if requested
		if isAdmin && !account.IsAdmin {
			if err := store.SetAdmin(ctx, account.ID, true); err != nil {
				slog.Warn("failed to set admin flag for existing account", "error", err)
			} else {
				account.IsAdmin = true
				slog.Info("promoted account to admin", "username", username)
			}
		}

		slog.Info("local account already exists", "username", username)
		return account, nil
	}

	// Account doesn't exist - create it
	privKey, pubKey, edPrivKey, edPubKey, err := federation.GenerateDualKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generate keys: %w", err)
	}
	sealedPrivKey, err := keycrypt.Seal(privKey)
	if err != nil {
		return nil, fmt.Errorf("seal private key: %w", err)
	}
	sealedEdPrivKey, err := keycrypt.Seal(edPrivKey)
	if err != nil {
		return nil, fmt.Errorf("seal ed25519 private key: %w", err)
	}

	now := time.Now()
	account = &models.Account{
		Username:             username,
		Domain:               nil,
		ActorURI:             fmt.Sprintf("https://%s/users/%s", domain, username),
		InboxURI:             fmt.Sprintf("https://%s/users/%s/inbox", domain, username),
		OutboxURI:            fmt.Sprintf("https://%s/users/%s/outbox", domain, username),
		FollowersURI:         fmt.Sprintf("https://%s/users/%s/followers", domain, username),
		FollowingURI:         fmt.Sprintf("https://%s/users/%s/following", domain, username),
		PublicKeyPem:         pubKey,
		PublicKeyID:          fmt.Sprintf("https://%s/users/%s#main-key", domain, username),
		PrivateKeyPem:        sealedPrivKey,
		Ed25519PublicKeyPem:  edPubKey,
		Ed25519PublicKeyID:   fmt.Sprintf("https://%s/users/%s#ed25519-key", domain, username),
		Ed25519PrivateKeyPem: sealedEdPrivKey,
		DisplayName:          username,
		CreatedAt:            now,
		UpdatedAt:            now,
		IsAdmin:              isAdmin,
		Approved:             true,
	}

	// Hash password if provided
	if password != "" {
		hash, err := auth.HashPassword(password)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}
		account.PasswordHash = hash
	}

	if err := store.CreateAccount(ctx, account); err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	if err := app.SendWelcomeDirectMessage(ctx, store, domain, account); err != nil {
		slog.Warn("failed to send welcome direct message", "username", username, "error", err)
	}

	slog.Info("created local account", "username", username, "actor_uri", account.ActorURI, "is_admin", isAdmin)
	return account, nil
}

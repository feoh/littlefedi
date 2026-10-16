// Package accounts handles registration, validation, and invite logic.
package accounts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/app"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/auth"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/federation"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

const DefaultMinPasswordLength = 15

var (
	ErrRegistrationClosed = errors.New("registration is closed")
	ErrUsernameTaken      = errors.New("username is already taken")
	ErrInvalidUsername    = errors.New("invalid username")
	ErrPasswordTooShort   = errors.New("password is too short")
	ErrInviteRequired     = errors.New("invite code is required")
	ErrInviteInvalid      = errors.New("invite code is invalid or expired")
	ErrInviteExhausted    = errors.New("invite code has been used too many times")
	ErrPasswordMismatch   = errors.New("passwords do not match")
	ErrEmailRequired      = errors.New("email is required")
	ErrInvalidEmail       = errors.New("invalid email address")
	ErrEmailTaken         = errors.New("email address is already in use")
)

var validUsername = regexp.MustCompile(`^[a-zA-Z0-9_]{2,30}$`)
var validEmail = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// reservedUsernames are names that cannot be used for local accounts.
var reservedUsernames = map[string]bool{
	"admin":         true,
	"root":          true,
	"instance":      true,
	"littlefedi":    true,
	"system":        true,
	"api":           true,
	"auth":          true,
	"oauth":         true,
	"settings":      true,
	"notifications": true,
	"public":        true,
	"local":         true,
	"about":         true,
	"help":          true,
	"support":       true,
	"contact":       true,
	"terms":         true,
	"privacy":       true,
	"robots.txt":    true,
	"favicon.ico":   true,
	"static":        true,
	"media":         true,
	"avatars":       true,
	"headers":       true,
	"emojis":        true,
	"users":         true,
	"objects":       true,
	"inbox":         true,
	"outbox":        true,
	"nodeinfo":      true,
	".well-known":   true,
}

// RegistrationRequest holds the fields for a new user registration.
type RegistrationRequest struct {
	Username        string
	Email           string // optional
	Password        string
	PasswordConfirm string
	InviteCode      string // optional, required if invite_required
}

// RegistrationConfig defines the registration policy for the instance.
type RegistrationConfig interface {
	IsMultiUser() bool
	GetRegistrations() bool
	GetInviteRequired() bool
	GetMinPasswordLength() int
	GetRequireApproval() bool
	GetRequireEmail() bool
}

// ConfigAdapter adapts a config to RegistrationConfig.
type ConfigAdapter struct {
	MultiUser         bool
	Registrations     bool
	InviteRequired    bool
	MinPasswordLength int
	RequireApproval   bool
	RequireEmail      bool
}

func (c *ConfigAdapter) IsMultiUser() bool         { return c.MultiUser }
func (c *ConfigAdapter) GetRegistrations() bool    { return c.Registrations }
func (c *ConfigAdapter) GetInviteRequired() bool   { return c.InviteRequired }
func (c *ConfigAdapter) GetMinPasswordLength() int { return c.MinPasswordLength }
func (c *ConfigAdapter) GetRequireApproval() bool  { return c.RequireApproval }
func (c *ConfigAdapter) GetRequireEmail() bool     { return c.RequireEmail }

// ValidateUsername applies the local-account username rules used by registration.
func ValidateUsername(username string) error {
	if !validUsername.MatchString(username) {
		return fmt.Errorf("%w: must be 2-30 alphanumeric characters or underscores", ErrInvalidUsername)
	}
	if reservedUsernames[strings.ToLower(username)] {
		return fmt.Errorf("%w: username is reserved", ErrInvalidUsername)
	}
	return nil
}

// ValidatePassword applies the password length rule used by registration.
func ValidatePassword(password string, minLen int) error {
	if minLen <= 0 {
		minLen = DefaultMinPasswordLength
	}
	if utf8.RuneCountInString(password) < minLen {
		return fmt.Errorf("%w: minimum length is %d characters", ErrPasswordTooShort, minLen)
	}
	return nil
}

// ValidateEmail applies the email-address rule used by every account write.
func ValidateEmail(email string) error {
	if !validEmail.MatchString(email) {
		return ErrInvalidEmail
	}
	return nil
}

// Register creates a new local account with the given registration request.
func Register(ctx context.Context, store storage.Storage, domain string, regCfg RegistrationConfig, req *RegistrationRequest) (*models.Account, error) {
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	// Check if registration is open
	if !regCfg.GetRegistrations() {
		return nil, ErrRegistrationClosed
	}

	// Validate username
	if err := ValidateUsername(req.Username); err != nil {
		return nil, err
	}

	// Validate password
	if err := ValidatePassword(req.Password, regCfg.GetMinPasswordLength()); err != nil {
		return nil, err
	}

	// Check password confirmation
	if req.Password != req.PasswordConfirm {
		return nil, ErrPasswordMismatch
	}

	// Validate email
	if regCfg.GetRequireEmail() && regCfg.IsMultiUser() && req.Email == "" {
		return nil, ErrEmailRequired
	}
	if req.Email != "" {
		if err := ValidateEmail(req.Email); err != nil {
			return nil, err
		}
		existing, err := store.GetAccountByEmail(ctx, req.Email)
		if err == nil && existing != nil {
			return nil, ErrEmailTaken
		}
	}

	// Check invite if required
	var invite *models.Invite
	if regCfg.GetInviteRequired() {
		if req.InviteCode == "" {
			return nil, ErrInviteRequired
		}
		var err error
		invite, err = store.GetInviteByCode(ctx, req.InviteCode)
		if err != nil {
			return nil, ErrInviteInvalid
		}
		// Check if expired
		if invite.ExpiresAt != nil && time.Now().After(*invite.ExpiresAt) {
			return nil, ErrInviteInvalid
		}
		// Check max uses
		if invite.MaxUses > 0 && invite.UseCount >= invite.MaxUses {
			return nil, ErrInviteExhausted
		}
	}

	// Check username is not already taken
	existing, err := store.GetAccountByUsernameDomain(ctx, req.Username, nil)
	if err == nil && existing != nil {
		return nil, ErrUsernameTaken
	}

	// Hash password
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	// Generate key pairs (RSA primary + Ed25519 for OIP / ed25519 signatures)
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
	approved := !regCfg.GetRequireApproval()

	account := &models.Account{
		Username:             req.Username,
		Domain:               nil,
		ActorURI:             fmt.Sprintf("https://%s/users/%s", domain, req.Username),
		InboxURI:             fmt.Sprintf("https://%s/users/%s/inbox", domain, req.Username),
		OutboxURI:            fmt.Sprintf("https://%s/users/%s/outbox", domain, req.Username),
		FollowersURI:         fmt.Sprintf("https://%s/users/%s/followers", domain, req.Username),
		FollowingURI:         fmt.Sprintf("https://%s/users/%s/following", domain, req.Username),
		PublicKeyPem:         pubKey,
		PublicKeyID:          fmt.Sprintf("https://%s/users/%s#main-key", domain, req.Username),
		PrivateKeyPem:        sealedPrivKey,
		Ed25519PublicKeyPem:  edPubKey,
		Ed25519PublicKeyID:   fmt.Sprintf("https://%s/users/%s#ed25519-key", domain, req.Username),
		Ed25519PrivateKeyPem: sealedEdPrivKey,
		DisplayName:          req.Username,
		Note:                 "",
		CreatedAt:            now,
		UpdatedAt:            now,
		Email:                req.Email,
		PasswordHash:         hash,
		Approved:             approved,
	}

	if invite != nil {
		account.InviteID = &invite.ID
		if err := store.IncrementInviteUse(ctx, invite.ID); err != nil {
			if errors.Is(err, storage.ErrInviteUnavailable) {
				return nil, ErrInviteExhausted
			}
			return nil, fmt.Errorf("consume invite: %w", err)
		}
	}

	if err := store.CreateAccount(ctx, account); err != nil {
		if invite != nil {
			if rollbackErr := store.DecrementInviteUse(ctx, invite.ID); rollbackErr != nil {
				slog.Error("failed to restore invite use after registration failure", "invite_id", invite.ID, "error", rollbackErr)
			}
		}
		return nil, fmt.Errorf("create account: %w", err)
	}
	if err := app.SendWelcomeDirectMessage(ctx, store, domain, account); err != nil {
		slog.Warn("failed to send welcome direct message", "username", account.Username, "error", err)
	}

	slog.Info("registered new local account",
		"username", account.Username,
		"actor_uri", account.ActorURI,
		"approved", account.Approved,
	)

	return account, nil
}

// GenerateInviteCode creates a random invite code.
func GenerateInviteCode() (string, error) {
	b := make([]byte, 8) // 8 bytes = 16 hex chars = 2^64 space
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate invite code: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CreateInvite creates a new invite with the given parameters.
func CreateInvite(ctx context.Context, store storage.Storage, createdByID, code string, maxUses int, expiresAt *time.Time) (*models.Invite, error) {
	if code == "" {
		generated, err := GenerateInviteCode()
		if err != nil {
			return nil, err
		}
		code = generated
	}

	invite := &models.Invite{
		CreatedAt:   time.Now(),
		ExpiresAt:   expiresAt,
		MaxUses:     maxUses,
		UseCount:    0,
		CreatedByID: createdByID,
		Code:        code,
	}

	if err := store.CreateInvite(ctx, invite); err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}

	slog.Info("created invite", "invite_id", invite.ID, "max_uses", maxUses)
	return invite, nil
}

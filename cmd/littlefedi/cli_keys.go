package main

import (
	"context"
	"fmt"
	"os"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/keycrypt"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// cmdKeys converts the stored ActivityPub private keys of local accounts
// between plaintext and sealed form. Both directions are idempotent, so an
// interrupted run is fixed by running it again.
//
// The conversion is a separate step rather than something that
// happens on first boot: rewriting every key in the database is not a thing an
// operator should discover afterwards in the logs.
func cmdKeys(ctx context.Context, store storage.Storage, args []string) error {
	if len(args) == 0 {
		printKeysUsage()
		return fmt.Errorf("missing keys command")
	}

	accounts, err := store.GetAllLocalAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list local accounts: %w", err)
	}

	switch args[0] {
	case "status":
		sealed, plain := 0, 0
		for _, account := range accounts {
			for _, key := range []string{account.PrivateKeyPem, account.Ed25519PrivateKeyPem} {
				if key == "" {
					continue
				}
				if keycrypt.IsSealed(key) {
					sealed++
				} else {
					plain++
				}
			}
		}
		fmt.Printf("Local accounts: %d\n", len(accounts))
		fmt.Printf("Private keys sealed: %d\n", sealed)
		fmt.Printf("Private keys in plaintext: %d\n", plain)
		if keycrypt.Enabled() {
			fmt.Println("Encryption key: configured")
		} else {
			fmt.Println("Encryption key: not configured (LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY is unset)")
		}
		return nil
	case "encrypt", "decrypt":
		if !keycrypt.Enabled() {
			return fmt.Errorf("LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY is not set")
		}
		convert, verb := keycrypt.Seal, "Sealed"
		if args[0] == "decrypt" {
			convert, verb = keycrypt.Open, "Unsealed"
		}

		changed := 0
		for _, account := range accounts {
			priv, err := convert(account.PrivateKeyPem)
			if err != nil {
				return fmt.Errorf("convert %s: %w", handle(account), err)
			}
			edPriv, err := convert(account.Ed25519PrivateKeyPem)
			if err != nil {
				return fmt.Errorf("convert %s: %w", handle(account), err)
			}
			if priv == account.PrivateKeyPem && edPriv == account.Ed25519PrivateKeyPem {
				continue
			}
			account.PrivateKeyPem = priv
			account.Ed25519PrivateKeyPem = edPriv
			if err := store.UpdateAccount(ctx, account); err != nil {
				return fmt.Errorf("write %s: %w", handle(account), err)
			}
			changed++
		}
		if args[0] == "encrypt" {
			if err := store.PurgeRetiredPrivateKeyData(ctx); err != nil {
				return fmt.Errorf("keys were sealed but old database pages could not be cleaned: %w", err)
			}
		}

		fmt.Printf("%s the keys of %d of %d local accounts.\n", verb, changed, len(accounts))
		if args[0] == "encrypt" {
			fmt.Println("Keep LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY somewhere the database backup is not:")
			fmt.Println("without it these accounts can no longer sign anything.")
		}
		return nil
	default:
		printKeysUsage()
		return fmt.Errorf("unknown keys command %q", args[0])
	}
}

func handle(account *models.Account) string {
	if account.IsServiceActor {
		return account.Username + " (instance actor)"
	}
	return account.Username
}

func printKeysUsage() {
	fmt.Fprintln(os.Stderr, `Usage: littlefedi admin keys <status|encrypt|decrypt>

  status     Report how many local private keys are sealed.
  encrypt    Seal every plaintext local private key with the configured key.
  decrypt    Return every sealed local private key to plaintext.

encrypt and decrypt need LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY set to the
base64 of 32 random bytes, for example:

  openssl rand -base64 32`)
}

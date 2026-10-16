// Package migration contains shared account-migration helpers.
package migration

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

var (
	ErrTargetAliasMissing = errors.New("target account does not list the old account in alsoKnownAs")
	ErrOldMovedToMismatch = errors.New("old account does not point movedTo at the target account")
)

// AccountAliases returns every alsoKnownAs URI stored on an account. Historical
// rows contain a single URI; newer rows may contain newline-separated URIs.
func AccountAliases(account *models.Account) []string {
	if account == nil || strings.TrimSpace(account.AlsoKnownAs) == "" {
		return nil
	}
	parts := strings.FieldsFunc(account.AlsoKnownAs, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' '
	})
	aliases := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		alias := strings.TrimSpace(part)
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if seen[key] {
			continue
		}
		seen[key] = true
		aliases = append(aliases, alias)
	}
	return aliases
}

// HasAccountAlias reports whether account alsoKnownAs contains actorURI.
func HasAccountAlias(account *models.Account, actorURI string) bool {
	for _, alias := range AccountAliases(account) {
		if SameActorURI(alias, actorURI) {
			return true
		}
	}
	return false
}

// AppendAccountAlias merges an alias into an existing alsoKnownAs value.
func AppendAccountAlias(existing, alias string) string {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return strings.TrimSpace(existing)
	}
	aliases := AccountAliases(&models.Account{AlsoKnownAs: existing})
	for _, current := range aliases {
		if SameActorURI(current, alias) {
			return strings.Join(aliases, "\n")
		}
	}
	aliases = append(aliases, alias)
	return strings.Join(aliases, "\n")
}

// AccountAliasesForAP returns the shape expected in ActivityPub actor docs:
// a string for one alias, an array for multiple aliases, or nil for none.
func AccountAliasesForAP(account *models.Account) interface{} {
	aliases := AccountAliases(account)
	switch len(aliases) {
	case 0:
		return nil
	case 1:
		return aliases[0]
	default:
		return aliases
	}
}

// VerifyMovePair checks the bidirectional account-migration claim. The new
// account must list the old actor in alsoKnownAs, and once the old actor has
// published movedTo it must point to the new actor.
func VerifyMovePair(oldAccount, newAccount *models.Account, requireMovedTo bool) error {
	if oldAccount == nil || newAccount == nil {
		return fmt.Errorf("missing migration account")
	}
	if !HasAccountAlias(newAccount, oldAccount.ActorURI) {
		return fmt.Errorf("%w: %s must include %s", ErrTargetAliasMissing, newAccount.ActorURI, oldAccount.ActorURI)
	}
	if requireMovedTo {
		if oldAccount.MovedToURI == "" || !SameActorURI(oldAccount.MovedToURI, newAccount.ActorURI) {
			return fmt.Errorf("%w: %s movedTo=%q target=%q", ErrOldMovedToMismatch, oldAccount.ActorURI, oldAccount.MovedToURI, newAccount.ActorURI)
		}
	}
	return nil
}

func SameActorURI(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	au, aerr := url.Parse(a)
	bu, berr := url.Parse(b)
	if aerr != nil || berr != nil {
		return strings.EqualFold(a, b)
	}
	return strings.EqualFold(au.Scheme, bu.Scheme) &&
		strings.EqualFold(au.Host, bu.Host) &&
		au.EscapedPath() == bu.EscapedPath() &&
		au.RawQuery == bu.RawQuery &&
		au.Fragment == bu.Fragment
}

package oauth

import (
	"sort"
	"strings"
)

// scopeChildren maps every broad Mastodon scope to the granular scopes it
// covers. Clients register a coarse scope such as "read write follow push" and
// then request granular ones ("read:accounts", "write:statuses"), so a plain
// token-for-token comparison rejects most modern clients.
var scopeChildren = map[string][]string{
	"read": {
		"profile", "read:accounts", "read:blocks", "read:bookmarks", "read:favourites",
		"read:filters", "read:follows", "read:lists", "read:mutes", "read:notifications",
		"read:search", "read:statuses",
	},
	"write": {
		"write:accounts", "write:blocks", "write:bookmarks", "write:conversations",
		"write:favourites", "write:filters", "write:follows", "write:lists", "write:media",
		"write:mutes", "write:notifications", "write:reports", "write:statuses",
	},
	"follow": {
		"read:blocks", "read:follows", "read:mutes",
		"write:blocks", "write:follows", "write:mutes",
	},
	"admin:read": {
		"admin:read:accounts", "admin:read:canonical_email_blocks", "admin:read:domain_allows",
		"admin:read:domain_blocks", "admin:read:email_domain_blocks", "admin:read:ip_blocks",
		"admin:read:reports",
	},
	"admin:write": {
		"admin:write:accounts", "admin:write:canonical_email_blocks", "admin:write:domain_allows",
		"admin:write:domain_blocks", "admin:write:email_domain_blocks", "admin:write:ip_blocks",
		"admin:write:reports",
	},
}

// coveredScopes returns every scope granted by the registered scope list: the
// scopes themselves plus everything they imply.
func coveredScopes(registered string) map[string]struct{} {
	covered := make(map[string]struct{})
	for _, scope := range strings.Fields(registered) {
		covered[scope] = struct{}{}
		for _, child := range scopeChildren[scope] {
			covered[child] = struct{}{}
		}
	}
	return covered
}

// scopesAllowed reports whether every requested scope is covered by the
// application's registration. A broad scope is also satisfied when all of the
// granular scopes it implies were registered, so an app holding
// "read write" may be authorized for "follow".
func scopesAllowed(registered string, requested []string) bool {
	if len(requested) == 0 {
		return false
	}
	covered := coveredScopes(registered)
	for _, scope := range requested {
		if _, ok := covered[scope]; ok {
			continue
		}
		children := scopeChildren[scope]
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if _, ok := covered[child]; !ok {
				return false
			}
		}
	}
	return true
}

// supportedScopes lists every scope the server understands, for the
// authorization server metadata document.
func supportedScopes() []string {
	scopes := []string{"push"}
	for parent, children := range scopeChildren {
		scopes = append(scopes, parent)
		scopes = append(scopes, children...)
	}
	seen := make(map[string]struct{}, len(scopes))
	unique := scopes[:0]
	for _, scope := range scopes {
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		unique = append(unique, scope)
	}
	sort.Strings(unique)
	return unique
}

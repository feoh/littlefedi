package federation

import "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"

// publicURI is the ActivityStreams magic collection denoting public addressing.
const publicURI = "https://www.w3.org/ns/activitystreams#Public"

// FEP-044f / interop term URIs used in the JSON-LD @context for quote posts.
const (
	fep044fQuote     = "https://w3id.org/fep/044f#quote"
	fep044fQuoteAuth = "https://w3id.org/fep/044f#quoteAuthorization"
	fedibirdQuoteUri = "http://fedibird.com/ns#quoteUri"
	misskeyQuote     = "https://misskey-hub.net/ns#_misskey_quote"
	gtsNS            = "https://gotosocial.org/ns#"
)

// QuoteJSONLDContext returns the @context value to use on a Note (or Create)
// that carries quote-post properties. It augments the AS2 namespace with the
// FEP-044f / Fedibird / Misskey / GoToSocial terms so the extra properties are
// understood as IRIs by conforming consumers (Mastodon, GoToSocial, Misskey).
func QuoteJSONLDContext() []interface{} {
	return []interface{}{
		"https://www.w3.org/ns/activitystreams",
		map[string]interface{}{
			"QuoteRequest":       "https://w3id.org/fep/044f#QuoteRequest",
			"QuoteAuthorization": "https://w3id.org/fep/044f#QuoteAuthorization",
			"quote":              map[string]interface{}{"@id": fep044fQuote, "@type": "@id"},
			"quoteUri":           map[string]interface{}{"@id": fedibirdQuoteUri, "@type": "@id"},
			"_misskey_quote":     map[string]interface{}{"@id": misskeyQuote, "@type": "@id"},
			"quoteAuthorization": map[string]interface{}{"@id": fep044fQuoteAuth, "@type": "@id"},
			"gts":                gtsNS,
			"interactingObject":  map[string]interface{}{"@id": "gts:interactingObject", "@type": "@id"},
			"interactionTarget":  map[string]interface{}{"@id": "gts:interactionTarget", "@type": "@id"},
			"interactionPolicy":  map[string]interface{}{"@id": "gts:interactionPolicy", "@type": "@id"},
			"canQuote":           map[string]interface{}{"@id": "gts:canQuote", "@type": "@id"},
			"automaticApproval":  map[string]interface{}{"@id": "gts:automaticApproval", "@type": "@id"},
			"manualApproval":     map[string]interface{}{"@id": "gts:manualApproval", "@type": "@id"},
		},
	}
}

// ApplyQuoteFields merges the quote-post properties (FEP-044f plus Fedibird and
// Misskey aliases for interop) into an AS2 Note object and upgrades its
// @context. quoteURI is the URI of the quoted object; approvalURI, when
// non-empty, is the QuoteAuthorization granted by the quoted author's server.
func ApplyQuoteFields(note map[string]interface{}, quoteURI string, approvalURI *string) {
	if quoteURI == "" {
		return
	}
	note["@context"] = QuoteJSONLDContext()
	note["quote"] = quoteURI
	note["quoteUri"] = quoteURI
	note["_misskey_quote"] = quoteURI
	if approvalURI != nil && *approvalURI != "" {
		note["quoteAuthorization"] = *approvalURI
	}
}

// buildQuoteAuthorizationAS2 renders a QuoteAuthorization (FEP-044f) object: the
// grant proving the quoted author permitted quotingURI to quote quotedURI.
func buildQuoteAuthorizationAS2(authURI, authorURI, quotingURI, quotedURI string) map[string]interface{} {
	return map[string]interface{}{
		"@context":          QuoteJSONLDContext(),
		"type":              "QuoteAuthorization",
		"id":                authURI,
		"attributedTo":      authorURI,
		"interactingObject": quotingURI,
		"interactionTarget": quotedURI,
	}
}

// InteractionPolicyFor builds the GoToSocial-style interactionPolicy advertising
// who may quote this account's posts, derived from the author's QuotePolicy.
func InteractionPolicyFor(account *models.Account) map[string]interface{} {
	var automatic []string
	switch account.QuotePolicy {
	case "nobody":
		automatic = []string{account.ActorURI}
	case "followers":
		automatic = []string{account.ActorURI, account.FollowersURI}
	default: // "everyone"
		automatic = []string{publicURI}
	}
	return map[string]interface{}{
		"canQuote": map[string]interface{}{
			"automaticApproval": automatic,
			"manualApproval":    []string{},
		},
	}
}

// QuotePolicyAllows reports whether a quote of a status with the given
// visibility, authored by an account with the given quote policy, should be
// auto-approved for a requester. requesterFollows indicates whether the
// requesting actor follows the author (only consulted for the "followers"
// policy).
func QuotePolicyAllows(policy string, vis models.Visibility, requesterFollows bool) bool {
	// Never authorize quotes of non-public posts.
	if vis != models.VisibilityPublic && vis != models.VisibilityUnlisted {
		return false
	}
	switch policy {
	case "nobody":
		return false
	case "followers":
		return requesterFollows
	default: // "everyone" (and any unset/legacy value)
		return true
	}
}

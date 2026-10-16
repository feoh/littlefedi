package federation

import (
	"context"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/storage"
)

// hostInboundBlocked reports whether inbound federation traffic from a remote
// host must be refused outright: either the instance is suspended or there is a
// suspend-severity domain block. Silence-severity blocks are not refused here -
// they are enforced at read time by the timeline queries - so silenced content
// is still accepted and stored.
func hostInboundBlocked(ctx context.Context, store storage.Storage, host string, localDomains ...string) bool {
	if host == "" {
		return false
	}
	for _, local := range localDomains {
		if strings.EqualFold(host, local) {
			return false
		}
	}
	allowed, reason, err := store.FederationDomainAllowed(ctx, host)
	if err != nil {
		reason = "policy_unavailable"
	}
	if err != nil || !allowed {
		metrics.IncrementFederationPolicyRejected("inbound", reason)
		return true
	}
	return false
}

// hostDeliveryBlocked reports whether outbound delivery to a remote host must be
// skipped because we have suspended that domain or instance. We never push our
// content to instances we have explicitly suspended.
func hostDeliveryBlocked(ctx context.Context, store storage.Storage, host string) bool {
	allowed, reason, err := store.FederationDomainAllowed(ctx, host)
	if err != nil {
		reason = "policy_unavailable"
	}
	if err != nil || !allowed {
		metrics.IncrementFederationPolicyRejected("outbound", reason)
		return true
	}
	return false
}

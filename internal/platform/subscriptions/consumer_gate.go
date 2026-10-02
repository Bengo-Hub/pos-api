package subscriptions

import (
	"context"
	sharedcache "github.com/Bengo-Hub/cache"
	"time"
)

// entitlementCacheTTL bounds how long a tenant's entitlement snapshot is reused by
// event consumers. 60s absorbs event bursts (e.g. stock storms) without hammering
// subscriptions-api, while staying fresh enough that a plan change takes effect quickly.
const entitlementCacheTTL = 60 * time.Second

// entCache is per replica, bounded and expiring (one entry per tenant, entitlementCacheTTL).
var entCache = sharedcache.NewLocal[string, *Entitlements](10000, entitlementCacheTTL)

// ConsumerHasFeature reports whether a tenant is entitled to featureCode, for use by
// NATS event consumers that have a tenant_id but no user JWT. It mirrors the HTTP-layer
// gating contract (authclient.IsGatingExempt):
//
//   - Demo-bypass and service-charge (PAYG) tenants are always allowed.
//   - Otherwise the feature must be present in the tenant's entitlement snapshot.
//
// It FAILS OPEN (returns true) when subscriptions-api is unreachable or the tenant has
// no snapshot, so a subscriptions-api outage never silently drops legitimate data sync.
// Results are cached per tenant for entitlementCacheTTL.
func (c *Client) ConsumerHasFeature(ctx context.Context, tenantID, featureCode string) bool {
	if c == nil || tenantID == "" {
		return true // not wired → fail open
	}
	e := c.cachedEntitlements(ctx, tenantID)
	if e == nil {
		return true // lookup failed → fail open
	}
	if e.IsDemoBypass || e.BillingMode == "service_charge" {
		return true // exempt — mirror IsGatingExempt
	}
	for _, f := range e.Features {
		if f == featureCode {
			return true
		}
	}
	return false
}

func (c *Client) cachedEntitlements(ctx context.Context, tenantID string) *Entitlements {
	if hit, ok := entCache.Get(tenantID); ok {
		return hit
	}
	e := c.GetEntitlements(ctx, tenantID)
	if e == nil {
		return nil // do not cache failures: retry next event
	}
	entCache.Set(tenantID, e)
	return e
}

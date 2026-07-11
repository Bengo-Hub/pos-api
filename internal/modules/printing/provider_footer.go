package printing

import (
	"context"
	"strings"

	sharedcache "github.com/Bengo-Hub/cache"
)

// The platform-owner (SaaS provider) advertisement footer shown at the bottom of EVERY
// customer-facing receipt/bill. It advertises Codevertex — the SaaS provider that develops and
// maintains the platform — using the provider's public contact details sourced from the auth-api
// public tenant-by-slug record for the platform-owner tenant ("codevertex"), cached via the shared
// tenant cache. Static constants below are the fallback when auth-api is unreachable; they mirror
// the codevertex tenant's public record so the advertisement is always accurate.
const (
	// ProviderSlug is the platform-owner tenant slug in auth-api (see architecture: is_platform_owner).
	ProviderSlug = "codevertex"

	providerDefaultName  = "Codevertex Africa Limited"
	providerDefaultWeb   = "www.codevertexitsolutions.com"
	providerDefaultEmail = "info@codevertexitsolutions.com"
	providerDefaultPhone = "+254 742 201 368"
)

// ProviderFooter is the resolved two-line platform-owner advertisement:
//
//	Developed & maintained by Codevertex Africa Limited
//	www.codevertexitsolutions.com  ·  info@codevertexitsolutions.com  ·  +254 742 201 368
type ProviderFooter struct {
	Lead    string // "Developed & maintained by <provider name>"
	Contact string // "<website>  ·  <email>  ·  <phone>"
}

// DefaultProviderFooter returns the static advertisement (used as a fallback and by pure renderers
// that cannot reach the tenant cache).
func DefaultProviderFooter() ProviderFooter {
	return ProviderFooter{
		Lead:    "Developed & maintained by " + providerDefaultName,
		Contact: providerDefaultWeb + "  ·  " + providerDefaultEmail + "  ·  " + providerDefaultPhone,
	}
}

// OrDefault returns the footer, substituting the static default for any empty line so a
// partially-populated (or zero) value never prints a blank advertisement.
func (f ProviderFooter) OrDefault() ProviderFooter {
	d := DefaultProviderFooter()
	if strings.TrimSpace(f.Lead) == "" {
		f.Lead = d.Lead
	}
	if strings.TrimSpace(f.Contact) == "" {
		f.Contact = d.Contact
	}
	return f
}

// ResolveProviderFooter builds the advertisement from the platform-owner (codevertex) record.
// Resolution order is fastest-first so this stays cheap on the receipt hot path:
//  1. Shared tenant cache (Redis, 6h TTL) — GetTenantDetails is a read-through cache, so this is a
//     sub-millisecond Redis read on the common path and only calls auth-api on a cold miss.
//  2. auth-api public tenant-by-slug (only on that cold miss; the result is then cached).
//  3. Static defaults (last resort, and per-field for any value the record leaves blank).
//
// It is best-effort and never errors — a document must always be able to render its footer.
func ResolveProviderFooter(ctx context.Context, c *sharedcache.Aside, authURL string) ProviderFooter {
	f := DefaultProviderFooter()
	if c == nil || authURL == "" {
		return f
	}
	td, err := sharedcache.GetTenantDetails(ctx, c, authURL, ProviderSlug, sharedcache.DefaultTenantTTL)
	if err != nil {
		return f
	}
	name := firstNonEmpty(strings.TrimSpace(td.Name), providerDefaultName)
	web := firstNonEmpty(cleanWebDisplay(td.Website), providerDefaultWeb)
	email := firstNonEmpty(strings.TrimSpace(td.ContactEmail), providerDefaultEmail)
	phone := firstNonEmpty(strings.TrimSpace(td.ContactPhone), providerDefaultPhone)
	f.Lead = "Developed & maintained by " + name
	f.Contact = web + "  ·  " + email + "  ·  " + phone
	return f
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// cleanWebDisplay strips the scheme + trailing slash for a compact printed website (e.g.
// "https://codevertexitsolutions.com/" → "codevertexitsolutions.com").
func cleanWebDisplay(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimRight(s, "/")
}

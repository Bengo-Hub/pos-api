// Package payref builds service-identifiable payment references in the canonical Codevertex form
// {SVC}-{SLUG6}-{ENTITY12}, mirroring the ISP-billing convention (HS-{slug}-{hex}). The reference is
// what treasury forwards to Paystack as the transaction reference, so it must identify the
// originating service and tenant at a glance on the Paystack dashboard. The suffix is DERIVED from
// the entity UUID (not random) so the reference is deterministic per (service, tenant, entity): a
// retried payment produces the SAME reference_id and treasury's dedup prevents duplicate intents.
package payref

import (
	"strings"

	"github.com/google/uuid"
)

// Build returns "{SVC}-{SLUG6}-{ENTITY12}", e.g. "POS-URBANL-B2B592518E5D".
// svc is the short service/channel code (e.g. "POS", "ORD", "SUB"). tenantSlug falls back to the
// tenant UUID when empty. entityID is the originating record's id (order/return/checkout).
func Build(svc, tenantSlug string, tenantID, entityID uuid.UUID) string {
	return strings.ToUpper(svc) + "-" + slugSeg(tenantSlug, tenantID) + "-" + entitySeg(entityID)
}

func slugSeg(slug string, tenantID uuid.UUID) string {
	s := keepAlnum(strings.ToUpper(slug))
	if s == "" {
		s = strings.ToUpper(strings.ReplaceAll(tenantID.String(), "-", ""))
	}
	if len(s) > 6 {
		s = s[:6]
	}
	return s
}

func entitySeg(entityID uuid.UUID) string {
	h := strings.ToUpper(strings.ReplaceAll(entityID.String(), "-", ""))
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

func keepAlnum(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

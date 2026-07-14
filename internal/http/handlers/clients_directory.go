package handlers

import (
	"net/http"
	"strings"

	"github.com/Bengo-Hub/pagination"
	"github.com/google/uuid"

	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
)

// ListCustomers handles GET /{tenantID}/pos/customers?q=&page=&limit=
//
// The tenant's customer DIRECTORY, backed by the MarketFlow CRM (the customer master) so it
// includes every known customer — not only the ones with a loyalty account (e.g. a freshly
// migrated legacy base). q filters by name/email/phone substring; empty q lists everyone,
// newest first, paginated. Each row is enriched with the customer's loyalty account (points)
// when one exists, so the Clients page can show membership at a glance.
func (h *ClientHandler) ListCustomers(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	if h.marketflow == nil || !h.marketflow.Enabled() {
		jsonError(w, "customer directory unavailable (CRM not configured)", http.StatusServiceUnavailable)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// Phone-shaped queries match stored formats via the national subscriber digits (last 9).
	if q != "" && strings.IndexFunc(q, func(c rune) bool { return c >= '0' && c <= '9' }) == 0 {
		if d := nationalSubscriberDigits(q); len(d) >= 7 {
			q = d
		}
	}

	p := pagination.Parse(r)
	contacts, total := h.marketflow.ListContacts(r.Context(), tid, q, p.Limit, p.Offset)

	// Loyalty enrichment — one query for the whole page, matched by crm_contact_id OR phone.
	crmIDs := make([]uuid.UUID, 0, len(contacts))
	phones := make([]string, 0, len(contacts))
	for _, c := range contacts {
		if id, parseErr := uuid.Parse(c.ID); parseErr == nil {
			crmIDs = append(crmIDs, id)
		}
		if c.Phone != "" {
			phones = append(phones, c.Phone)
		}
	}
	type loyaltyLite struct {
		id       uuid.UUID
		points   int
		lifetime int
	}
	byCrm := map[uuid.UUID]loyaltyLite{}
	byPhone := map[string]loyaltyLite{}
	if len(crmIDs) > 0 || len(phones) > 0 {
		preds := entla.TenantID(tid)
		accs, qErr := h.db.LoyaltyAccount.Query().
			Where(preds, entla.Or(entla.CrmContactIDIn(crmIDs...), entla.CustomerPhoneIn(phones...))).
			All(r.Context())
		if qErr == nil {
			for _, a := range accs {
				l := loyaltyLite{id: a.ID, points: a.PointsBalance, lifetime: a.LifetimePoints}
				if a.CrmContactID != nil {
					byCrm[*a.CrmContactID] = l
				}
				if d := nationalSubscriberDigits(a.CustomerPhone); d != "" {
					byPhone[d] = l
				}
			}
		}
	}

	out := make([]map[string]any, 0, len(contacts))
	for _, c := range contacts {
		name := strings.TrimSpace(c.FirstName + " " + c.LastName)
		row := map[string]any{
			"id":              "",
			"tenant_id":       tid,
			"customer_name":   name,
			"customer_phone":  c.Phone,
			"customer_email":  c.Email,
			"crm_contact_id":  c.ID,
			"points_balance":  0,
			"lifetime_points": 0,
			"source":          "crm",
			"created_at":      c.CreatedAt,
		}
		var hit *loyaltyLite
		if id, parseErr := uuid.Parse(c.ID); parseErr == nil {
			if l, ok := byCrm[id]; ok {
				hit = &l
			}
		}
		if hit == nil {
			if d := nationalSubscriberDigits(c.Phone); d != "" {
				if l, ok := byPhone[d]; ok {
					hit = &l
				}
			}
		}
		if hit != nil {
			row["id"] = hit.id
			row["points_balance"] = hit.points
			row["lifetime_points"] = hit.lifetime
			row["source"] = "loyalty"
		}
		out = append(out, row)
	}

	jsonOK(w, pagination.NewResponse(out, total, p))
}

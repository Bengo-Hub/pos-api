package customers

import (
	"context"
	"fmt"
	"strings"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/Bengo-Hub/httpware/contact"
	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"

	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
	entlp "github.com/bengobox/pos-service/internal/ent/layawayplan"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// ErasedName replaces the customer name on a forgotten person's layaway plans, where a name is
// required. It matches the name marketflow gives an erased contact.
const ErasedName = "Erased"

// IsErasedContact reports whether the CRM contact was stripped by an erasure: marketflow sets
// the first name to "Erased" and removes the last name, phone and email. The erasure marker
// marketflow keeps in the contact metadata is not part of the contact the CRM client returns,
// so this is the check pos can make.
func IsErasedContact(c crm.Contact) bool {
	return strings.TrimSpace(c.FirstName) == ErasedName && strings.TrimSpace(c.LastName) == "" &&
		strings.TrimSpace(c.Phone) == "" && strings.TrimSpace(c.Email) == ""
}

// ForgetSales blanks the customer name and phone on the tenant's sales and layaway plans that
// belong to a CRM contact that was deleted or erased. The person is found through the loyalty
// accounts (merged shells included) and client records linked to the contact: their phones in
// every stored spelling find the orders, and the accounts also find the layaway plans opened on
// them. Orders lose customer_name and customer_phone; layaway plans get the name "Erased" and
// an empty phone and email. Totals, lines, payments and every id stay.
//
// Call it before Unlink or a snapshot refresh, which clear or rewrite the links and phones it
// reads. It is idempotent, and each table gets one update. Logs carry ids and counts only.
func (r *Resolver) ForgetSales(ctx context.Context, tenantID, contactID uuid.UUID) error {
	if r == nil || r.db == nil || tenantID == uuid.Nil || contactID == uuid.Nil {
		return nil
	}
	accountIDs, phones, err := r.contactAccounts(ctx, tenantID, contactID)
	if err != nil {
		return err
	}
	clientPhones, err := r.db.ClientRecord.Query().
		Where(entclient.TenantID(tenantID), entclient.CrmContactID(contactID)).
		Select(entclient.FieldPhone).
		Strings(ctx)
	if err != nil {
		return fmt.Errorf("load client records: %w", err)
	}
	phones = append(phones, clientPhones...)

	spellings, norms := phoneSpellings(phones)
	orders, err := r.forgetOrders(ctx, tenantID, spellings, norms)
	if err != nil {
		return err
	}
	plans, err := r.forgetLayaways(ctx, tenantID, accountIDs, spellings)
	if err != nil {
		return err
	}
	if orders > 0 || plans > 0 {
		r.log.Info("customer details removed from past sales",
			zap.String("tenant_id", tenantID.String()), zap.String("crm_contact_id", contactID.String()),
			zap.Int("orders", orders), zap.Int("layaway_plans", plans))
	}
	return nil
}

// contactAccounts returns the ids and phones of the tenant's loyalty accounts linked to the
// contact, plus the merged shells of those accounts (followed up to maxMergeHops) and shells
// that were linked to the contact before a merge.
func (r *Resolver) contactAccounts(ctx context.Context, tenantID, contactID uuid.UUID) ([]uuid.UUID, []string, error) {
	rows, err := r.db.LoyaltyAccount.Query().
		Where(entla.TenantID(tenantID), entla.Or(entla.CrmContactID(contactID), mergedFromContact(contactID))).
		Select(entla.FieldID, entla.FieldCustomerPhone).
		All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load loyalty accounts: %w", err)
	}
	seen := make(map[uuid.UUID]bool, len(rows))
	var ids, frontier []uuid.UUID
	var phones []string
	for _, a := range rows {
		seen[a.ID] = true
		ids = append(ids, a.ID)
		frontier = append(frontier, a.ID)
		phones = append(phones, a.CustomerPhone)
	}
	for hop := 0; hop < maxMergeHops && len(frontier) > 0; hop++ {
		shells, err := r.db.LoyaltyAccount.Query().
			Where(entla.TenantID(tenantID), mergedIntoAny(frontier)).
			Select(entla.FieldID, entla.FieldCustomerPhone).
			All(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("load merged shells: %w", err)
		}
		frontier = nil
		for _, s := range shells {
			if seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			ids = append(ids, s.ID)
			frontier = append(frontier, s.ID)
			phones = append(phones, s.CustomerPhone)
		}
	}
	return ids, phones, nil
}

// mergedFromContact matches shells whose metadata keeps the contact as merged_crm_contact_id.
func mergedFromContact(contactID uuid.UUID) predicate.LoyaltyAccount {
	return predicate.LoyaltyAccount(func(s *sql.Selector) {
		s.Where(sqljson.ValueEQ(s.C(entla.FieldMetadata), contactID.String(), sqljson.Path(MergedCrmContactKey)))
	})
}

// phoneSpellings returns every common spelling of the phones (see lookupSpellings) and the
// E.164 forms of those that normalise.
func phoneSpellings(phones []string) ([]string, map[string]bool) {
	var all []string
	norms := map[string]bool{}
	for _, p := range phones {
		raw := strings.TrimSpace(p)
		if raw == "" {
			continue
		}
		norm, sp, ok := lookupSpellings(raw)
		all = append(all, sp...)
		if ok {
			norms[norm] = true
		}
	}
	return dedupe(all), norms
}

// forgetOrders clears customer_name and customer_phone on the tenant's orders stored under any
// of the spellings. Orders stored in another spelling that still ends in the nine subscriber
// digits (such as "254 712345678") are found by that suffix and kept only when they normalise to
// one of norms, so a foreign number with the same digits is never touched.
func (r *Resolver) forgetOrders(ctx context.Context, tenantID uuid.UUID, spellings []string, norms map[string]bool) (int, error) {
	if len(spellings) == 0 {
		return 0, nil
	}
	var oddIDs []uuid.UUID
	subs := map[string]bool{}
	for norm := range norms {
		sub := contact.SubscriberDigits(norm)
		if sub == "" || subs[sub] {
			continue
		}
		subs[sub] = true
		cands, err := r.db.POSOrder.Query().
			Where(entorder.TenantID(tenantID), entorder.CustomerPhoneHasSuffix(sub),
				entorder.CustomerPhoneNotIn(spellings...)).
			Select(entorder.FieldID, entorder.FieldCustomerPhone).
			All(ctx)
		if err != nil {
			return 0, fmt.Errorf("find orders by phone: %w", err)
		}
		for _, o := range cands {
			if o.CustomerPhone != nil && norms[NormalisePhone(*o.CustomerPhone)] {
				oddIDs = append(oddIDs, o.ID)
			}
		}
	}
	match := entorder.CustomerPhoneIn(spellings...)
	if len(oddIDs) > 0 {
		match = entorder.Or(match, entorder.IDIn(oddIDs...))
	}
	n, err := r.db.POSOrder.Update().
		Where(entorder.TenantID(tenantID), match).
		ClearCustomerName().ClearCustomerPhone().
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("clear customer on orders: %w", err)
	}
	return n, nil
}

// forgetLayaways sets the name to ErasedName and empties the phone and email on the tenant's
// layaway plans opened on the accounts or stored under any of the spellings. Plans already
// blanked are left alone.
func (r *Resolver) forgetLayaways(ctx context.Context, tenantID uuid.UUID, accountIDs []uuid.UUID, spellings []string) (int, error) {
	var or []predicate.LayawayPlan
	if len(accountIDs) > 0 {
		or = append(or, entlp.LoyaltyAccountIDIn(accountIDs...))
	}
	if len(spellings) > 0 {
		or = append(or, entlp.CustomerPhoneIn(spellings...))
	}
	if len(or) == 0 {
		return 0, nil
	}
	n, err := r.db.LayawayPlan.Update().
		Where(entlp.TenantID(tenantID), entlp.Or(or...), entlp.Or(
			entlp.CustomerNameNEQ(ErasedName), entlp.CustomerPhoneNEQ(""), entlp.CustomerEmailNEQ(""),
		)).
		SetCustomerName(ErasedName).SetCustomerPhone("").SetCustomerEmail("").
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("clear customer on layaway plans: %w", err)
	}
	return n, nil
}

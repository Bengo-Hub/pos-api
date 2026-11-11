// Package customers links pos-api's customer records to the MarketFlow CRM, which owns customer
// identity. Loyalty accounts and client records keep the CRM contact id plus a read-only
// snapshot of name, phone and email that is refreshed from the CRM.
package customers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Bengo-Hub/httpware/contact"
	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
)

// linkBudget bounds the CRM upsert done while a customer is being created, so a slow or down
// CRM never holds up the till for long.
const linkBudget = 2 * time.Second

// Identity is a customer as the CRM knows it. Phone is E.164 when it could be normalised.
// CrmContactID is nil when the CRM could not be reached; the reconciler links it later.
type Identity struct {
	CrmContactID *uuid.UUID
	Phone        string
	Name         string
	Email        string
}

// Resolver links local customer records to CRM contacts and finds loyalty accounts by phone.
// Every method is safe on a nil *Resolver: Link then returns the local identity unlinked.
type Resolver struct {
	db  *ent.Client
	crm *crm.Client
	log *zap.Logger
}

// NewResolver builds a resolver. A nil or unconfigured crm client means nothing is linked.
func NewResolver(db *ent.Client, crmClient *crm.Client, log *zap.Logger) *Resolver {
	if log == nil {
		log = zap.NewNop()
	}
	named := log.Named("customers")
	lookupLog.Store(named)
	return &Resolver{db: db, crm: crmClient, log: named}
}

// NormalisePhone returns the phone in E.164, or the trimmed input when it cannot be parsed.
func NormalisePhone(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if n, err := contact.NormalizePhone(raw, contact.DefaultRegion); err == nil {
		return n
	}
	return raw
}

// RejectionMessage reports whether err is the CRM refusing the input and, if so, the CRM's
// message. Callers answer 400 with that message. Only 400 and 422 count: marketflow's upsert
// never answers 409, and 401, 403, 408, 429 and the rest mean the CRM could not be used, which
// Link treats as an outage.
func RejectionMessage(err error) (string, bool) {
	var ce *crm.Error
	if errors.As(err, &ce) && (ce.Status == http.StatusBadRequest || ce.Status == http.StatusUnprocessableEntity) {
		msg := ce.Message
		if msg == "" {
			msg = ce.Error()
		}
		return msg, true
	}
	return "", false
}

// Link creates or finds the CRM contact for a customer and returns the CRM's view of it.
// A CRM rejection (4xx) is returned as the error. Any other failure, such as an outage or a
// timeout, returns the local identity with a nil CrmContactID and no error, so the caller can
// still save its record.
func (r *Resolver) Link(ctx context.Context, tenantID uuid.UUID, phone, email, name string) (Identity, error) {
	local := Identity{
		Phone: NormalisePhone(phone),
		Name:  strings.TrimSpace(name),
		Email: strings.TrimSpace(email),
	}
	if r == nil || r.crm == nil {
		return local, nil
	}
	if tenantID == uuid.Nil {
		return local, crm.ErrNoTenant
	}

	first, last := crm.SplitName(local.Name)
	callCtx, cancel := context.WithTimeout(ctx, linkBudget)
	defer cancel()
	got, err := r.crm.UpsertStrict(callCtx, crm.UpsertInput{
		TenantID:  tenantID,
		Phone:     local.Phone,
		Email:     local.Email,
		FirstName: first,
		LastName:  last,
	})
	if err != nil {
		if _, rejected := RejectionMessage(err); rejected {
			return local, err
		}
		var ce *crm.Error
		switch {
		case errors.Is(err, crm.ErrDisabled), errors.Is(err, crm.ErrNoIdentifier):
		case errors.As(err, &ce) && (ce.Status == http.StatusUnauthorized || ce.Status == http.StatusForbidden):
			// A refused service key is a configuration fault, not a passing outage.
			r.log.Error("crm refused the service key, saving unlinked for the reconciler",
				zap.String("tenant_id", tenantID.String()), zap.Int("status", ce.Status))
		default:
			r.log.Warn("crm link failed, saving unlinked for the reconciler",
				zap.String("tenant_id", tenantID.String()), zap.Error(err))
		}
		return local, nil
	}
	if got.ID == uuid.Nil {
		return local, nil
	}
	return mergeIdentity(local, got), nil
}

// mergeIdentity takes the CRM's answer as the snapshot and falls back to the local value for any
// field the CRM left empty, so required columns are never blanked.
func mergeIdentity(local Identity, c crm.Contact) Identity {
	id := c.ID
	out := Identity{CrmContactID: &id, Phone: local.Phone, Name: local.Name, Email: local.Email}
	if p := NormalisePhone(c.Phone); p != "" {
		out.Phone = p
	}
	if n := strings.TrimSpace(c.FirstName + " " + c.LastName); n != "" {
		out.Name = n
	}
	if e := strings.TrimSpace(c.Email); e != "" {
		out.Email = e
	}
	return out
}

// Search returns CRM contacts matching q for the customer picker. It never fails: any error,
// or a query shorter than two characters, gives no results.
func (r *Resolver) Search(ctx context.Context, tenantID uuid.UUID, q string, limit int) []crm.Contact {
	if r == nil || r.crm == nil || len(strings.TrimSpace(q)) < 2 {
		return nil
	}
	out, err := r.crm.Search(ctx, tenantID, q, limit)
	if err != nil {
		if !errors.Is(err, crm.ErrDisabled) {
			r.log.Warn("crm search failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		}
		return nil
	}
	return out
}

// FindAccount returns the tenant's loyalty account for a phone, or nil when there is none.
// "0712..." and "+254712..." find the same account; see FindAccountIn for the exact rules.
func (r *Resolver) FindAccount(ctx context.Context, tenantID uuid.UUID, phone string) (*ent.LoyaltyAccount, error) {
	if r == nil {
		return nil, nil
	}
	return FindAccountIn(ctx, r.db, tenantID, phone)
}

// suffixCandidates bounds the rows read when matching other spellings of a number.
const suffixCandidates = 10

// lookupLog receives the ambiguous-match warnings of the package-level finders. NewResolver
// points it at the service logger.
var lookupLog atomic.Pointer[zap.Logger]

func ambiguityLog() *zap.Logger {
	if l := lookupLog.Load(); l != nil {
		return l
	}
	return zap.NewNop()
}

// FindAccountIn is FindAccount for callers that hold an ent client but no Resolver.
//
// The lookup runs in two steps:
//  1. one indexed query over every common spelling of the number (see lookupSpellings), so
//     "0712...", "254712...", "712..." and "+254712..." rows are all found together;
//  2. only when step 1 finds nothing, up to ten rows ending in the same nine subscriber digits.
//
// Every row found is kept only when its stored phone normalises to the same E.164 number, so a
// +256 number never matches a Kenyan account. An input that does not normalise is matched
// exactly only.
//
// Rows found are the same number, so the same person. When there are several, one is picked
// with pickAccount and never nil, so callers never create another account beside them. Every
// spelling of the number gets the same row: the busiest of the rows stored in a common
// spelling (with no contact id given). A row stored in an unusual spelling, such as one with
// spaces, is seen only by the suffix step, so it is not considered while a common spelling
// exists.
func FindAccountIn(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string) (*ent.LoyaltyAccount, error) {
	return FindAccountForContact(ctx, db, tenantID, phone, nil)
}

// FindAccountForContact is FindAccountIn for a caller that knows the customer's CRM contact
// id: when the number is stored in several spellings, the account linked to that contact wins.
// A nil contactID behaves exactly like FindAccountIn.
func FindAccountForContact(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string, contactID *uuid.UUID) (*ent.LoyaltyAccount, error) {
	raw := strings.TrimSpace(phone)
	if db == nil || raw == "" {
		return nil, nil
	}
	norm, spellings, ok := lookupSpellings(raw)
	rows, err := db.LoyaltyAccount.Query().
		Where(entla.TenantID(tenantID), entla.CustomerPhoneIn(spellings...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return pickLive(ctx, db, tenantID, rows, contactID)
	}
	hits := make([]*ent.LoyaltyAccount, 0, len(rows))
	for _, a := range rows {
		if NormalisePhone(a.CustomerPhone) == norm {
			hits = append(hits, a)
		}
	}
	if len(hits) > 0 {
		return pickLive(ctx, db, tenantID, hits, contactID)
	}
	cands, err := db.LoyaltyAccount.Query().
		Where(entla.TenantID(tenantID), entla.CustomerPhoneHasSuffix(contact.SubscriberDigits(norm))).
		Order(ent.Asc(entla.FieldCreatedAt)).
		Limit(suffixCandidates).
		All(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]*ent.LoyaltyAccount, 0, len(cands))
	for _, c := range cands {
		if NormalisePhone(c.CustomerPhone) == norm {
			matches = append(matches, c)
		}
	}
	return pickLive(ctx, db, tenantID, matches, contactID)
}

// pickLive is pickAccount over rows with merged shells swapped for their survivors (see
// MergeDuplicateAccounts), so points never land on an emptied shell.
func pickLive(ctx context.Context, db *ent.Client, tenantID uuid.UUID, rows []*ent.LoyaltyAccount, contactID *uuid.UUID) (*ent.LoyaltyAccount, error) {
	live, err := followMerged(ctx, db, tenantID, rows)
	if err != nil {
		return nil, err
	}
	return pickAccount(tenantID, live, contactID), nil
}

// lookupSpellings returns the E.164 form of raw, the spellings to match exactly in one indexed
// query, and whether raw normalised. The spellings are the E.164 form and the typed form, plus,
// for a Kenyan (+254) number only, the common legacy forms "0" plus the nine subscriber digits,
// "254" plus the nine digits, and the bare nine digits. Stored local forms are read as Kenyan,
// so other countries' local forms could never match and are not derived.
//
// An input that does not normalise gives only the typed spelling.
func lookupSpellings(raw string) (string, []string, bool) {
	norm, err := contact.NormalizePhone(raw, contact.DefaultRegion)
	if err != nil {
		return "", []string{raw}, false
	}
	out := []string{norm, raw}
	if strings.HasPrefix(norm, "+254") {
		sub := contact.SubscriberDigits(norm)
		out = append(out, "0"+sub, "254"+sub, sub)
	}
	return norm, dedupe(out), true
}

// dedupe drops empty and repeated spellings, keeping the first of each.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// hasLink reports whether a stored contact link is set.
func hasLink(link *uuid.UUID) bool {
	return link != nil && *link != uuid.Nil
}

// pickAccount chooses among accounts that hold the same number: the one linked to contactID
// when given, else the most lifetime points, else any row linked to the CRM, else the oldest,
// then the lowest id. Points come before the link so the pick stays on the same row when the
// reconciler later links a busy legacy row; points earned in between are never stranded.
func pickAccount(tenantID uuid.UUID, rows []*ent.LoyaltyAccount, contactID *uuid.UUID) *ent.LoyaltyAccount {
	best := chooseAccount(rows, contactID)
	if best != nil && len(rows) > 1 {
		others := make([]string, 0, len(rows)-1)
		for _, a := range rows {
			if a.ID != best.ID {
				others = append(others, a.ID.String())
			}
		}
		ambiguityLog().Warn("phone matches several loyalty accounts, picked one",
			zap.String("tenant_id", tenantID.String()), zap.String("account_id", best.ID.String()),
			zap.Strings("other_account_ids", others))
	}
	return best
}

// chooseAccount is pickAccount without the log line. The duplicate account merge uses it so
// the survivor is the row every lookup would have picked.
func chooseAccount(rows []*ent.LoyaltyAccount, contactID *uuid.UUID) *ent.LoyaltyAccount {
	if len(rows) == 0 {
		return nil
	}
	best := rows[0]
	for _, a := range rows[1:] {
		if contactID != nil {
			am, bm := linkedTo(a.CrmContactID, *contactID), linkedTo(best.CrmContactID, *contactID)
			if am != bm {
				if am {
					best = a
				}
				continue
			}
		}
		if a.LifetimePoints != best.LifetimePoints {
			if a.LifetimePoints > best.LifetimePoints {
				best = a
			}
			continue
		}
		if al, bl := hasLink(a.CrmContactID), hasLink(best.CrmContactID); al != bl {
			if al {
				best = a
			}
			continue
		}
		if olderRow(a.CreatedAt, a.ID, best.CreatedAt, best.ID) {
			best = a
		}
	}
	return best
}

// FindClientIn finds the tenant's client record for a phone with the same steps as
// FindAccountIn: one indexed query over every common spelling, then the suffix scan only on a
// miss. Several matches never give nil; see pickClient.
func FindClientIn(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string) (*ent.ClientRecord, error) {
	return FindClientForContact(ctx, db, tenantID, phone, nil)
}

// FindClientForContact is FindClientIn that prefers the record linked to contactID when the
// number is stored in several spellings. A nil contactID behaves exactly like FindClientIn.
func FindClientForContact(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string, contactID *uuid.UUID) (*ent.ClientRecord, error) {
	raw := strings.TrimSpace(phone)
	if db == nil || raw == "" {
		return nil, nil
	}
	norm, spellings, ok := lookupSpellings(raw)
	rows, err := db.ClientRecord.Query().
		Where(entclient.TenantID(tenantID), entclient.PhoneIn(spellings...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return pickClient(tenantID, rows, contactID), nil
	}
	hits := make([]*ent.ClientRecord, 0, len(rows))
	for _, c := range rows {
		if NormalisePhone(c.Phone) == norm {
			hits = append(hits, c)
		}
	}
	if len(hits) > 0 {
		return pickClient(tenantID, hits, contactID), nil
	}
	cands, err := db.ClientRecord.Query().
		Where(entclient.TenantID(tenantID), entclient.PhoneHasSuffix(contact.SubscriberDigits(norm))).
		Order(ent.Asc(entclient.FieldCreatedAt)).
		Limit(suffixCandidates).
		All(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]*ent.ClientRecord, 0, len(cands))
	for _, c := range cands {
		if NormalisePhone(c.Phone) == norm {
			matches = append(matches, c)
		}
	}
	return pickClient(tenantID, matches, contactID), nil
}

// pickClient chooses among client records that hold the same number: the one linked to
// contactID when given, else any record linked to the CRM, else the oldest, then the lowest id.
func pickClient(tenantID uuid.UUID, rows []*ent.ClientRecord, contactID *uuid.UUID) *ent.ClientRecord {
	if len(rows) == 0 {
		return nil
	}
	best := rows[0]
	for _, c := range rows[1:] {
		if contactID != nil {
			cm, bm := linkedTo(c.CrmContactID, *contactID), linkedTo(best.CrmContactID, *contactID)
			if cm != bm {
				if cm {
					best = c
				}
				continue
			}
		}
		if cl, bl := hasLink(c.CrmContactID), hasLink(best.CrmContactID); cl != bl {
			if cl {
				best = c
			}
			continue
		}
		if olderRow(c.CreatedAt, c.ID, best.CreatedAt, best.ID) {
			best = c
		}
	}
	if len(rows) > 1 {
		others := make([]string, 0, len(rows)-1)
		for _, c := range rows {
			if c.ID != best.ID {
				others = append(others, c.ID.String())
			}
		}
		ambiguityLog().Warn("phone matches several client records, picked one",
			zap.String("tenant_id", tenantID.String()), zap.String("client_id", best.ID.String()),
			zap.Strings("other_client_ids", others))
	}
	return best
}

// linkedTo reports whether a stored contact link equals id.
func linkedTo(link *uuid.UUID, id uuid.UUID) bool {
	return link != nil && *link == id
}

// olderRow reports whether row a sorts before row b by created_at, then by id.
func olderRow(aAt time.Time, aID uuid.UUID, bAt time.Time, bID uuid.UUID) bool {
	if !aAt.Equal(bAt) {
		return aAt.Before(bAt)
	}
	return bytes.Compare(aID[:], bID[:]) < 0
}

// FindClientsIn returns every client record of the tenant stored under any spelling of the
// phone, at most limit rows from the suffix scan. Used by the client list filter, which needs
// every spelling, so it always runs the suffix scan.
func FindClientsIn(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string, limit int) ([]*ent.ClientRecord, error) {
	return findClients(ctx, db, tenantID, phone, limit)
}

// findClients collects every client record stored under any spelling of the phone: exact
// spellings first, then other spellings of the same number.
func findClients(ctx context.Context, db *ent.Client, tenantID uuid.UUID, phone string, limit int) ([]*ent.ClientRecord, error) {
	raw := strings.TrimSpace(phone)
	if db == nil || raw == "" {
		return nil, nil
	}
	norm, spellings, ok := lookupSpellings(raw)
	rows, err := db.ClientRecord.Query().
		Where(entclient.TenantID(tenantID), entclient.PhoneIn(spellings...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return rows, nil
	}
	out := make([]*ent.ClientRecord, 0, len(rows))
	seen := make(map[uuid.UUID]bool, len(rows))
	for _, rec := range rows {
		if NormalisePhone(rec.Phone) == norm {
			seen[rec.ID] = true
			out = append(out, rec)
		}
	}
	cands, err := db.ClientRecord.Query().
		Where(entclient.TenantID(tenantID), entclient.PhoneHasSuffix(contact.SubscriberDigits(norm))).
		Order(ent.Asc(entclient.FieldCreatedAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range cands {
		if !seen[c.ID] && NormalisePhone(c.Phone) == norm {
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// SamePhone reports whether two stored spellings are the same number.
func SamePhone(a, b string) bool {
	na, nb := NormalisePhone(a), NormalisePhone(b)
	return na != "" && na == nb
}

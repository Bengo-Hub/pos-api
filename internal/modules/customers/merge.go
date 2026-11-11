package customers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/Bengo-Hub/httpware/contact"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entlayaway "github.com/bengobox/pos-service/internal/ent/layawayplan"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entlt "github.com/bengobox/pos-service/internal/ent/loyaltytransaction"
	entorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entpay "github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	entref "github.com/bengobox/pos-service/internal/ent/referral"
)

// Metadata keys written by the duplicate account merge. A row carrying MergedIntoKey is an
// empty shell: lookups follow it to the survivor and the reconciler never links it again.
const (
	MergedIntoKey   = "merged_into"
	MergedAtKey     = "merged_at"
	MergedReasonKey = "merged_reason"
	// MergedFromKey lists, on the survivor, the ids of the accounts merged into it.
	MergedFromKey = "merged_from"
	// MergedCrmContactKey keeps, on a shell, the CRM contact it was linked to before the merge.
	MergedCrmContactKey = "merged_crm_contact_id"
	// MergedFromPaymentKey keeps, on a repointed loyalty tender payment, the account id it
	// first named.
	MergedFromPaymentKey = "merged_from_loyalty_account_id"
)

// Why a survivor was chosen, in the pick order of pickAccount.
const (
	ReasonMostLifetimePoints = "most lifetime points"
	ReasonLinked             = "linked to a CRM contact"
	ReasonOldest             = "oldest account"
	ReasonLowestID           = "lowest id"
)

// Group outcomes in a merge report.
const (
	MergeStatusWouldMerge = "would_merge"
	MergeStatusMerged     = "merged"
	MergeStatusRefused    = "refused"
)

// mergeReason is stored on every shell.
const mergeReason = "duplicate loyalty account for the same phone number"

// IsMerged reports whether metadata marks the account as a merged shell.
func IsMerged(meta map[string]any) bool {
	v, ok := meta[MergedIntoKey].(string)
	return ok && v != ""
}

// mergedInto returns the survivor id a shell points to.
func mergedInto(meta map[string]any) (uuid.UUID, bool) {
	v, ok := meta[MergedIntoKey].(string)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(v)
	return id, err == nil && id != uuid.Nil
}

// MergeOptions scopes a merge run. The zero value is a dry run over every tenant.
type MergeOptions struct {
	// TenantID limits the run to one tenant; nil means every tenant with a loyalty account.
	TenantID *uuid.UUID
	// Apply makes the changes. When false nothing is written and the report says what would
	// happen.
	Apply bool
}

// MergeAccount is one account of a duplicate group as shown in the report. It carries ids and
// balances only, never the name, phone or email.
type MergeAccount struct {
	ID             uuid.UUID  `json:"id"`
	Points         int        `json:"points"`
	LifetimePoints int        `json:"lifetime_points"`
	CrmContactID   *uuid.UUID `json:"crm_contact_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// MergeGroup is one set of accounts of a tenant whose phones are the same number.
type MergeGroup struct {
	TenantID uuid.UUID `json:"tenant_id"`
	// Phone is the normalised number with every digit but the last three masked.
	Phone      string         `json:"phone"`
	Accounts   []MergeAccount `json:"accounts"`
	SurvivorID uuid.UUID      `json:"survivor_id"`
	Reason     string         `json:"reason"`
	// ContactConflict is true when the accounts are linked to different CRM contacts. The
	// survivor keeps its own link (or adopts the busiest linked one); every other link is kept
	// on its shell as metadata.merged_crm_contact_id, never silently dropped.
	ContactConflict bool   `json:"contact_conflict"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
}

// MergeReport is the outcome of one run.
type MergeReport struct {
	DryRun  bool         `json:"dry_run"`
	Tenants int          `json:"tenants_scanned"`
	Groups  []MergeGroup `json:"groups"`
	Merged  int          `json:"merged"`
	Refused int          `json:"refused"`
}

// MergeDuplicateAccounts finds, per tenant, loyalty accounts whose phones normalise to the
// same E.164 number and merges each group into one survivor chosen with the lookup pick order
// (most lifetime points, then linked to the CRM, then oldest, then lowest id).
//
// With Apply false it only reports. With Apply true each group runs in its own transaction:
// the survivor gains the points and lifetime points of the others, adopts a CRM link when it
// has none, and every loyalty transaction, referral, layaway plan and loyalty tender payment
// that named another account is moved to it. The other accounts are left as empty, unlinked
// shells with merged_into, merged_at and merged_reason in their metadata. A group whose rows
// changed while it was read is refused and reported, never half merged.
//
// Merged shells are never grouped again, so a second run finds nothing to do. Phones that do
// not normalise are never grouped. The report carries ids, balances and a masked phone only.
func MergeDuplicateAccounts(ctx context.Context, db *ent.Client, opts MergeOptions, now time.Time) (MergeReport, error) {
	rep := MergeReport{DryRun: !opts.Apply, Groups: []MergeGroup{}}
	if db == nil {
		return rep, errors.New("no database")
	}
	tenants, err := mergeTenants(ctx, db, opts.TenantID)
	if err != nil {
		return rep, err
	}
	rep.Tenants = len(tenants)
	for _, tid := range tenants {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		groups, err := duplicateGroups(ctx, db, tid)
		if err != nil {
			return rep, err
		}
		for _, g := range groups {
			out := g.report()
			if opts.Apply {
				res, err := mergeGroup(ctx, db, tid, g, now)
				if err != nil {
					out.Status = MergeStatusRefused
					out.Error = err.Error()
					rep.Refused++
				} else {
					out.Status = MergeStatusMerged
					out.SurvivorID, out.Reason, out.ContactConflict = res.survivor, res.reason, res.conflict
					rep.Merged++
				}
			}
			rep.Groups = append(rep.Groups, out)
		}
	}
	return rep, nil
}

// mergeTenants returns the one tenant asked for, or every tenant with a loyalty account.
func mergeTenants(ctx context.Context, db *ent.Client, only *uuid.UUID) ([]uuid.UUID, error) {
	if only != nil {
		return []uuid.UUID{*only}, nil
	}
	var rows []struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := db.LoyaltyAccount.Query().GroupBy(entla.FieldTenantID).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list tenants with loyalty accounts: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r.TenantID != uuid.Nil {
			out = append(out, r.TenantID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// dupGroup is a planned merge: the normalised number and its live accounts.
type dupGroup struct {
	tenantID uuid.UUID
	phone    string
	accounts []*ent.LoyaltyAccount
}

func (g dupGroup) report() MergeGroup {
	best := chooseAccount(g.accounts, nil)
	_, conflict := survivorLink(best, losersInPickOrder(best, g.accounts))
	out := MergeGroup{
		TenantID:        g.tenantID,
		Phone:           maskPhone(g.phone),
		SurvivorID:      best.ID,
		Reason:          pickReason(best, g.accounts),
		ContactConflict: conflict,
		Status:          MergeStatusWouldMerge,
	}
	for _, a := range g.accounts {
		out.Accounts = append(out.Accounts, MergeAccount{
			ID: a.ID, Points: a.PointsBalance, LifetimePoints: a.LifetimePoints,
			CrmContactID: a.CrmContactID, CreatedAt: a.CreatedAt,
		})
	}
	return out
}

// duplicateGroups loads the tenant's live accounts and groups those whose phones normalise to
// the same E.164 number. Shells and phones that do not normalise are left out.
func duplicateGroups(ctx context.Context, db *ent.Client, tid uuid.UUID) ([]dupGroup, error) {
	rows, err := db.LoyaltyAccount.Query().
		Where(entla.TenantID(tid)).
		Order(ent.Asc(entla.FieldCreatedAt), ent.Asc(entla.FieldID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load loyalty accounts: %w", err)
	}
	byPhone := map[string][]*ent.LoyaltyAccount{}
	var order []string
	for _, a := range rows {
		if IsMerged(a.Metadata) {
			continue
		}
		norm, ok := strictPhone(a.CustomerPhone)
		if !ok {
			continue
		}
		if _, seen := byPhone[norm]; !seen {
			order = append(order, norm)
		}
		byPhone[norm] = append(byPhone[norm], a)
	}
	var out []dupGroup
	for _, p := range order {
		if accs := byPhone[p]; len(accs) > 1 {
			out = append(out, dupGroup{tenantID: tid, phone: p, accounts: accs})
		}
	}
	return out, nil
}

// strictPhone returns the E.164 form of a stored phone, or false when it does not normalise.
func strictPhone(raw string) (string, bool) {
	n, err := contact.NormalizePhone(strings.TrimSpace(raw), contact.DefaultRegion)
	if err != nil || n == "" {
		return "", false
	}
	return n, true
}

// errGroupChanged refuses a group whose rows changed between planning and merging.
var errGroupChanged = errors.New("the accounts changed while merging; run again")

// mergeResult is what mergeGroup did.
type mergeResult struct {
	survivor uuid.UUID
	reason   string
	conflict bool
}

// losersInPickOrder returns every row but the survivor, busiest first by the pick order.
func losersInPickOrder(survivor *ent.LoyaltyAccount, rows []*ent.LoyaltyAccount) []*ent.LoyaltyAccount {
	losers := make([]*ent.LoyaltyAccount, 0, len(rows))
	for _, a := range rows {
		if a.ID != survivor.ID {
			losers = append(losers, a)
		}
	}
	sort.SliceStable(losers, func(i, j int) bool {
		return chooseAccount([]*ent.LoyaltyAccount{losers[i], losers[j]}, nil) == losers[i]
	})
	return losers
}

// survivorLink returns the CRM link the survivor ends with (its own, else the busiest linked
// loser's) and whether any loser is linked to a different contact.
func survivorLink(survivor *ent.LoyaltyAccount, losers []*ent.LoyaltyAccount) (*uuid.UUID, bool) {
	final := survivor.CrmContactID
	if !hasLink(final) {
		final = nil
		for _, l := range losers {
			if hasLink(l.CrmContactID) {
				final = l.CrmContactID
				break
			}
		}
	}
	conflict := false
	for _, l := range losers {
		if hasLink(l.CrmContactID) && (final == nil || *l.CrmContactID != *final) {
			conflict = true
		}
	}
	return final, conflict
}

// mergeGroup merges one group in a single transaction and returns the survivor and why it won.
// The rows are read again inside the transaction and every loser update checks the balances it
// read, so a sale landing during the merge refuses the group instead of losing points.
func mergeGroup(ctx context.Context, db *ent.Client, tid uuid.UUID, g dupGroup, now time.Time) (mergeResult, error) {
	ids := make([]uuid.UUID, 0, len(g.accounts))
	for _, a := range g.accounts {
		ids = append(ids, a.ID)
	}

	tx, err := db.Tx(ctx)
	if err != nil {
		return mergeResult{}, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	fresh, err := tx.LoyaltyAccount.Query().
		Where(entla.TenantID(tid), entla.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return mergeResult{}, fmt.Errorf("reload accounts: %w", err)
	}
	if len(fresh) != len(ids) {
		return mergeResult{}, errGroupChanged
	}
	for _, a := range fresh {
		if IsMerged(a.Metadata) {
			return mergeResult{}, errors.New("an account was already merged by another run")
		}
		if n, ok := strictPhone(a.CustomerPhone); !ok || n != g.phone {
			return mergeResult{}, errGroupChanged
		}
	}

	survivor := chooseAccount(fresh, nil)
	reason := pickReason(survivor, fresh)
	// Losers in pick order, so the busiest linked loser gives its link.
	losers := losersInPickOrder(survivor, fresh)
	loserIDs := make([]uuid.UUID, 0, len(losers))
	for _, l := range losers {
		loserIDs = append(loserIDs, l.ID)
	}
	finalLink, conflict := survivorLink(survivor, losers)

	stamp := now.UTC().Format(time.RFC3339)
	var addPoints, addLifetime int
	var adoptCustomer, adoptProgram *uuid.UUID
	for _, l := range losers {
		addPoints += l.PointsBalance
		addLifetime += l.LifetimePoints
		if adoptCustomer == nil && l.CustomerID != nil && *l.CustomerID != uuid.Nil {
			adoptCustomer = l.CustomerID
		}
		if adoptProgram == nil && l.ProgramID != nil && *l.ProgramID != uuid.Nil {
			adoptProgram = l.ProgramID
		}

		// Empty the loser, but only if it is still a live row with the balances read above.
		n, err := tx.LoyaltyAccount.Update().
			Where(entla.ID(l.ID), entla.PointsBalance(l.PointsBalance), entla.LifetimePoints(l.LifetimePoints),
				AccountNotMerged()).
			SetPointsBalance(0).
			SetLifetimePoints(0).
			ClearCrmContactID().
			ClearCustomerID().
			SetMetadata(shellMetadata(l, survivor.ID, stamp)).
			Save(ctx)
		if err != nil {
			return mergeResult{}, fmt.Errorf("empty merged account: %w", err)
		}
		if n != 1 {
			return mergeResult{}, errGroupChanged
		}
	}

	// Lock the survivor after the losers (the same order an earn on a shell takes: shell, then
	// survivor) and write its metadata and links from this locked read, so a concurrent change
	// to its metadata is never overwritten with the copy read at the start.
	locked, err := LockAccount(ctx, tx, tid, survivor.ID, false)
	if err != nil {
		var me *MergedError
		if errors.As(err, &me) {
			return mergeResult{}, errors.New("the survivor was merged by another run")
		}
		return mergeResult{}, fmt.Errorf("lock survivor: %w", err)
	}
	survivor = locked
	// The survivor may have been linked since the first read; it keeps its own link if so.
	finalLink, conflict = survivorLink(survivor, losers)

	upd := tx.LoyaltyAccount.UpdateOneID(survivor.ID).
		AddPointsBalance(addPoints).
		AddLifetimePoints(addLifetime)
	meta := survivorMetadata(survivor.Metadata, loserIDs)
	if !hasLink(survivor.CrmContactID) && finalLink != nil {
		upd = upd.SetCrmContactID(*finalLink)
		// A live link from the loser replaces a deleted-contact marker on the survivor.
		meta = WithoutUnlinked(meta)
	}
	if (survivor.CustomerID == nil || *survivor.CustomerID == uuid.Nil) && adoptCustomer != nil {
		upd = upd.SetCustomerID(*adoptCustomer)
	}
	if (survivor.ProgramID == nil || *survivor.ProgramID == uuid.Nil) && adoptProgram != nil {
		upd = upd.SetProgramID(*adoptProgram)
	}
	if err := upd.SetMetadata(meta).Exec(ctx); err != nil {
		return mergeResult{}, fmt.Errorf("credit survivor: %w", err)
	}

	if err := repointChildren(ctx, tx, tid, loserIDs, survivor.ID); err != nil {
		return mergeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return mergeResult{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return mergeResult{survivor: survivor.ID, reason: reason, conflict: conflict}, nil
}

// repointChildren moves every row that names a loser account to the survivor: loyalty
// transactions, referrals on both sides, layaway plans and loyalty tender payments.
func repointChildren(ctx context.Context, tx *ent.Tx, tid uuid.UUID, losers []uuid.UUID, survivor uuid.UUID) error {
	if _, err := tx.LoyaltyTransaction.Update().
		Where(entlt.TenantID(tid), entlt.AccountIDIn(losers...)).
		SetAccountID(survivor).
		Save(ctx); err != nil {
		return fmt.Errorf("repoint loyalty transactions: %w", err)
	}
	if _, err := tx.Referral.Update().
		Where(entref.TenantID(tid), entref.ReferrerAccountIDIn(losers...)).
		SetReferrerAccountID(survivor).
		Save(ctx); err != nil {
		return fmt.Errorf("repoint referrals made: %w", err)
	}
	if _, err := tx.Referral.Update().
		Where(entref.TenantID(tid), entref.ReferredAccountIDIn(losers...)).
		SetReferredAccountID(survivor).
		Save(ctx); err != nil {
		return fmt.Errorf("repoint referrals received: %w", err)
	}
	if _, err := tx.LayawayPlan.Update().
		Where(entlayaway.TenantID(tid), entlayaway.LoyaltyAccountIDIn(losers...)).
		SetLoyaltyAccountID(survivor).
		Save(ctx); err != nil {
		return fmt.Errorf("repoint layaway plans: %w", err)
	}
	for _, loser := range losers {
		pays, err := tx.POSPayment.Query().
			Where(paymentNamesAccount(loser), entpay.HasOrderWith(entorder.TenantID(tid))).
			All(ctx)
		if err != nil {
			return fmt.Errorf("load loyalty tender payments: %w", err)
		}
		for _, p := range pays {
			data := make(map[string]any, len(p.PaymentData)+1)
			for k, v := range p.PaymentData {
				data[k] = v
			}
			data["loyalty_account_id"] = survivor.String()
			data[MergedFromPaymentKey] = loser.String()
			if err := tx.POSPayment.UpdateOneID(p.ID).SetPaymentData(data).Exec(ctx); err != nil {
				return fmt.Errorf("repoint loyalty tender payment: %w", err)
			}
		}
	}
	return nil
}

// paymentNamesAccount matches payments whose payment_data.loyalty_account_id is the account.
func paymentNamesAccount(id uuid.UUID) predicate.POSPayment {
	return predicate.POSPayment(func(s *sql.Selector) {
		s.Where(sqljson.ValueEQ(s.C(entpay.FieldPaymentData), id.String(), sqljson.Path("loyalty_account_id")))
	})
}

// shellMetadata keeps the loser's metadata and adds the merge audit note.
func shellMetadata(l *ent.LoyaltyAccount, survivor uuid.UUID, stamp string) map[string]any {
	out := make(map[string]any, len(l.Metadata)+3)
	for k, v := range l.Metadata {
		out[k] = v
	}
	out[MergedIntoKey] = survivor.String()
	out[MergedAtKey] = stamp
	out[MergedReasonKey] = mergeReason
	// The link is cleared on the shell; keep it here so it is never silently dropped.
	if hasLink(l.CrmContactID) {
		out[MergedCrmContactKey] = l.CrmContactID.String()
	}
	return out
}

// survivorMetadata keeps the survivor's metadata and appends the merged ids to merged_from.
func survivorMetadata(meta map[string]any, losers []uuid.UUID) map[string]any {
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	var from []any
	switch prev := meta[MergedFromKey].(type) {
	case []any:
		from = append(from, prev...)
	case []string:
		for _, s := range prev {
			from = append(from, s)
		}
	}
	for _, id := range losers {
		from = append(from, id.String())
	}
	out[MergedFromKey] = from
	return out
}

// pickReason names the first pick-order rule that set best apart from the other rows.
func pickReason(best *ent.LoyaltyAccount, rows []*ent.LoyaltyAccount) string {
	tied := make([]*ent.LoyaltyAccount, 0, len(rows))
	for _, a := range rows {
		if a.ID != best.ID && a.LifetimePoints == best.LifetimePoints {
			tied = append(tied, a)
		}
	}
	if len(tied) == 0 {
		return ReasonMostLifetimePoints
	}
	next := tied[:0:0]
	for _, a := range tied {
		if hasLink(a.CrmContactID) == hasLink(best.CrmContactID) {
			next = append(next, a)
		}
	}
	if len(next) == 0 {
		return ReasonLinked
	}
	for _, a := range next {
		if a.CreatedAt.Equal(best.CreatedAt) {
			return ReasonLowestID
		}
	}
	return ReasonOldest
}

// maskPhone hides every digit but the last three. Other characters, such as the plus sign,
// are dropped so the country code is hidden too.
func maskPhone(phone string) string {
	var d []byte
	for i := 0; i < len(phone); i++ {
		if phone[i] >= '0' && phone[i] <= '9' {
			d = append(d, phone[i])
		}
	}
	if len(d) <= 3 {
		return strings.Repeat("*", len(d))
	}
	return strings.Repeat("*", len(d)-3) + string(d[len(d)-3:])
}

// followMerged swaps merged shells for the accounts they were merged into, so a lookup that
// matched only a shell's spelling still lands on the live account. Live rows are kept as they
// are; shells are dropped when a live row is present. Chains are followed up to maxMergeHops.
//
// When only shells were found and their survivor cannot be reached (deleted, a bad id, or a
// chain that is too long) it returns ErrMergeChainBroken, never an empty result: an empty
// result would make callers create a new account beside the customer's real one.
func followMerged(ctx context.Context, db *ent.Client, tenantID uuid.UUID, rows []*ent.LoyaltyAccount) ([]*ent.LoyaltyAccount, error) {
	for hop := 0; hop <= maxMergeHops; hop++ {
		live := make([]*ent.LoyaltyAccount, 0, len(rows))
		var targets []uuid.UUID
		shells := 0
		seen := map[uuid.UUID]bool{}
		for _, a := range rows {
			if !IsMerged(a.Metadata) {
				live = append(live, a)
				continue
			}
			shells++
			if id, ok := mergedInto(a.Metadata); ok && !seen[id] {
				seen[id] = true
				targets = append(targets, id)
			}
		}
		if len(live) > 0 || shells == 0 {
			return live, nil
		}
		if len(targets) == 0 {
			return nil, ErrMergeChainBroken
		}
		next, err := db.LoyaltyAccount.Query().
			Where(entla.TenantID(tenantID), entla.IDIn(targets...)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		if len(next) == 0 {
			return nil, ErrMergeChainBroken
		}
		rows = next
	}
	return nil, ErrMergeChainBroken
}

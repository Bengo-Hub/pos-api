package customers

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/Bengo-Hub/httpware/contact"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
)

const (
	// reconcileLockID is the Postgres advisory lock that keeps one replica reconciling at a time.
	reconcileLockID = 727271050
	// reconcileInterval is how often unlinked records are retried.
	reconcileInterval = 10 * time.Minute
	// reconcileBatch is the most accounts, and separately client records, linked per tenant per pass.
	reconcileBatch = 500
)

// cursor remembers where the last pass stopped in one tenant, so records the CRM keeps refusing
// do not hold the head of the queue forever. It resets when a pass reaches the end.
type cursor struct {
	at time.Time
	id uuid.UUID
}

// Reconciler links loyalty accounts and client records that were saved while the CRM was down.
type Reconciler struct {
	res   *Resolver
	sqlDB *sql.DB
	log   *zap.Logger

	accountCursor map[uuid.UUID]cursor
	clientCursor  map[uuid.UUID]cursor
}

// NewReconciler builds the reconciler. sqlDB backs the advisory lock and must be the database
// behind the resolver's ent client. A nil sqlDB runs without the lock (tests only).
func NewReconciler(res *Resolver, sqlDB *sql.DB, log *zap.Logger) *Reconciler {
	if log == nil {
		log = zap.NewNop()
	}
	return &Reconciler{
		res:           res,
		sqlDB:         sqlDB,
		log:           log.Named("customers.reconciler"),
		accountCursor: map[uuid.UUID]cursor{},
		clientCursor:  map[uuid.UUID]cursor{},
	}
}

// Run reconciles every reconcileInterval until ctx ends. Start it in a goroutine.
func (r *Reconciler) Run(ctx context.Context) {
	if r == nil || r.res == nil || r.res.crm == nil {
		return
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	// The first pass waits a minute so it does not compete with startup work.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
		if err := r.RunOnce(ctx); err != nil {
			r.log.Warn("customer reconcile pass failed", zap.Error(err))
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil {
				r.log.Warn("customer reconcile pass failed", zap.Error(err))
			}
		}
	}
}

// RunOnce does one pass under the advisory lock. It returns nil without work when another
// replica holds the lock.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	release, ok, err := r.lock(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer release()

	tenants, err := r.unlinkedTenants(ctx)
	if err != nil {
		return err
	}
	for _, tid := range tenants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		up, err := r.linkAccounts(ctx, tid)
		if err != nil {
			return err
		}
		if !up {
			r.log.Info("crm unavailable, reconcile pass stopped early")
			return nil
		}
		up, err = r.linkClients(ctx, tid)
		if err != nil {
			return err
		}
		if !up {
			r.log.Info("crm unavailable, reconcile pass stopped early")
			return nil
		}
	}
	return nil
}

// lock takes the advisory lock on one pooled connection and returns its release function.
func (r *Reconciler) lock(ctx context.Context) (func(), bool, error) {
	if r.sqlDB == nil {
		return func() {}, true, nil
	}
	conn, err := r.sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("reconcile lock conn: %w", err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", reconcileLockID).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("reconcile lock: %w", err)
	}
	if !got {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		var released bool
		err := conn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1)", reconcileLockID).Scan(&released)
		if err != nil || !released {
			// The session may still hold the lock. Returning driver.ErrBadConn from Raw makes
			// database/sql discard the connection, which ends the session and frees the lock,
			// instead of handing a locked connection back to the pool.
			r.log.Warn("reconcile unlock failed, discarding the connection", zap.Error(err))
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, true, nil
}

// unlinkedTenants lists tenants with at least one unlinked account or client record.
func (r *Reconciler) unlinkedTenants(ctx context.Context) ([]uuid.UUID, error) {
	var rows []struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := r.res.db.LoyaltyAccount.Query().
		Where(entla.CrmContactIDIsNil(), accountNotUnlinked()).
		GroupBy(entla.FieldTenantID).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list tenants with unlinked accounts: %w", err)
	}
	var clientRows []struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := r.res.db.ClientRecord.Query().
		Where(entclient.CrmContactIDIsNil(), clientNotUnlinked()).
		GroupBy(entclient.FieldTenantID).
		Scan(ctx, &clientRows); err != nil {
		return nil, fmt.Errorf("list tenants with unlinked clients: %w", err)
	}
	seen := map[uuid.UUID]bool{}
	out := make([]uuid.UUID, 0, len(rows)+len(clientRows))
	for _, row := range append(rows, clientRows...) {
		if row.TenantID == uuid.Nil || seen[row.TenantID] {
			continue
		}
		seen[row.TenantID] = true
		out = append(out, row.TenantID)
	}
	return out, nil
}

// linkAccounts links up to reconcileBatch unlinked accounts of one tenant, oldest first. It
// returns false when the CRM stopped answering, so the pass can end early.
func (r *Reconciler) linkAccounts(ctx context.Context, tid uuid.UUID) (bool, error) {
	q := r.res.db.LoyaltyAccount.Query().
		Where(entla.TenantID(tid), entla.CrmContactIDIsNil(), accountNotUnlinked())
	if c, ok := r.accountCursor[tid]; ok {
		q = q.Where(entla.Or(
			entla.CreatedAtGT(c.at),
			entla.And(entla.CreatedAt(c.at), entla.IDGT(c.id)),
		))
	}
	accounts, err := q.Order(ent.Asc(entla.FieldCreatedAt), ent.Asc(entla.FieldID)).
		Limit(reconcileBatch).
		All(ctx)
	if err != nil {
		return true, fmt.Errorf("load unlinked accounts: %w", err)
	}
	if len(accounts) < reconcileBatch {
		delete(r.accountCursor, tid)
	} else {
		last := accounts[len(accounts)-1]
		r.accountCursor[tid] = cursor{at: last.CreatedAt, id: last.ID}
	}

	for _, acc := range accounts {
		ident, err := r.res.Link(ctx, tid, acc.CustomerPhone, acc.CustomerEmail, acc.CustomerName)
		if err != nil {
			r.log.Warn("crm refused an unlinked loyalty account",
				zap.String("account_id", acc.ID.String()), zap.Error(err))
			continue
		}
		if ident.CrmContactID == nil {
			delete(r.accountCursor, tid)
			return false, nil
		}
		r.reportAccountCollision(ctx, acc, *ident.CrmContactID)
		if err := r.res.snapshotAccount(ctx, acc, ident); err != nil {
			return true, err
		}
	}
	return true, nil
}

// reportAccountCollision logs other accounts of the same tenant that are the same customer:
// linked to the same contact, or sharing the subscriber digits of the phone. It never merges.
func (r *Reconciler) reportAccountCollision(ctx context.Context, acc *ent.LoyaltyAccount, contactID uuid.UUID) {
	match := entla.CrmContactID(contactID)
	if digits := contact.SubscriberDigits(acc.CustomerPhone); len(digits) == 9 {
		match = entla.Or(match, entla.CustomerPhoneHasSuffix(digits))
	}
	others, err := r.res.db.LoyaltyAccount.Query().
		Where(entla.TenantID(acc.TenantID), entla.IDNEQ(acc.ID), match).
		IDs(ctx)
	if err != nil {
		return
	}
	for _, id := range others {
		r.log.Warn("two loyalty accounts are the same customer, not merged",
			zap.String("account_id", acc.ID.String()), zap.String("other_account_id", id.String()))
	}
}

// linkClients links up to reconcileBatch unlinked client records of one tenant, oldest first.
func (r *Reconciler) linkClients(ctx context.Context, tid uuid.UUID) (bool, error) {
	q := r.res.db.ClientRecord.Query().
		Where(entclient.TenantID(tid), entclient.CrmContactIDIsNil(), clientNotUnlinked())
	if c, ok := r.clientCursor[tid]; ok {
		q = q.Where(entclient.Or(
			entclient.CreatedAtGT(c.at),
			entclient.And(entclient.CreatedAt(c.at), entclient.IDGT(c.id)),
		))
	}
	clients, err := q.Order(ent.Asc(entclient.FieldCreatedAt), ent.Asc(entclient.FieldID)).
		Limit(reconcileBatch).
		All(ctx)
	if err != nil {
		return true, fmt.Errorf("load unlinked clients: %w", err)
	}
	if len(clients) < reconcileBatch {
		delete(r.clientCursor, tid)
	} else {
		last := clients[len(clients)-1]
		r.clientCursor[tid] = cursor{at: last.CreatedAt, id: last.ID}
	}

	for _, rec := range clients {
		ident, err := r.res.Link(ctx, tid, rec.Phone, "", "")
		if err != nil {
			r.log.Warn("crm refused an unlinked client record",
				zap.String("client_id", rec.ID.String()), zap.Error(err))
			continue
		}
		if ident.CrmContactID == nil {
			delete(r.clientCursor, tid)
			return false, nil
		}
		if err := r.res.snapshotClient(ctx, rec, ident); err != nil {
			return true, err
		}
	}
	return true, nil
}

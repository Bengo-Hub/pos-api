package identity

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/ent/staffoutlet"
	"github.com/bengobox/pos-service/internal/ent/tenant"
	"github.com/bengobox/pos-service/internal/ent/user"
	"github.com/bengobox/pos-service/internal/ent/posuserroleassignment"
	"github.com/bengobox/pos-service/internal/ent/userposrole"
	"github.com/bengobox/pos-service/internal/posrole"
)

// staffPinFastHash computes hex(SHA256(tenantID+":"+userID+":"+pin)).
// Mirrors the identical helper in handlers/pin_auth.go — user-scoped so one PIN works
// across all outlets the user is assigned to.
func staffPinFastHash(tenantID, userID uuid.UUID, pin string) string {
	h := sha256.Sum256([]byte(tenantID.String() + ":" + userID.String() + ":" + pin))
	return fmt.Sprintf("%x", h)
}

// AuthEventHandler handles auth-service user events for proactive user sync.
type AuthEventHandler struct {
	client       *ent.Client
	tenantSyncer interface {
		SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
		SyncOutlets(ctx context.Context, tenantID uuid.UUID, tenantSlug string) error
	}
	logger *zap.Logger
}

// NewAuthEventHandler creates a new auth event handler.
func NewAuthEventHandler(client *ent.Client, svc *Service, logger *zap.Logger) *AuthEventHandler {
	return &AuthEventHandler{
		client:       client,
		tenantSyncer: svc.tenantSyncer,
		logger:       logger.Named("identity.auth_events"),
	}
}

// SubscribeToAuthEvents subscribes to auth-service user events via JetStream with durable consumers.
func (h *AuthEventHandler) SubscribeToAuthEvents(nc *nats.Conn) error {
	if nc == nil {
		h.logger.Warn("NATS connection not available, skipping auth event subscriptions")
		return nil
	}

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("auth user events: jetstream init: %w", err)
	}

	// Ensure auth stream exists (guard against startup race with auth-api).
	if _, err := js.StreamInfo(authStream); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name:      authStream,
			Subjects:  []string{"auth.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		}); addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			h.logger.Warn("auth user events: ensure auth stream failed", zap.Error(addErr))
		}
	}

	type sub struct {
		subject string
		durable string
		handler func(context.Context, *sharedevents.Event) error
	}
	subs := []sub{
		{"auth.user.created", "pos-auth-user-created", h.handleUserCreated},
		{"auth.user.updated", "pos-auth-user-updated", h.handleUserUpdated},
		{"auth.user.pin_set", "pos-auth-user-pin-set", h.handleUserPINSet},
		{"auth.user.deleted", "pos-auth-user-deleted", h.handleUserDeleted},
	}

	for _, s := range subs {
		s := s
		sharedevents.SubscribeQueueWithRebind(h.logger, js, "auth", s.subject, s.durable, func(msg *nats.Msg) {
			evt, err := sharedevents.FromJSON(msg.Data)
			if err != nil {
				h.logger.Error("failed to unmarshal auth user event",
					zap.String("subject", s.subject), zap.Error(err))
				_ = msg.Nak()
				return
			}
			ctx := context.Background()
			if err := s.handler(ctx, evt); err != nil {
				h.logger.Error("failed to handle auth user event",
					zap.String("subject", s.subject), zap.Error(err))
				_ = msg.Nak()
				return
			}
			_ = msg.Ack()
		},
			nats.Durable(s.durable),
			nats.AckExplicit(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(5),
			nats.DeliverAll(),
		)
	}

	h.logger.Info("auth event subscriptions active",
		zap.String("subjects", "auth.user.created, auth.user.updated, auth.user.pin_set"))
	return nil
}

func (h *AuthEventHandler) handleUserCreated(ctx context.Context, evt *sharedevents.Event) error {
	userIDStr, _ := evt.Payload["user_id"].(string)
	email, _ := evt.Payload["email"].(string)
	fullName, _ := evt.Payload["full_name"].(string)
	tenantSlug, _ := evt.Payload["tenant_slug"].(string)

	authServiceUserID, err := uuid.Parse(userIDStr)
	if err != nil {
		return fmt.Errorf("invalid user_id %q: %w", userIDStr, err)
	}
	if handled, err := h.revoked(ctx, evt, authServiceUserID); handled {
		return err
	}

	// Check if user already exists
	exists, _ := h.client.User.Query().
		Where(user.AuthServiceUserIDEQ(authServiceUserID)).
		Exist(ctx)
	if exists {
		h.logger.Debug("user already exists, skipping",
			zap.String("user_id", userIDStr))
		return nil
	}

	// Resolve tenant ID — prefer tenant_slug from payload, fallback to event tenant_id
	var tenantID uuid.UUID
	if tenantSlug != "" && h.tenantSyncer != nil {
		tenantID, err = h.tenantSyncer.SyncTenant(ctx, tenantSlug)
		if err != nil {
			h.logger.Warn("failed to sync tenant from slug, using event tenant_id",
				zap.String("slug", tenantSlug), zap.Error(err))
			tenantID = evt.TenantID
		}
	} else {
		tenantID = evt.TenantID
	}

	if tenantID == uuid.Nil {
		return fmt.Errorf("no tenant_id available for user %s", userIDStr)
	}

	// Create user
	_, err = h.client.User.Create().
		SetID(authServiceUserID).
		SetAuthServiceUserID(authServiceUserID).
		SetTenantID(tenantID).
		SetEmail(email).
		SetFullName(fullName).
		SetStatus("active").
		SetSyncStatus("synced").
		SetSyncAt(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create user from auth event: %w", err)
	}

	h.logger.Info("user created from auth.user.created event",
		zap.String("user_id", userIDStr),
		zap.String("tenant_id", tenantID.String()),
		zap.String("email", email))

	// Upsert StaffMember so this user can set a PIN and log in at POS terminals.
	h.upsertStaffMember(ctx, authServiceUserID, tenantID, fullName, evt)

	return nil
}

func (h *AuthEventHandler) handleUserUpdated(ctx context.Context, evt *sharedevents.Event) error {
	userIDStr, _ := evt.Payload["user_id"].(string)
	email, _ := evt.Payload["email"].(string)
	fullName, _ := evt.Payload["full_name"].(string)

	authServiceUserID, err := uuid.Parse(userIDStr)
	if err != nil {
		return fmt.Errorf("invalid user_id %q: %w", userIDStr, err)
	}
	if handled, err := h.revoked(ctx, evt, authServiceUserID); handled {
		return err
	}

	// Find existing user — if absent, create it so the StaffMember name patch below can run.
	u, err := h.client.User.Query().
		Where(user.AuthServiceUserIDEQ(authServiceUserID)).
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return fmt.Errorf("query user: %w", err)
		}
		// User row doesn't exist (e.g. after a DB reset). Resolve tenant then create it.
		tenantID := h.eventTenant(ctx, evt)
		if tenantID == uuid.Nil {
			h.logger.Debug("user not found and no tenant_id in updated event, skipping",
				zap.String("user_id", userIDStr))
			return nil
		}
		if _, createErr := h.client.User.Create().
			SetID(authServiceUserID).
			SetAuthServiceUserID(authServiceUserID).
			SetTenantID(tenantID).
			SetEmail(email).
			SetFullName(fullName).
			SetStatus("active").
			SetSyncStatus("synced").
			SetSyncAt(time.Now()).
			Save(ctx); createErr != nil {
			return fmt.Errorf("create user from updated event: %w", createErr)
		}
		h.logger.Info("user created from auth.user.updated event (backfill)",
			zap.String("user_id", userIDStr))
	} else {
		// Update user fields
		update := h.client.User.UpdateOne(u)
		if email != "" {
			update = update.SetEmail(email)
		}
		if fullName != "" {
			update = update.SetFullName(fullName)
		}
		update = update.SetSyncStatus("synced").SetSyncAt(time.Now())
		if _, err := update.Save(ctx); err != nil {
			return fmt.Errorf("update user from auth event: %w", err)
		}
	}

	// Patch the StaffMember of the EVENT's tenant only. The roles and outlets in the event are
	// that tenant's membership; u.TenantID is merely the tenant where pos first saw the person,
	// so patching by it applied a second tenant's roles to the first tenant's row (a cashier in
	// tenant A who is admin of their own tenant B became admin in A). No row there, or no
	// resolvable tenant: change nothing.
	staffTenant := h.eventTenant(ctx, evt)
	posRole := mapSSORoleToPOS(evt.Payload)
	outletIDs := parseOutletIDs(evt.Payload)
	if staffTenant == uuid.Nil {
		if fullName != "" || posRole != "" || len(outletIDs) > 0 {
			h.logger.Warn("auth update: event tenant not resolvable, staff rows left unchanged",
				zap.String("user_id", userIDStr), zap.String("event_tenant_id", evt.TenantID.String()))
		}
	} else if fullName != "" || posRole != "" || len(outletIDs) > 0 {
		sm, qerr := h.client.StaffMember.Query().
			Where(staffmember.TenantID(staffTenant), staffmember.UserID(authServiceUserID)).
			Only(ctx)
		switch {
		case ent.IsNotFound(qerr):
			h.logger.Info("auth update: no staff row in the event's tenant, nothing to patch",
				zap.String("user_id", userIDStr), zap.String("tenant_id", staffTenant.String()))
		case qerr != nil:
			h.logger.Warn("auth update: query staff row", zap.Error(qerr),
				zap.String("user_id", userIDStr), zap.String("tenant_id", staffTenant.String()))
		default:
			if fullName != "" || posRole != "" {
				upd := sm.Update()
				if fullName != "" {
					upd = upd.SetName(fullName)
				}
				// Update role if the event includes roles — ensures re-provisioning corrects stale
				// role mappings. A raise into a higher tier also clears the PIN (clearPINOnRaise).
				if posRole != "" {
					upd = upd.SetRole(posRole)
					upd = h.clearPINOnRaise(upd, sm, posRole)
				}
				if err := upd.Exec(ctx); err != nil {
					h.logger.Error("auth update: patch staff row failed", zap.Error(err),
						zap.String("tenant_id", staffTenant.String()), zap.String("staff_id", sm.ID.String()))
				}
			}
			if len(outletIDs) > 0 {
				h.upsertStaffOutlets(ctx, sm.ID, staffTenant, outletIDs)
			}
		}
	}

	h.logger.Info("user updated from auth.user.updated event",
		zap.String("user_id", userIDStr),
		zap.String("email", email))
	return nil
}

func (h *AuthEventHandler) handleUserPINSet(ctx context.Context, evt *sharedevents.Event) error {
	userIDStr, _ := evt.Payload["user_id"].(string)
	service, _ := evt.Payload["service"].(string)
	pinHash, _ := evt.Payload["pin_hash"].(string)

	if service != "pos" {
		// Not for this service — silently skip
		return nil
	}

	authServiceUserID, err := uuid.Parse(userIDStr)
	if err != nil {
		return fmt.Errorf("invalid user_id %q: %w", userIDStr, err)
	}

	if pinHash == "" {
		return fmt.Errorf("pin_hash is required")
	}
	if handled, err := h.revoked(ctx, evt, authServiceUserID); handled {
		return err
	}

	// The PIN and role belong to the EVENT's tenant. Resolve it by tenant_slug first (pos's
	// tenant id can differ from auth-api's after a tenant is recreated upstream), then the
	// event's tenant id, and touch only the (tenant, user) row. Looking the row up by user_id
	// alone wrote a PIN and role the person set in their own tenant B onto their row in tenant
	// A, where they then signed in as admin.
	tenantID := h.eventTenant(ctx, evt)
	if tenantID == uuid.Nil {
		h.logger.Warn("auth pin_set: event tenant not resolvable, no staff row changed",
			zap.String("user_id", userIDStr), zap.String("event_tenant_id", evt.TenantID.String()))
		return nil
	}
	existing, err := h.client.StaffMember.Query().
		Where(staffmember.TenantID(tenantID), staffmember.UserID(authServiceUserID)).
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return fmt.Errorf("query StaffMember: %w", err)
		}
		// No row in the event's tenant yet (pin_set can arrive before auth.user.created):
		// create it in THAT tenant only, so the PIN is not lost.
		name := ""
		role := "cashier" // safe fallback

		// Use full_name and role from event payload (auth-api now includes both).
		if fn, _ := evt.Payload["full_name"].(string); fn != "" {
			name = fn
		}
		if r := mapSSORoleToPOS(evt.Payload); r != "" {
			role = r
		}

		// The local User row supplies a name only; its tenant is wherever pos first saw the
		// person and must not decide which tenant's row gets this PIN.
		if name == "" {
			if u, uErr := h.client.User.Query().Where(user.AuthServiceUserIDEQ(authServiceUserID)).Only(ctx); uErr == nil {
				name = u.FullName
			}
		}
		if name == "" {
			name = userIDStr[:8]
		}
		tenantSlug, _ := evt.Payload["tenant_slug"].(string)

		homeOutlet, oErr := h.client.Outlet.Query().
			Where(outlet.TenantID(tenantID), outlet.IsHq(true), outlet.StatusEQ("active"),
				outlet.UseCaseNEQ("logistics"), outlet.UseCaseNEQ("warehouse")).
			First(ctx)
		if oErr != nil {
			homeOutlet, oErr = h.client.Outlet.Query().
				Where(outlet.TenantID(tenantID), outlet.StatusEQ("active"),
					outlet.UseCaseNEQ("logistics"), outlet.UseCaseNEQ("warehouse")).
				First(ctx)
		}
		// No outlet yet — try syncing from auth-api then retry once.
		if oErr != nil && tenantSlug != "" && h.tenantSyncer != nil {
			if syncErr := h.tenantSyncer.SyncOutlets(ctx, tenantID, tenantSlug); syncErr == nil {
				homeOutlet, oErr = h.client.Outlet.Query().
					Where(outlet.TenantID(tenantID), outlet.StatusEQ("active"),
						outlet.UseCaseNEQ("logistics"), outlet.UseCaseNEQ("warehouse")).
					First(ctx)
			}
		}
		if oErr != nil {
			return fmt.Errorf("no active outlet found for tenant %s, cannot create StaffMember: %w", tenantID, oErr)
		}
		// auth.user.created and auth.user.pin_set can be consumed concurrently
		// (e.g. different pos-api replicas, back-to-back events during tenant
		// registration for the owner/admin account) — both racing to create the
		// same StaffMember row. Upsert on the (tenant_id, user_id) unique index
		// instead of plain Create so the loser of the race gracefully no-ops
		// instead of erroring the whole handler out. Use Exec (not .ID), since
		// Postgres DO NOTHING has no RETURNING row to give back on conflict —
		// re-query below for the authoritative row either way.
		if createErr := h.client.StaffMember.Create().
			SetTenantID(tenantID).
			SetUserID(authServiceUserID).
			SetName(name).
			SetRole(role).
			SetIsActive(true).
			SetPinFailedAttempts(0).
			OnConflictColumns(staffmember.FieldTenantID, staffmember.FieldUserID).
			DoNothing().
			Exec(ctx); createErr != nil {
			return fmt.Errorf("create StaffMember for PIN set: %w", createErr)
		}
		// Re-fetch: either our own insert, or the row the concurrent racer created.
		existing, err = h.client.StaffMember.Query().
			Where(staffmember.TenantID(tenantID), staffmember.UserID(authServiceUserID)).
			Only(ctx)
		if err != nil {
			return fmt.Errorf("StaffMember not found after upsert: %w", err)
		}
		// Assign home outlet via join table.
		_ = h.client.StaffOutlet.Create().
			SetTenantID(tenantID).
			SetStaffMemberID(existing.ID).
			SetOutletID(homeOutlet.ID).
			SetIsHomeOutlet(true).
			OnConflict().DoNothing().Exec(ctx)
	}

	upd := existing.Update().SetPinHash(pinHash)
	// If the event includes the raw pin (sent by auth-api seed for internal NATS events),
	// pre-compute and store pin_fast_hash (user-scoped) for the direct-auth endpoint.
	if rawPin, _ := evt.Payload["pin"].(string); rawPin != "" {
		upd = upd.SetPinFastHash(staffPinFastHash(existing.TenantID, existing.UserID, rawPin))
	}
	// Always update role from the event payload. This corrects stale role assignments
	// (e.g. "cashier" set by old events that lacked a roles field) every time the PIN
	// is re-set, so admins and managers are never stuck with a demoted role.
	if posRole := mapSSORoleToPOS(evt.Payload); posRole != "" {
		upd = upd.SetRole(posRole)
	}
	// Update name if provided (event now includes full_name from auth-api).
	if fullName, _ := evt.Payload["full_name"].(string); fullName != "" && existing.Name != fullName {
		upd = upd.SetName(fullName)
	}
	if err := upd.Exec(ctx); err != nil {
		return fmt.Errorf("update PIN hash: %w", err)
	}

	h.logger.Info("staff PIN updated from auth.user.pin_set event",
		zap.String("user_id", userIDStr),
		zap.String("role", existing.Role))
	return nil
}

// handleUserDeleted hard-deletes this user's local pos-api rows after auth-api
// permanently deletes the account (AdminPurgeUser). Child rows first, in FK order
// (both user_pos_roles->users and staff_outlets->staff_members carry a real
// OnDelete:NoAction constraint, so deleting the parent before its children errors
// instead of cascading): staff_outlets -> user_pos_roles -> staff_members -> users.
// Payroll/advance/commission/purchase-link/housekeeping rows that softly reference
// staff_member_id (no DB FK, comment-only) are deliberately left alone — those are
// financial/audit records, not identity data, matching auth-api's own AdminPurgeUser
// doc comment about LoginAttempt/AuditLog rows staying in place with a dangling ID.
func (h *AuthEventHandler) handleUserDeleted(ctx context.Context, evt *sharedevents.Event) error {
	userIDStr, _ := evt.Payload["user_id"].(string)
	authServiceUserID, err := uuid.Parse(userIDStr)
	if err != nil {
		return fmt.Errorf("invalid user_id %q: %w", userIDStr, err)
	}
	if err := h.removeLocalUser(ctx, authServiceUserID); err != nil {
		return err
	}
	h.logger.Info("user hard-deleted from auth.user.deleted event", zap.String("user_id", userIDStr))
	return nil
}

// posRelevance reads auth-api's services list on auth.user events (shared-events UserRelevance).
var posRelevance = sharedevents.UserRelevance{Service: "pos"}

// revoked removes the person's copy in the event's tenant (resolved by slug, since pos-api's
// tenant id can differ from auth-api's) when the event's services list leaves POS out. Their
// rows in other tenants stay; the User row goes only with the last staff row (see
// removeLocalUserInTenant). Reports whether the event was handled that way. Events without the
// list are never handled here.
func (h *AuthEventHandler) revoked(ctx context.Context, evt *sharedevents.Event, authServiceUserID uuid.UUID) (bool, error) {
	if !posRelevance.Revoked(evt.Payload) {
		return false, nil
	}
	// The revoke is about the event's tenant only. A person can work in several tenants; their
	// staff rows elsewhere stay. (This used to compare against the User row's tenant: a revoke
	// there deleted the person's rows in every tenant, and a revoke anywhere else did nothing.)
	tenantID := h.eventTenant(ctx, evt)
	if tenantID == uuid.Nil {
		h.logger.Warn("auth revoke: event tenant not resolvable, nothing removed",
			zap.String("user_id", authServiceUserID.String()), zap.String("event_tenant_id", evt.TenantID.String()))
		return true, nil
	}
	userRemoved, err := h.removeLocalUserInTenant(ctx, authServiceUserID, tenantID)
	if err != nil {
		return true, fmt.Errorf("remove user outside pos: %w", err)
	}
	h.logger.Info("user removed from tenant: not a pos member", zap.String("user_id", authServiceUserID.String()),
		zap.String("tenant_id", tenantID.String()), zap.Bool("user_row_removed", userRemoved))
	return true, nil
}

// removeLocalUserInTenant removes pos-api's copy of a person in ONE tenant: their staff rows
// there (with staff outlets), their POS role assignments there and their legacy outlet roles on
// that tenant's outlets. The person's rows in other tenants are left as they are. The User row
// (and anything left that references it) goes only when no tenant still has a staff row for
// them, since users carry FKs from role tables. Reports whether the User row was removed.
func (h *AuthEventHandler) removeLocalUserInTenant(ctx context.Context, authServiceUserID, tenantID uuid.UUID) (bool, error) {
	tx, err := h.client.Tx(ctx)
	if err != nil {
		return false, fmt.Errorf("start tx: %w", err)
	}
	fail := func(step string, err error) (bool, error) {
		_ = tx.Rollback()
		return false, fmt.Errorf("%s: %w", step, err)
	}

	staffIDs, err := tx.StaffMember.Query().
		Where(staffmember.TenantID(tenantID), staffmember.UserID(authServiceUserID)).
		IDs(ctx)
	if err != nil {
		return fail("query staff members", err)
	}
	if len(staffIDs) > 0 {
		if _, err := tx.StaffOutlet.Delete().Where(staffoutlet.StaffMemberIDIn(staffIDs...)).Exec(ctx); err != nil {
			return fail("delete staff outlets", err)
		}
		if _, err := tx.StaffMember.Delete().Where(staffmember.IDIn(staffIDs...)).Exec(ctx); err != nil {
			return fail("delete staff members", err)
		}
	}
	if _, err := tx.POSUserRoleAssignment.Delete().
		Where(posuserroleassignment.TenantID(tenantID), posuserroleassignment.UserID(authServiceUserID)).
		Exec(ctx); err != nil {
		return fail("delete pos user role assignments", err)
	}
	outletIDs, err := tx.Outlet.Query().Where(outlet.TenantID(tenantID)).IDs(ctx)
	if err != nil {
		return fail("query tenant outlets", err)
	}
	if len(outletIDs) > 0 {
		if _, err := tx.UserPOSRole.Delete().
			Where(userposrole.UserID(authServiceUserID), userposrole.OutletIDIn(outletIDs...)).
			Exec(ctx); err != nil {
			return fail("delete user pos roles", err)
		}
	}

	remaining, err := tx.StaffMember.Query().Where(staffmember.UserID(authServiceUserID)).Count(ctx)
	if err != nil {
		return fail("count remaining staff rows", err)
	}
	userRemoved := false
	if remaining == 0 {
		if _, err := tx.UserPOSRole.Delete().Where(userposrole.UserID(authServiceUserID)).Exec(ctx); err != nil {
			return fail("delete user pos roles", err)
		}
		if _, err := tx.POSUserRoleAssignment.Delete().Where(posuserroleassignment.UserID(authServiceUserID)).Exec(ctx); err != nil {
			return fail("delete pos user role assignments", err)
		}
		n, err := tx.User.Delete().Where(user.AuthServiceUserIDEQ(authServiceUserID)).Exec(ctx)
		if err != nil {
			return fail("delete user", err)
		}
		userRemoved = n > 0
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return userRemoved, nil
}

// removeLocalUser hard-deletes pos-api's copy of a user, child rows first: staff outlets, POS
// roles, staff member, user. Orders and payments keep their user id.
func (h *AuthEventHandler) removeLocalUser(ctx context.Context, authServiceUserID uuid.UUID) error {
	tx, err := h.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("start tx: %w", err)
	}

	staffMembers, err := tx.StaffMember.Query().Where(staffmember.UserID(authServiceUserID)).All(ctx)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("query staff members: %w", err)
	}
	staffMemberIDs := make([]uuid.UUID, 0, len(staffMembers))
	for _, sm := range staffMembers {
		staffMemberIDs = append(staffMemberIDs, sm.ID)
	}
	if len(staffMemberIDs) > 0 {
		if _, err := tx.StaffOutlet.Delete().Where(staffoutlet.StaffMemberIDIn(staffMemberIDs...)).Exec(ctx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete staff outlets: %w", err)
		}
	}
	if _, err := tx.UserPOSRole.Delete().Where(userposrole.UserID(authServiceUserID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete user pos roles: %w", err)
	}
	// pos_user_role_assignments has a NO ACTION FK to users too: without this, removing anyone
	// who holds a v2 role assignment failed and the copy stayed.
	if _, err := tx.POSUserRoleAssignment.Delete().Where(posuserroleassignment.UserID(authServiceUserID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete pos user role assignments: %w", err)
	}
	if _, err := tx.StaffMember.Delete().Where(staffmember.UserID(authServiceUserID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete staff members: %w", err)
	}
	if _, err := tx.User.Delete().Where(user.AuthServiceUserIDEQ(authServiceUserID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// eventTenant resolves the pos tenant an auth.user event belongs to, the way revoked() does:
// tenant_slug through the tenant syncer (pos's tenant id can differ from auth-api's), falling
// back to the event's tenant id. Returns uuid.Nil when neither names a tenant pos holds, so the
// caller changes nothing rather than guess another tenant.
func (h *AuthEventHandler) eventTenant(ctx context.Context, evt *sharedevents.Event) uuid.UUID {
	if slug, _ := evt.Payload["tenant_slug"].(string); slug != "" && h.tenantSyncer != nil {
		if tid, err := h.tenantSyncer.SyncTenant(ctx, slug); err == nil && tid != uuid.Nil {
			return tid
		}
	}
	if evt.TenantID == uuid.Nil {
		return uuid.Nil
	}
	if ok, err := h.client.Tenant.Query().Where(tenant.ID(evt.TenantID)).Exist(ctx); err != nil || !ok {
		return uuid.Nil
	}
	return evt.TenantID
}

// clearPINOnRaise adds a PIN reset to upd when newRole puts the row in a higher tier than it
// holds (staff, then manager, then admin). The PIN may have been chosen by a manager while the
// row sat below their tier; after the raise it would sign in at the higher tier, so the person
// sets a new one. Logs ids only. auth.user.pin_set is not routed here: it carries the person's
// new PIN together with the role.
func (h *AuthEventHandler) clearPINOnRaise(upd *ent.StaffMemberUpdateOne, sm *ent.StaffMember, newRole string) *ent.StaffMemberUpdateOne {
	if !posrole.RaisesTier(sm.Role, newRole) || (sm.PinHash == nil && sm.PinFastHash == nil) {
		return upd
	}
	h.logger.Info("staff PIN cleared after a role raise from auth",
		zap.String("tenant_id", sm.TenantID.String()), zap.String("staff_id", sm.ID.String()))
	return upd.ClearPinHash().ClearPinFastHash().SetPinFailedAttempts(0).ClearPinLockedUntil()
}

// upsertStaffMember creates a StaffMember for the given user if one does not exist,
// or updates name/role if they changed, then upserts StaffOutlet rows. This is idempotent.
func (h *AuthEventHandler) upsertStaffMember(ctx context.Context, userID, tenantID uuid.UUID, name string, evt *sharedevents.Event) {
	posRole := mapSSORoleToPOS(evt.Payload)
	if posRole == "" {
		h.logger.Debug("skipping StaffMember upsert for non-POS role",
			zap.String("user_id", userID.String()),
			zap.Any("roles", evt.Payload["roles"]))
		return
	}

	// Resolve outlet IDs from event payload (many-to-many).
	outletIDs := parseOutletIDs(evt.Payload)

	// Fallback: use HQ outlet if no outlet_ids in event.
	if len(outletIDs) == 0 {
		homeOutlet, err := h.client.Outlet.Query().
			Where(outlet.TenantID(tenantID), outlet.IsHq(true), outlet.StatusEQ("active"),
				outlet.UseCaseNEQ("logistics"), outlet.UseCaseNEQ("warehouse")).
			First(ctx)
		if err != nil {
			homeOutlet, err = h.client.Outlet.Query().
				Where(outlet.TenantID(tenantID), outlet.StatusEQ("active"),
					outlet.UseCaseNEQ("logistics"), outlet.UseCaseNEQ("warehouse")).
				First(ctx)
		}
		if err == nil {
			outletIDs = []uuid.UUID{homeOutlet.ID}
		} else {
			h.logger.Warn("no active outlet found for tenant, cannot create StaffMember",
				zap.String("tenant_id", tenantID.String()))
			return
		}
	}

	existing, err := h.client.StaffMember.Query().
		Where(staffmember.TenantID(tenantID), staffmember.UserID(userID)).
		Only(ctx)
	if err == nil {
		upd := existing.Update()
		changed := false
		if existing.Name != name && name != "" {
			upd = upd.SetName(name)
			changed = true
		}
		if existing.Role != posRole {
			upd = upd.SetRole(posRole)
			upd = h.clearPINOnRaise(upd, existing, posRole)
			changed = true
		}
		if changed {
			_ = upd.Exec(ctx)
		}
		h.upsertStaffOutlets(ctx, existing.ID, tenantID, outletIDs)
		return
	}
	if !ent.IsNotFound(err) {
		h.logger.Warn("error querying StaffMember", zap.Error(err))
		return
	}

	// Racy against handleUserPINSet's create-fallback path when auth.user.created
	// and auth.user.pin_set land close together (e.g. tenant registration for the
	// owner/admin account) — upsert on the unique index instead of a plain Create
	// so the loser of the race no-ops instead of failing on the unique constraint.
	// Exec (not .ID): Postgres DO NOTHING has no RETURNING row on conflict, so
	// re-query below for the authoritative row either way.
	if createErr := h.client.StaffMember.Create().
		SetTenantID(tenantID).
		SetUserID(userID).
		SetName(name).
		SetRole(posRole).
		SetIsActive(true).
		SetPinFailedAttempts(0).
		OnConflictColumns(staffmember.FieldTenantID, staffmember.FieldUserID).
		DoNothing().
		Exec(ctx); createErr != nil {
		h.logger.Warn("failed to create StaffMember from auth event", zap.Error(createErr))
		return
	}
	created, err := h.client.StaffMember.Query().
		Where(staffmember.TenantID(tenantID), staffmember.UserID(userID)).
		Only(ctx)
	if err != nil {
		h.logger.Warn("StaffMember not found after upsert", zap.Error(err))
		return
	}
	h.upsertStaffOutlets(ctx, created.ID, tenantID, outletIDs)
}

// parseOutletIDs extracts outlet UUIDs from an event payload.
// Reads the new outlet_ids array first, falls back to the legacy outlet_id string.
func parseOutletIDs(payload map[string]interface{}) []uuid.UUID {
	var ids []uuid.UUID
	if raw, ok := payload["outlet_ids"].([]interface{}); ok {
		for _, v := range raw {
			if str, ok := v.(string); ok {
				if id, err := uuid.Parse(str); err == nil {
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		if str, ok := payload["outlet_id"].(string); ok && str != "" {
			if id, err := uuid.Parse(str); err == nil {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// upsertStaffOutlets creates StaffOutlet rows for each outletID (idempotent via ON CONFLICT DO NOTHING).
func (h *AuthEventHandler) upsertStaffOutlets(ctx context.Context, staffMemberID, tenantID uuid.UUID, outletIDs []uuid.UUID) {
	for i, outletID := range outletIDs {
		err := h.client.StaffOutlet.Create().
			SetTenantID(tenantID).
			SetStaffMemberID(staffMemberID).
			SetOutletID(outletID).
			SetIsHomeOutlet(i == 0).
			OnConflict().DoNothing().Exec(ctx)
		if err != nil {
			h.logger.Warn("failed to upsert StaffOutlet",
				zap.String("staff_member_id", staffMemberID.String()),
				zap.String("outlet_id", outletID.String()),
				zap.Error(err))
		}
	}
}

// mapSSORoleToPOS converts SSO-level roles from the event payload to a POS role code.
// Returns "" for roles that are not POS-relevant (rider, driver, customer, etc.).
func mapSSORoleToPOS(payload map[string]interface{}) string {
	roles, _ := payload["roles"].([]interface{})
	for _, r := range roles {
		role, _ := r.(string)
		switch role {
		case "admin", "superuser":
			return "admin" // unrestricted tenant/platform access
		case "manager":
			return "manager" // RBAC-scoped access, different from admin
		case "staff":
			return "cashier"
		// technician works a services outlet's production board (printing, garage, laundry);
		// barista runs a cafe bar. Both are seeded POS system roles with PIN login.
		case "cashier", "waiter", "receptionist", "kitchen", "bar", "barista", "pharmacist", "stylist", "therapist", "technician":
			return role
		case "viewer":
			return "" // viewer accesses POS via SSO only, no PIN login
		case "rider", "driver", "delivery_coordinator", "customer", "member":
			return ""
		}
	}
	return ""
}

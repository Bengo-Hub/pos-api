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
	"github.com/bengobox/pos-service/internal/ent/user"
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

	// Find existing user — if absent, create it so the StaffMember name patch below can run.
	u, err := h.client.User.Query().
		Where(user.AuthServiceUserIDEQ(authServiceUserID)).
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return fmt.Errorf("query user: %w", err)
		}
		// User row doesn't exist (e.g. after a DB reset). Resolve tenant then create it.
		tenantID := evt.TenantID
		tenantSlug, _ := evt.Payload["tenant_slug"].(string)
		if tenantSlug != "" && h.tenantSyncer != nil {
			if tid, syncErr := h.tenantSyncer.SyncTenant(ctx, tenantSlug); syncErr == nil {
				tenantID = tid
			}
		}
		if tenantID == uuid.Nil {
			h.logger.Debug("user not found and no tenant_id in updated event, skipping",
				zap.String("user_id", userIDStr))
			return nil
		}
		created, createErr := h.client.User.Create().
			SetID(authServiceUserID).
			SetAuthServiceUserID(authServiceUserID).
			SetTenantID(tenantID).
			SetEmail(email).
			SetFullName(fullName).
			SetStatus("active").
			SetSyncStatus("synced").
			SetSyncAt(time.Now()).
			Save(ctx)
		if createErr != nil {
			return fmt.Errorf("create user from updated event: %w", createErr)
		}
		h.logger.Info("user created from auth.user.updated event (backfill)",
			zap.String("user_id", userIDStr))
		u = created
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

	// Patch StaffMember fields if the event carries them.
	smUpdate := h.client.StaffMember.Update().
		Where(staffmember.TenantID(u.TenantID), staffmember.UserID(authServiceUserID))
	smChanged := false
	if fullName != "" {
		smUpdate = smUpdate.SetName(fullName)
		smChanged = true
	}
	// Update role if the event includes roles — ensures re-provisioning corrects stale role mappings.
	if posRole := mapSSORoleToPOS(evt.Payload); posRole != "" {
		smUpdate = smUpdate.SetRole(posRole)
		smChanged = true
	}
	if smChanged {
		_ = smUpdate.Exec(ctx)
	}

	// Upsert StaffOutlet rows for each outlet in the event payload.
	outletIDs := parseOutletIDs(evt.Payload)
	if len(outletIDs) > 0 {
		sm, smErr := h.client.StaffMember.Query().
			Where(staffmember.TenantID(u.TenantID), staffmember.UserID(authServiceUserID)).
			Only(ctx)
		if smErr == nil {
			h.upsertStaffOutlets(ctx, sm.ID, u.TenantID, outletIDs)
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

	existing, err := h.client.StaffMember.Query().
		Where(staffmember.TenantID(evt.TenantID), staffmember.UserID(authServiceUserID)).
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return fmt.Errorf("query StaffMember: %w", err)
		}
		// StaffMember doesn't exist — create it so we can set the PIN.
		tenantID := evt.TenantID
		name := ""
		role := "cashier" // safe fallback

		// Use full_name and role from event payload (auth-api now includes both).
		if fn, _ := evt.Payload["full_name"].(string); fn != "" {
			name = fn
		}
		if r := mapSSORoleToPOS(evt.Payload); r != "" {
			role = r
		}

		// Prefer tenant from local User record (most accurate tenantID).
		u, uErr := h.client.User.Query().
			Where(user.AuthServiceUserIDEQ(authServiceUserID)).
			Only(ctx)
		if uErr == nil {
			if name == "" {
				name = u.FullName
			}
			tenantID = u.TenantID
		}
		if name == "" {
			name = userIDStr[:8]
		}

		// Ensure tenant + outlets exist locally. The pin_set event now carries
		// tenant_slug so we can bootstrap from auth-api if needed.
		tenantSlug, _ := evt.Payload["tenant_slug"].(string)
		if tenantSlug != "" && h.tenantSyncer != nil {
			if syncedID, sErr := h.tenantSyncer.SyncTenant(ctx, tenantSlug); sErr == nil {
				tenantID = syncedID
			}
		}

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
		created, createErr := h.client.StaffMember.Create().
			SetTenantID(tenantID).
			SetUserID(authServiceUserID).
			SetName(name).
			SetRole(role).
			SetIsActive(true).
			SetPinFailedAttempts(0).
			Save(ctx)
		if createErr != nil {
			return fmt.Errorf("create StaffMember for PIN set: %w", createErr)
		}
		// Assign home outlet via join table.
		_ = h.client.StaffOutlet.Create().
			SetTenantID(tenantID).
			SetStaffMemberID(created.ID).
			SetOutletID(homeOutlet.ID).
			SetIsHomeOutlet(true).
			OnConflict().DoNothing().Exec(ctx)
		// Now try again
		existing, err = h.client.StaffMember.Query().
			Where(staffmember.TenantID(tenantID), staffmember.UserID(authServiceUserID)).
			Only(ctx)
		if err != nil {
			return fmt.Errorf("StaffMember not found after upsert: %w", err)
		}
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

	created, err := h.client.StaffMember.Create().
		SetTenantID(tenantID).
		SetUserID(userID).
		SetName(name).
		SetRole(posRole).
		SetIsActive(true).
		SetPinFailedAttempts(0).
		Save(ctx)
	if err != nil {
		h.logger.Warn("failed to create StaffMember from auth event", zap.Error(err))
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
		case "cashier", "waiter", "receptionist", "kitchen", "bar", "pharmacist", "stylist", "therapist":
			return role
		case "viewer":
			return "" // viewer accesses POS via SSO only, no PIN login
		case "rider", "driver", "delivery_coordinator", "technician", "customer", "member":
			return ""
		}
	}
	return ""
}

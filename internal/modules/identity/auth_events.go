package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/ent/user"
)

// AuthEventHandler handles auth-service user events for proactive user sync.
type AuthEventHandler struct {
	client       *ent.Client
	tenantSyncer interface {
		SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
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

// authUserEvent represents the shared-events envelope for auth user events.
type authUserEvent struct {
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      uuid.UUID              `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// SubscribeToAuthEvents subscribes to auth-service user events via NATS.
func (h *AuthEventHandler) SubscribeToAuthEvents(nc *nats.Conn) error {
	if nc == nil {
		h.logger.Warn("NATS connection not available, skipping auth event subscriptions")
		return nil
	}

	// Subscribe to auth.user.created
	_, err := nc.Subscribe("auth.user.created", func(msg *nats.Msg) {
		var evt authUserEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("failed to unmarshal auth.user.created event", zap.Error(err))
			return
		}

		ctx := context.Background()
		if err := h.handleUserCreated(ctx, &evt); err != nil {
			h.logger.Error("failed to handle auth.user.created event", zap.Error(err))
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("subscribe to auth.user.created: %w", err)
	}

	// Subscribe to auth.user.updated
	_, err = nc.Subscribe("auth.user.updated", func(msg *nats.Msg) {
		var evt authUserEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("failed to unmarshal auth.user.updated event", zap.Error(err))
			return
		}

		ctx := context.Background()
		if err := h.handleUserUpdated(ctx, &evt); err != nil {
			h.logger.Error("failed to handle auth.user.updated event", zap.Error(err))
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("subscribe to auth.user.updated: %w", err)
	}

	// Subscribe to auth.user.pin_set
	_, err = nc.Subscribe("auth.user.pin_set", func(msg *nats.Msg) {
		var evt authUserEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			h.logger.Error("failed to unmarshal auth.user.pin_set event", zap.Error(err))
			return
		}

		ctx := context.Background()
		if err := h.handleUserPINSet(ctx, &evt); err != nil {
			h.logger.Error("failed to handle auth.user.pin_set event", zap.Error(err))
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("subscribe to auth.user.pin_set: %w", err)
	}

	h.logger.Info("auth event subscriptions active",
		zap.Strings("subjects", []string{"auth.user.created", "auth.user.updated", "auth.user.pin_set"}))
	return nil
}

func (h *AuthEventHandler) handleUserCreated(ctx context.Context, evt *authUserEvent) error {
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

func (h *AuthEventHandler) handleUserUpdated(ctx context.Context, evt *authUserEvent) error {
	userIDStr, _ := evt.Payload["user_id"].(string)
	email, _ := evt.Payload["email"].(string)
	fullName, _ := evt.Payload["full_name"].(string)

	authServiceUserID, err := uuid.Parse(userIDStr)
	if err != nil {
		return fmt.Errorf("invalid user_id %q: %w", userIDStr, err)
	}

	// Find existing user
	u, err := h.client.User.Query().
		Where(user.AuthServiceUserIDEQ(authServiceUserID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			h.logger.Debug("user not found for update event, will be created on first login",
				zap.String("user_id", userIDStr))
			return nil
		}
		return fmt.Errorf("query user: %w", err)
	}

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

	// Patch StaffMember name if it changed
	if fullName != "" {
		_ = h.client.StaffMember.Update().
			Where(staffmember.TenantID(u.TenantID), staffmember.UserID(authServiceUserID)).
			SetName(fullName).
			Exec(ctx)
	}

	h.logger.Info("user updated from auth.user.updated event",
		zap.String("user_id", userIDStr),
		zap.String("email", email))
	return nil
}

func (h *AuthEventHandler) handleUserPINSet(ctx context.Context, evt *authUserEvent) error {
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
		// StaffMember doesn't exist — create it with HQ outlet as default
		tenantID := evt.TenantID
		name := ""
		// Try to get name from User record
		u, uErr := h.client.User.Query().
			Where(user.AuthServiceUserIDEQ(authServiceUserID)).
			Only(ctx)
		if uErr == nil {
			name = u.FullName
			tenantID = u.TenantID
		}
		if name == "" {
			name = userIDStr[:8]
		}
		h.upsertStaffMember(ctx, authServiceUserID, tenantID, name, evt)
		// Now try again
		existing, err = h.client.StaffMember.Query().
			Where(staffmember.TenantID(tenantID), staffmember.UserID(authServiceUserID)).
			Only(ctx)
		if err != nil {
			return fmt.Errorf("StaffMember not found after upsert: %w", err)
		}
	}

	if err := existing.Update().SetPinHash(pinHash).Exec(ctx); err != nil {
		return fmt.Errorf("update PIN hash: %w", err)
	}

	h.logger.Info("staff PIN updated from auth.user.pin_set event",
		zap.String("user_id", userIDStr))
	return nil
}

// upsertStaffMember creates a StaffMember for the given user if one does not exist,
// or updates name/role if they changed. This is idempotent.
// StaffMember.outlet_id is set to the first non-logistics, active outlet for the tenant
// (preferring HQ). Staff visibility is tenant-wide at PIN login — outlet_id here is a
// "home outlet" default for session creation, not a login restriction.
// Roles that are not POS-relevant (rider, driver, customer, technician, viewer,
// delivery_coordinator) are skipped — they have no business logging into a POS terminal.
func (h *AuthEventHandler) upsertStaffMember(ctx context.Context, userID, tenantID uuid.UUID, name string, evt *authUserEvent) {
	// Skip non-POS roles that have no business accessing a POS terminal.
	posRole := mapSSORoleToPOS(evt.Payload)
	if posRole == "" {
		h.logger.Debug("skipping StaffMember upsert for non-POS role",
			zap.String("user_id", userID.String()),
			zap.Any("roles", evt.Payload["roles"]))
		return
	}

	// Find the best outlet for this tenant: prefer HQ, then any non-logistics active outlet.
	// Logistics outlets (use_case=logistics/warehouse) don't run POS terminals.
	homeOutlet, err := h.client.Outlet.Query().
		Where(outlet.TenantID(tenantID), outlet.IsHq(true), outlet.StatusEQ("active"),
			outlet.UseCaseNEQ("logistics"), outlet.UseCaseNEQ("warehouse")).
		First(ctx)
	if err != nil {
		homeOutlet, err = h.client.Outlet.Query().
			Where(outlet.TenantID(tenantID), outlet.StatusEQ("active"),
				outlet.UseCaseNEQ("logistics"), outlet.UseCaseNEQ("warehouse")).
			First(ctx)
		if err != nil {
			h.logger.Warn("no active outlet found for tenant, cannot create StaffMember",
				zap.String("tenant_id", tenantID.String()))
			return
		}
	}

	// Upsert StaffMember — idempotent on (tenant_id, user_id)
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
		return
	}
	if !ent.IsNotFound(err) {
		h.logger.Warn("error querying StaffMember", zap.Error(err))
		return
	}

	_, err = h.client.StaffMember.Create().
		SetTenantID(tenantID).
		SetOutletID(homeOutlet.ID).
		SetUserID(userID).
		SetName(name).
		SetRole(posRole).
		SetIsActive(true).
		SetPinFailedAttempts(0).
		Save(ctx)
	if err != nil {
		h.logger.Warn("failed to create StaffMember from auth event", zap.Error(err))
	}
}

// mapSSORoleToPOS converts SSO-level roles from the event payload to a POS role code.
// Returns "" for roles that are not POS-relevant (rider, driver, customer, etc.) —
// the caller must skip StaffMember creation when "" is returned.
func mapSSORoleToPOS(payload map[string]interface{}) string {
	roles, _ := payload["roles"].([]interface{})
	for _, r := range roles {
		role, _ := r.(string)
		switch role {
		case "admin", "superuser":
			return "manager"
		case "manager":
			return "manager"
		case "staff":
			return "cashier"
		case "cashier", "waiter", "receptionist", "kitchen", "bar", "pharmacist":
			return role
		case "viewer":
			return "viewer"
		// Non-POS roles — these users don't log into POS terminals.
		case "rider", "driver", "delivery_coordinator", "technician", "customer", "member":
			return ""
		}
	}
	// Unknown role with no explicit POS mapping — skip rather than default to cashier
	// to avoid ghost staff members for non-POS service users.
	return ""
}

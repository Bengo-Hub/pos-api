package identity

import (
	"context"
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
		if _, subErr := js.Subscribe(s.subject, func(msg *nats.Msg) {
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
		); subErr != nil {
			h.logger.Warn("auth user events: subscribe failed",
				zap.String("subject", s.subject), zap.Error(subErr))
		}
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

	// Patch StaffMember outlet if outlet_id is present in event
	outletIDStr, _ := evt.Payload["outlet_id"].(string)
	if outletIDStr != "" {
		if outletID, err := uuid.Parse(outletIDStr); err == nil {
			_ = h.client.StaffMember.Update().
				Where(staffmember.TenantID(u.TenantID), staffmember.UserID(authServiceUserID)).
				SetOutletID(outletID).
				Exec(ctx)
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
		// The pin_set event has no roles payload, so we can't use upsertStaffMember
		// here (it would see no roles and skip creation). Create directly instead.
		tenantID := evt.TenantID
		name := ""
		role := "cashier" // safe fallback
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
		// Use role from event payload if available (auth-api includes roles in pin_set events).
		if r := mapSSORoleToPOS(evt.Payload); r != "" {
			role = r
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
		if oErr != nil {
			return fmt.Errorf("no active outlet found for tenant %s, cannot create StaffMember: %w", tenantID, oErr)
		}
		_, createErr := h.client.StaffMember.Create().
			SetTenantID(tenantID).
			SetOutletID(homeOutlet.ID).
			SetUserID(authServiceUserID).
			SetName(name).
			SetRole(role).
			SetIsActive(true).
			SetPinFailedAttempts(0).
			Save(ctx)
		if createErr != nil {
			return fmt.Errorf("create StaffMember for PIN set: %w", createErr)
		}
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
func (h *AuthEventHandler) upsertStaffMember(ctx context.Context, userID, tenantID uuid.UUID, name string, evt *sharedevents.Event) {
	posRole := mapSSORoleToPOS(evt.Payload)
	if posRole == "" {
		h.logger.Debug("skipping StaffMember upsert for non-POS role",
			zap.String("user_id", userID.String()),
			zap.Any("roles", evt.Payload["roles"]))
		return
	}

	// Find best outlet: prefer HQ, then any non-logistics active outlet.
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

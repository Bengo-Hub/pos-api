package handlers

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/platform/clientinfo"
)

// offlineOriginInput is who rang up an offline sale and on which device, captured by pos-ui at
// ring-up time and replayed with the sale when the queue syncs. Without it the sale was credited
// to whoever happened to be logged in on that browser when it came back online, which can be
// hours later and a different person (order 002384, urban-loft, 2026-10-18).
type offlineOriginInput struct {
	RungUpByUserID string `json:"rung_up_by_user_id"`
	RungUpByName   string `json:"rung_up_by_name"`
	ClientID       string `json:"client_id"`
	DeviceName     string `json:"device_name"`
	UserAgent      string `json:"user_agent"`
}

// offlineMetaKey holds the origin block on an order's metadata.
const offlineMetaKey = "offline"

// resolveOfflineOrigin decides who an offline sale is credited to and builds its metadata block.
//
// The sale is credited to the staff member who rang it up only when that id is an ACTIVE staff
// member of this tenant (the browser verified their PIN locally while offline). Anything else
// (an unknown id, an inactive member, an old queued sale without the field) falls back to the
// logged-in user who synced it, exactly as before. Either way the block records both people so
// an audit can see the difference.
func (h *POSOrderHandler) resolveOfflineOrigin(ctx context.Context, tid, syncUserID uuid.UUID, in *offlineOriginInput, offlineAt time.Time) (uuid.UUID, map[string]any) {
	creator := syncUserID
	block := map[string]any{
		"synced_by_user_id":  syncUserID.String(),
		"synced_at":          time.Now().UTC().Format(time.RFC3339),
		"offline_created_at": offlineAt.UTC().Format(time.RFC3339),
		"rung_up_by_verified": false,
	}
	if in == nil {
		return creator, block
	}
	if dev := clientinfo.FromReported(in.ClientID, in.DeviceName, in.UserAgent); !dev.IsZero() {
		block["rung_up_on"] = dev.Map()
	}
	claimed, err := uuid.Parse(in.RungUpByUserID)
	if err != nil || claimed == uuid.Nil {
		return creator, block
	}
	block["rung_up_by_user_id"] = claimed.String()
	if name := clientinfo.CleanDeviceName(in.RungUpByName); name != "" {
		block["rung_up_by_name"] = name
	}
	member, err := h.client.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserID(claimed), entstaff.IsActive(true)).
		First(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			h.log.Warn("offline origin: staff lookup failed", zap.Error(err))
		}
		return creator, block
	}
	block["rung_up_by_verified"] = true
	block["rung_up_by_name"] = member.Name
	return claimed, block
}

// recordOfflineSync writes an order.offline_sync audit entry so Loss Prevention shows every
// offline sale with who rang it up, who synced it and the devices involved.
func (h *POSOrderHandler) recordOfflineSync(ctx context.Context, order *ent.POSOrder, syncUserID uuid.UUID, block map[string]any) {
	if h.auditSvc == nil || order == nil {
		return
	}
	amount := order.TotalAmount
	oid := order.OutletID
	reason := "offline sale synced"
	if by, _ := block["rung_up_by_user_id"].(string); by != "" && by != syncUserID.String() {
		reason = "offline sale synced under a different login"
	}
	h.auditSvc.Record(ctx, audit.Entry{
		TenantID:    order.TenantID,
		OutletID:    &oid,
		ActorUserID: syncUserID,
		Action:      "order.offline_sync",
		EntityType:  "pos_order",
		EntityID:    order.ID.String(),
		Reason:      reason,
		After:       block,
		Amount:      &amount,
	})
}

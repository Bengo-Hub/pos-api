package handlers

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
)

// PIN sign-in audit actions. Before 2026-10-23 sign-ins were not audited at all, so an
// investigation could not tell whose PIN was used where; the entries carry the device, browser
// and IP via audit.TrailHook.
const (
	actionPINLogin  = "auth.pin_login"
	actionPINFailed = "auth.pin_failed"
)

// recordPINSignIn audits a successful PIN sign-in and records the browser on the outlet device.
func (h *PINAuthHandler) recordPINSignIn(ctx context.Context, member *ent.StaffMember, outletID uuid.UUID, method string) {
	touchOutletDeviceClient(ctx, h.client, h.log, member.TenantID, outletID, member.UserID, member.Name)
	staffID := member.ID
	h.auditSvc.Record(ctx, audit.Entry{
		TenantID:     member.TenantID,
		OutletID:     optionalOutlet(outletID),
		ActorUserID:  member.UserID,
		ActorStaffID: &staffID,
		Action:       actionPINLogin,
		EntityType:   "staff_member",
		EntityID:     member.ID.String(),
		Reason:       method,
	})
}

// recordPINFailedAttempt audits a rejected PIN. member is nil when the PIN matched nobody (the
// "enter PIN only" identify flow), which is recorded against the zero user id.
func (h *PINAuthHandler) recordPINFailedAttempt(ctx context.Context, tid uuid.UUID, member *ent.StaffMember, outletID uuid.UUID, method string) {
	entry := audit.Entry{
		TenantID:    tid,
		OutletID:    optionalOutlet(outletID),
		ActorUserID: uuid.Nil,
		Action:      actionPINFailed,
		Reason:      method + ": PIN matched no staff member",
	}
	if member != nil {
		staffID := member.ID
		entry.ActorUserID = member.UserID
		entry.ActorStaffID = &staffID
		entry.EntityType = "staff_member"
		entry.EntityID = member.ID.String()
		entry.Reason = method + ": wrong PIN"
	}
	h.auditSvc.Record(ctx, entry)
}

// headerOutletID is the outlet the terminal selected (X-Outlet-ID), or uuid.Nil.
func headerOutletID(r *http.Request) uuid.UUID {
	id, _ := uuid.Parse(r.Header.Get("X-Outlet-ID"))
	return id
}

func optionalOutlet(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

package handlers

import (
	"testing"

	"github.com/google/uuid"
)

// These tests lock the per-order decision rules the bulk endpoints share with the single
// DeleteDraft / VoidOrder handlers (pure functions — no ent client needed, mirroring the
// internal/modules/promotions test approach). If either rule drifts, the single and bulk
// surfaces would disagree about which orders are deletable/voidable.

func TestDraftDeleteSkipReason(t *testing.T) {
	owner := uuid.New()
	other := uuid.New()

	tests := []struct {
		name         string
		status       string
		orderUserID  uuid.UUID
		callerID     uuid.UUID
		canDeleteAny bool
		want         string
	}{
		{"own draft without manage is deletable", "draft", owner, owner, false, ""},
		{"someone else's draft without manage is refused", "draft", owner, other, false, "not_owner"},
		{"someone else's draft WITH manage is deletable", "draft", owner, other, true, ""},
		{"own draft with manage is deletable", "draft", owner, owner, true, ""},
		// Only draft-status orders are hard-deletable — finalized/active sales carry
		// ledger/eTIMS/kitchen state and must be voided or returned instead. The status
		// gate applies BEFORE ownership: even a manager may not delete a non-draft.
		{"open order is never deletable", "open", owner, owner, true, "not_draft"},
		{"pending_payment order is never deletable", "pending_payment", owner, owner, true, "not_draft"},
		{"completed sale is never deletable", "completed", owner, owner, true, "not_draft"},
		{"voided order is never deletable", "voided", owner, owner, true, "not_draft"},
		{"cancelled order is never deletable", "cancelled", other, other, false, "not_draft"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := draftDeleteSkipReason(tt.status, tt.orderUserID, tt.callerID, tt.canDeleteAny)
			if got != tt.want {
				t.Fatalf("draftDeleteSkipReason(%q, owner=%v, caller=%v, manage=%v) = %q, want %q",
					tt.status, tt.orderUserID, tt.callerID, tt.canDeleteAny, got, tt.want)
			}
		})
	}
}

func TestVoidSkipReason(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		// Unsettled orders are voidable.
		{"draft", ""},
		{"open", ""},
		{"pending_payment", ""},
		// Idempotency: an already-voided order is a skip, never an error.
		{"voided", "already_voided"},
		// Finalized sales are posted to the ledger and transmitted to KRA eTIMS — they must
		// be reversed via a return/refund (ledger reversal + eTIMS credit note), never a
		// bare status flip.
		{"completed", "finalized"},
		{"paid", "finalized"},
		{"closed", "finalized"},
		// Mirrors the single VoidOrder handler exactly: it only guards voided + finalized
		// statuses, so cancelled/refunded remain voidable there and must here too.
		{"cancelled", ""},
		{"refunded", ""},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := voidSkipReason(tt.status); got != tt.want {
				t.Fatalf("voidSkipReason(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

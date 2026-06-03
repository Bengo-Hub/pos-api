package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	enteventbooking "github.com/bengobox/pos-service/internal/ent/eventbooking"
	entmeal "github.com/bengobox/pos-service/internal/ent/mealentitlement"
)

type generateMealCardsInput struct {
	// MealPeriods included in the package for each conference day (sourced from the
	// inventory Bundle's MEAL_PERIOD components by the caller). e.g. ["breakfast","lunch","pm_break"].
	MealPeriods []string `json:"meal_periods"`
	// DelegateRefs names/badges; when empty, anonymous vouchers are generated using DelegateCount.
	DelegateRefs []string `json:"delegate_refs"`
}

// GenerateMealCards handles POST /{tenantID}/hotel/events/{id}/generate-mealcards.
// Creates one MealEntitlement per delegate × conference-day × meal-period.
func (h *HotelHandler) GenerateMealCards(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid event id", http.StatusBadRequest)
		return
	}
	var in generateMealCardsInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(in.MealPeriods) == 0 {
		jsonError(w, "meal_periods is required (e.g. breakfast, lunch)", http.StatusBadRequest)
		return
	}

	event, err := h.client.EventBooking.Query().
		Where(enteventbooking.ID(id), enteventbooking.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "event not found", http.StatusNotFound)
		return
	}

	// Additive / idempotent generation (top-up): re-running this endpoint after more
	// delegates join, a new meal period is added, or conference days are extended will
	// only issue the MISSING cards — never duplicate existing ones. Per (day, period):
	//   - named delegate_refs → issue only for refs that don't yet have a card that slot
	//   - anonymous           → issue (delegate_count - existing) additional slots
	if event.ConferenceDays < 1 {
		jsonError(w, "event has no conference_days", http.StatusBadRequest)
		return
	}
	useNamed := len(in.DelegateRefs) > 0
	if !useNamed && event.DelegateCount < 1 {
		jsonError(w, "event has no delegate_count and no delegate_refs", http.StatusBadRequest)
		return
	}

	baseDay := time.Date(event.StartAt.Year(), event.StartAt.Month(), event.StartAt.Day(), 0, 0, 0, 0, event.StartAt.Location())

	tx, err := h.client.Tx(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	issueCard := func(delegate string, day time.Time, period string) error {
		_, cErr := tx.MealEntitlement.Create().
			SetTenantID(tid).
			SetEventBookingID(id).
			SetDelegateRef(delegate).
			SetConferenceDay(day).
			SetMealPeriod(entmeal.MealPeriod(period)).
			SetCode("MC-" + uuid.NewString()[:10]).
			SetValidWindowStart(day).
			SetValidWindowEnd(day.AddDate(0, 0, 1)).
			Save(r.Context())
		return cErr
	}

	created := 0
	for d := 0; d < event.ConferenceDays; d++ {
		day := baseDay.AddDate(0, 0, d)
		dayStart := day
		dayEnd := day.AddDate(0, 0, 1)
		for _, period := range in.MealPeriods {
			// Existing cards already issued for this exact day + period.
			existingCards, qErr := tx.MealEntitlement.Query().
				Where(
					entmeal.TenantID(tid),
					entmeal.EventBookingID(id),
					entmeal.MealPeriodEQ(entmeal.MealPeriod(period)),
					entmeal.ConferenceDayGTE(dayStart),
					entmeal.ConferenceDayLT(dayEnd),
				).All(r.Context())
			if qErr != nil {
				_ = tx.Rollback()
				jsonError(w, "internal error", http.StatusInternalServerError)
				return
			}

			if useNamed {
				have := make(map[string]struct{}, len(existingCards))
				for _, c := range existingCards {
					have[c.DelegateRef] = struct{}{}
				}
				for _, ref := range in.DelegateRefs {
					if _, ok := have[ref]; ok {
						continue
					}
					if cErr := issueCard(ref, day, period); cErr != nil {
						_ = tx.Rollback()
						h.log.Error("generate meal card failed", zap.Error(cErr))
						jsonError(w, "failed to generate meal cards", http.StatusInternalServerError)
						return
					}
					created++
				}
			} else {
				missing := event.DelegateCount - len(existingCards)
				for i := 0; i < missing; i++ {
					if cErr := issueCard(fmt.Sprintf("Delegate %d", len(existingCards)+i+1), day, period); cErr != nil {
						_ = tx.Rollback()
						h.log.Error("generate meal card failed", zap.Error(cErr))
						jsonError(w, "failed to generate meal cards", http.StatusInternalServerError)
						return
					}
					created++
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		_ = h.publisher.PublishConferenceMealcardIssued(r.Context(), tid, map[string]any{
			"event_booking_id": id,
			"cards_issued":     created,
			"conference_days":  event.ConferenceDays,
			"meal_periods":     in.MealPeriods,
		})
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"event_booking_id": id, "cards_issued": created})
}

type redeemMealCardInput struct {
	RedeemedBy string `json:"redeemed_by"`
	POSOrderID string `json:"pos_order_id"`
}

// RedeemMealCard handles POST /{tenantID}/hotel/mealcards/{code}/redeem.
// Enforces one-time redemption + validity window; emits conference.mealcard.redeemed
// so inventory-api can backflush the meal's BOM.
func (h *HotelHandler) RedeemMealCard(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	code := chi.URLParam(r, "code")
	if code == "" {
		jsonError(w, "code is required", http.StatusBadRequest)
		return
	}
	var in redeemMealCardInput
	_ = json.NewDecoder(r.Body).Decode(&in)

	card, err := h.client.MealEntitlement.Query().
		Where(entmeal.TenantID(tid), entmeal.Code(code)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "meal card not found", http.StatusNotFound)
		return
	}
	if card.Status != entmeal.StatusIssued {
		jsonError(w, "meal card already "+string(card.Status), http.StatusConflict)
		return
	}
	now := time.Now()
	if card.ValidWindowStart != nil && now.Before(*card.ValidWindowStart) {
		jsonError(w, "meal card not yet valid", http.StatusConflict)
		return
	}
	if card.ValidWindowEnd != nil && now.After(*card.ValidWindowEnd) {
		_, _ = h.client.MealEntitlement.UpdateOne(card).SetStatus(entmeal.StatusExpired).Save(r.Context())
		jsonError(w, "meal card has expired", http.StatusConflict)
		return
	}

	upd := h.client.MealEntitlement.UpdateOne(card).
		SetStatus(entmeal.StatusRedeemed).
		SetRedeemedAt(now)
	if oidStr := httpware.GetOutletID(r.Context()); oidStr != "" {
		if oid, perr := uuid.Parse(oidStr); perr == nil {
			upd = upd.SetRedeemedOutletID(oid)
		}
	}
	if by, perr := uuid.Parse(in.RedeemedBy); perr == nil {
		upd = upd.SetRedeemedBy(by)
	}
	var posOrderID *uuid.UUID
	if oid, perr := uuid.Parse(in.POSOrderID); perr == nil {
		upd = upd.SetPosOrderID(oid)
		posOrderID = &oid
	}
	redeemed, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("redeem meal card failed", zap.Error(err))
		jsonError(w, "failed to redeem meal card", http.StatusInternalServerError)
		return
	}

	if h.publisher != nil {
		// Include the inventory Bundle id + tenant so inventory-api can resolve the meal
		// period's BundleComponent and backflush that meal's BOM on redemption.
		payload := map[string]any{
			"meal_entitlement_id": redeemed.ID,
			"tenant_id":           tid.String(),
			"event_booking_id":    redeemed.EventBookingID,
			"meal_period":         redeemed.MealPeriod,
			"pos_order_id":        posOrderID,
			"redeemed_at":         now,
		}
		if ev, evErr := h.client.EventBooking.Get(r.Context(), redeemed.EventBookingID); evErr == nil && ev.InventoryBundleID != nil {
			payload["inventory_bundle_id"] = ev.InventoryBundleID.String()
		}
		_ = h.publisher.PublishConferenceMealcardRedeemed(r.Context(), tid, payload)
	}

	jsonOK(w, redeemed)
}

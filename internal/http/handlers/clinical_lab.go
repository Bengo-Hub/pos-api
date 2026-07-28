package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entexam "github.com/bengobox/pos-service/internal/ent/examinationrecord"
	entlaborder "github.com/bengobox/pos-service/internal/ent/laborder"
	entlabline "github.com/bengobox/pos-service/internal/ent/laborderline"
	entvisit "github.com/bengobox/pos-service/internal/ent/patientvisit"
)

type labOrderWithLines struct {
	*ent.LabOrder
	Lines []*ent.LabOrderLine `json:"lines"`
}

func (h *ClinicalHandler) labOrdersForVisit(r *http.Request, visitID uuid.UUID) ([]labOrderWithLines, error) {
	orders, err := h.db.LabOrder.Query().
		Where(entlaborder.VisitID(visitID)).
		Order(ent.Desc(entlaborder.FieldOrderedAt)).
		All(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]labOrderWithLines, 0, len(orders))
	for _, o := range orders {
		lines, _ := h.db.LabOrderLine.Query().Where(entlabline.LabOrderID(o.ID)).All(r.Context())
		out = append(out, labOrderWithLines{LabOrder: o, Lines: lines})
	}
	return out, nil
}

// ListLabOrders handles GET /{tenantID}/pos/clinical/lab-orders?status=ordered
func (h *ClinicalHandler) ListLabOrders(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.LabOrder.Query().Where(entlaborder.TenantID(tid))
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entlaborder.StatusEQ(entlaborder.Status(status)))
	} else {
		// The Lab module never shows unpaid work: an awaiting_payment order only becomes visible
		// once its bill is settled (see ActivateLabOrderIfPaid). Callers that specifically want to
		// inspect unpaid orders can still ask for ?status=awaiting_payment.
		q = q.Where(entlaborder.StatusNEQ(entlaborder.StatusAwaitingPayment))
	}
	rows, err := q.Order(ent.Desc(entlaborder.FieldOrderedAt)).Limit(100).All(r.Context())
	if err != nil {
		jsonError(w, "failed to list lab orders", http.StatusInternalServerError)
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		lines, _ := h.db.LabOrderLine.Query().Where(entlabline.LabOrderID(o.ID)).All(r.Context())
		visit, _ := h.db.PatientVisit.Get(r.Context(), o.VisitID)
		entry := map[string]any{"lab_order": o, "lines": lines}
		if visit != nil {
			if patient, err := h.db.Patient.Get(r.Context(), visit.PatientID); err == nil {
				entry["patient_name"] = patient.FullName
				entry["visit_number"] = visit.VisitNumber
			}
		}
		out = append(out, entry)
	}
	jsonOK(w, map[string]any{"data": out})
}

type labResultLineInput struct {
	LineID         string `json:"line_id"`
	Result         string `json:"result"`
	Unit           string `json:"unit,omitempty"`
	ReferenceRange string `json:"reference_range,omitempty"`
	Flag           string `json:"flag,omitempty"` // normal | abnormal | critical
	Notes          string `json:"notes,omitempty"`
}

// SubmitLabResults handles POST /{tenantID}/pos/clinical/lab-orders/{labOrderID}/results
// Records results per line, marks the order completed once every line has a result, and moves
// the visit to lab_complete so it re-surfaces in the Examination queue for the doctor to review.
func (h *ClinicalHandler) SubmitLabResults(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	loID, err := uuid.Parse(chi.URLParam(r, "labOrderID"))
	if err != nil {
		jsonError(w, "invalid lab_order_id", http.StatusBadRequest)
		return
	}
	order, err := h.db.LabOrder.Query().Where(entlaborder.ID(loID), entlaborder.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "lab order not found", http.StatusNotFound)
		return
	}
	visit, err := h.db.PatientVisit.Get(r.Context(), order.VisitID)
	if err != nil {
		jsonError(w, "visit not found", http.StatusNotFound)
		return
	}
	if !h.requireModule(w, r, visit.OutletID, moduleLab) {
		return
	}

	var body struct {
		Lines []labResultLineInput `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	var userID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		userID, _ = uuid.Parse(claims.Subject)
	}
	now := time.Now()

	for _, l := range body.Lines {
		lineID, err := uuid.Parse(l.LineID)
		if err != nil {
			continue
		}
		upd := h.db.LabOrderLine.UpdateOneID(lineID).
			SetResult(l.Result).
			SetUnit(l.Unit).
			SetReferenceRange(l.ReferenceRange).
			SetResultedBy(userID).
			SetResultedAt(now)
		if l.Flag != "" {
			upd = upd.SetFlag(entlabline.Flag(l.Flag))
		} else {
			upd = upd.SetFlag(entlabline.FlagNormal)
		}
		if l.Notes != "" {
			upd = upd.SetNotes(l.Notes)
		}
		if _, err := upd.Save(r.Context()); err != nil {
			h.log.Warn("failed to save lab result line", zap.Error(err))
		}
	}

	// Completed only once every line on the order carries a result.
	remaining, _ := h.db.LabOrderLine.Query().
		Where(entlabline.LabOrderID(loID), entlabline.ResultEQ("")).
		Count(r.Context())
	var updatedOrder *ent.LabOrder
	if remaining == 0 {
		updatedOrder, err = h.db.LabOrder.UpdateOneID(loID).
			SetStatus(entlaborder.StatusCompleted).
			SetCompletedAt(now).
			Save(r.Context())
	} else {
		updatedOrder, err = h.db.LabOrder.UpdateOneID(loID).SetStatus(entlaborder.StatusInProgress).Save(r.Context())
	}
	if err != nil {
		h.log.Warn("failed to update lab order status", zap.Error(err))
	}

	var updatedVisit *ent.PatientVisit
	if remaining == 0 {
		updatedVisit, err = h.db.PatientVisit.UpdateOneID(visit.ID).SetStatus(entvisit.StatusLabComplete).Save(r.Context())
		if err != nil {
			h.log.Warn("failed to advance visit to lab_complete", zap.Error(err))
		}
		if order.ExaminationID != nil {
			if _, err := h.db.ExaminationRecord.UpdateOneID(*order.ExaminationID).SetStatus(entexam.StatusInProgress).Save(r.Context()); err != nil {
				h.log.Warn("failed to reopen examination for lab review", zap.Error(err))
			}
		}
	}

	lines, _ := h.db.LabOrderLine.Query().Where(entlabline.LabOrderID(loID)).All(r.Context())
	jsonOK(w, map[string]any{"lab_order": updatedOrder, "lines": lines, "visit": updatedVisit})
}

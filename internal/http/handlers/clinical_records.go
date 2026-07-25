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
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entpatient "github.com/bengobox/pos-service/internal/ent/patient"
	entvisit "github.com/bengobox/pos-service/internal/ent/patientvisit"
	"github.com/bengobox/pos-service/internal/modules/documents"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// ── Patients ────────────────────────────────────────────────────────────────────

type createPatientInput struct {
	OutletID     string   `json:"outlet_id"`
	FullName     string   `json:"full_name"`
	DOB          string   `json:"dob,omitempty"` // YYYY-MM-DD
	Gender       string   `json:"gender,omitempty"`
	Phone        string   `json:"phone,omitempty"`
	IDNumber     string   `json:"id_number,omitempty"`
	Address      string   `json:"address,omitempty"`
	AllergyFlags []string `json:"allergy_flags,omitempty"`
	CRMContactID string   `json:"crm_contact_id,omitempty"`
}

// CreatePatient handles POST /{tenantID}/pos/clinical/patients
func (h *ClinicalHandler) CreatePatient(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input createPatientInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}
	if !h.requireModule(w, r, outletID, moduleRecords) {
		return
	}
	if input.FullName == "" {
		jsonError(w, "full_name is required", http.StatusBadRequest)
		return
	}

	patientNumber := ""
	if n, err := h.seq.GenerateNumber(r.Context(), tid, documents.DocTypePatient); err == nil {
		patientNumber = n
	} else {
		jsonError(w, "failed to generate patient number", http.StatusInternalServerError)
		return
	}

	creator := h.db.Patient.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetPatientNumber(patientNumber).
		SetFullName(input.FullName).
		SetGender(input.Gender).
		SetPhone(input.Phone).
		SetIDNumber(input.IDNumber).
		SetAddress(input.Address)
	if len(input.AllergyFlags) > 0 {
		creator = creator.SetAllergyFlags(input.AllergyFlags)
	}
	if input.DOB != "" {
		if d, err := time.Parse("2006-01-02", input.DOB); err == nil {
			creator = creator.SetDob(d)
		}
	}
	if input.CRMContactID != "" {
		if cid, err := uuid.Parse(input.CRMContactID); err == nil {
			creator = creator.SetCrmContactID(cid)
		}
	}

	p, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create patient failed", zap.Error(err))
		jsonError(w, "failed to register patient", http.StatusInternalServerError)
		return
	}
	jsonOK(w, p)
}

// ListPatients handles GET /{tenantID}/pos/clinical/patients?q=
func (h *ClinicalHandler) ListPatients(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.Patient.Query().Where(entpatient.TenantID(tid)).Order(ent.Desc(entpatient.FieldCreatedAt))
	if search := r.URL.Query().Get("q"); search != "" {
		q = q.Where(entpatient.Or(
			entpatient.FullNameContainsFold(search),
			entpatient.PhoneContainsFold(search),
			entpatient.IDNumberContainsFold(search),
			entpatient.PatientNumberContainsFold(search),
		))
	}
	rows, err := q.Limit(50).All(r.Context())
	if err != nil {
		jsonError(w, "failed to list patients", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": rows})
}

// GetPatient handles GET /{tenantID}/pos/clinical/patients/{patientID} — includes visit history.
func (h *ClinicalHandler) GetPatient(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	pid, err := uuid.Parse(chi.URLParam(r, "patientID"))
	if err != nil {
		jsonError(w, "invalid patient_id", http.StatusBadRequest)
		return
	}
	p, err := h.db.Patient.Query().Where(entpatient.ID(pid), entpatient.TenantID(tid)).Only(r.Context())
	if err != nil {
		jsonError(w, "patient not found", http.StatusNotFound)
		return
	}
	visits, _ := h.db.PatientVisit.Query().
		Where(entvisit.PatientID(pid), entvisit.TenantID(tid)).
		Order(ent.Desc(entvisit.FieldCreatedAt)).
		All(r.Context())
	jsonOK(w, map[string]any{"patient": p, "visits": visits})
}

// ── Visits ──────────────────────────────────────────────────────────────────────

type createVisitInput struct {
	PatientID      string `json:"patient_id"`
	OutletID       string `json:"outlet_id"`
	ChiefComplaint string `json:"chief_complaint,omitempty"`
}

// CreateVisit handles POST /{tenantID}/pos/clinical/visits — opens a new OPD episode for a
// registered patient. When the outlet requires a registration fee, this also creates (but does
// not force payment of) the fee order via the SAME order-creation path every POS sale uses — the
// registering desk still collects payment through the normal checkout/payment modal.
func (h *ClinicalHandler) CreateVisit(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	var input createVisitInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	patientID, err := uuid.Parse(input.PatientID)
	if err != nil {
		jsonError(w, "invalid patient_id", http.StatusBadRequest)
		return
	}
	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}
	if !h.requireModule(w, r, outletID, moduleRecords) {
		return
	}

	visitNumber, err := h.seq.GenerateNumber(r.Context(), tid, documents.DocTypeVisit)
	if err != nil {
		jsonError(w, "failed to generate visit number", http.StatusInternalServerError)
		return
	}

	var userID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims.Subject != "" {
		userID, _ = uuid.Parse(claims.Subject)
	}

	creator := h.db.PatientVisit.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetPatientID(patientID).
		SetVisitNumber(visitNumber).
		SetChiefComplaint(input.ChiefComplaint)
	if userID != uuid.Nil {
		creator = creator.SetRegisteredBy(userID)
	}

	visit, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create visit failed", zap.Error(err))
		jsonError(w, "failed to open visit", http.StatusInternalServerError)
		return
	}

	// Registration/consultation fee — best-effort, never blocks visit creation. The order is
	// created unpaid; the Records desk collects payment via the normal checkout flow using the
	// returned order_id, and triage is gated on it being paid only when require_registration_fee
	// is on (see requireFeePaid in clinical_triage.go).
	feeOrder := h.createRegistrationFeeOrder(r, tid, outletID, userID)
	if feeOrder != nil {
		if _, err := h.db.PatientVisit.UpdateOneID(visit.ID).SetRegistrationFeeOrderID(feeOrder.ID).Save(r.Context()); err == nil {
			visit.RegistrationFeeOrderID = &feeOrder.ID
		}
	}

	resp := map[string]any{"visit": visit}
	if feeOrder != nil {
		resp["registration_fee_order"] = map[string]any{
			"id": feeOrder.ID, "order_number": feeOrder.OrderNumber, "total_amount": feeOrder.TotalAmount,
		}
	}
	jsonOK(w, resp)
}

// createRegistrationFeeOrder builds a single-line order for the outlet's configured
// registration_fee_catalog_item_id, if any. Returns nil (best-effort, logged) on any failure or
// when the outlet has no fee item configured — a visit must never fail to open over billing setup.
func (h *ClinicalHandler) createRegistrationFeeOrder(r *http.Request, tid, outletID, userID uuid.UUID) *ent.POSOrder {
	setting, err := h.db.OutletSetting.Query().Where(entoutletsetting.OutletID(outletID)).Only(r.Context())
	if err != nil || setting.RegistrationFeeCatalogItemID == nil {
		return nil
	}
	if h.orderSvc == nil {
		return nil
	}

	tenantSlug := resolveTenantSlug(r, h.db)
	items, _ := fetchInventoryItems(r.Context(), tenantSlug, "", nil)
	var sku, name string
	var price float64
	for _, it := range items {
		if it.ID == setting.RegistrationFeeCatalogItemID.String() {
			sku, name, price = it.SKU, it.Name, 0
			break
		}
	}
	if sku == "" {
		h.log.Warn("registration fee catalog item not found in inventory", zap.String("item_id", setting.RegistrationFeeCatalogItemID.String()))
		return nil
	}
	if p, ok, err := h.inventory.GetItemPrice(r.Context(), tid.String(), setting.RegistrationFeeCatalogItemID.String(), 1); err == nil && ok && p != nil {
		price = p.UnitPrice
	}

	order, err := h.orderSvc.CreateOrder(r.Context(), orders.CreateOrderRequest{
		TenantID:     tid,
		TenantSlug:   tenantSlug,
		OutletID:     outletID,
		UserID:       userID,
		OrderSubtype: "retail",
		Source:       "pos_terminal",
		Lines: []orders.OrderLineInput{{
			CatalogItemID: *setting.RegistrationFeeCatalogItemID,
			SKU:           sku,
			Name:          name,
			Quantity:      1,
			UnitPrice:     price,
			TotalPrice:    price,
		}},
	})
	if err != nil {
		h.log.Warn("registration fee order creation failed", zap.Error(err))
		return nil
	}
	return order
}

package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entdic "github.com/bengobox/pos-service/internal/ent/druginteractioncheck"
	entoverride "github.com/bengobox/pos-service/internal/ent/poscatalogoverride"
	entpx "github.com/bengobox/pos-service/internal/ent/prescription"
	entpxl "github.com/bengobox/pos-service/internal/ent/prescriptionline"
	"github.com/bengobox/pos-service/internal/modules/inventory"
)

type PharmacyHandler struct {
	log       *zap.Logger
	db        *ent.Client
	inventory *inventory.Client
}

func NewPharmacyHandler(log *zap.Logger, db *ent.Client, inventoryClient *inventory.Client) *PharmacyHandler {
	return &PharmacyHandler{log: log, db: db, inventory: inventoryClient}
}

type prescriptionLineInput struct {
	DrugName           string   `json:"drug_name"`
	Dosage             string   `json:"dosage"`
	Form               string   `json:"form"`
	Instructions       string   `json:"instructions"`
	QuantityPrescribed int      `json:"quantity_prescribed"`
	CatalogItemID      *string  `json:"catalog_item_id"`
	LotNumber          string   `json:"lot_number"`
	UnitPrice          *float64 `json:"unit_price"`
}

type createPrescriptionInput struct {
	OutletID           string                  `json:"outlet_id"`
	OrderID            *string                 `json:"order_id"`
	PrescriptionNumber string                  `json:"prescription_number"`
	PrescriberName     string                  `json:"prescriber_name"`
	PrescriberLicense  string                  `json:"prescriber_license"`
	PatientName        string                  `json:"patient_name"`
	PatientDOB         string                  `json:"patient_dob"`
	PatientIDNumber    string                  `json:"patient_id_number"`
	Notes              string                  `json:"notes"`
	AllergyFlags       []string                `json:"allergy_flags"`
	Lines              []prescriptionLineInput `json:"lines"`
}

type interactionCheckInput struct {
	DrugSKUs       []string `json:"drug_skus"`
	PrescriptionID *string  `json:"prescription_id"`
	OrderID        *string  `json:"order_id"`
}

// CreatePrescription handles POST /{tenantID}/pos/pharmacy/prescriptions
func (h *PharmacyHandler) CreatePrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input createPrescriptionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.PrescriptionNumber == "" || input.PatientName == "" {
		jsonError(w, "prescription_number and patient_name are required", http.StatusBadRequest)
		return
	}

	outletID, err := uuid.Parse(input.OutletID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	md := map[string]any{}
	if len(input.AllergyFlags) > 0 {
		md["allergy_flags"] = input.AllergyFlags
	}

	creator := h.db.Prescription.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetPrescriptionNumber(input.PrescriptionNumber).
		SetPrescriberName(input.PrescriberName).
		SetPrescriberLicense(input.PrescriberLicense).
		SetPatientName(input.PatientName).
		SetPatientDob(input.PatientDOB).
		SetPatientIDNumber(input.PatientIDNumber).
		SetNotes(input.Notes).
		SetMetadata(md)

	if input.OrderID != nil {
		oid, err := uuid.Parse(*input.OrderID)
		if err == nil {
			creator = creator.SetOrderID(oid)
		}
	}

	px, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create prescription failed", zap.Error(err))
		jsonError(w, "failed to create prescription", http.StatusInternalServerError)
		return
	}

	catalogItemIDs := make([]string, 0, len(input.Lines))
	for _, li := range input.Lines {
		if li.DrugName == "" || li.QuantityPrescribed <= 0 {
			continue
		}
		lc := h.db.PrescriptionLine.Create().
			SetPrescriptionID(px.ID).
			SetDrugName(li.DrugName).
			SetDosage(li.Dosage).
			SetForm(li.Form).
			SetInstructions(li.Instructions).
			SetQuantityPrescribed(li.QuantityPrescribed).
			SetLotNumber(li.LotNumber)
		if li.CatalogItemID != nil {
			cid, err := uuid.Parse(*li.CatalogItemID)
			if err == nil {
				lc = lc.SetCatalogItemID(cid)
				catalogItemIDs = append(catalogItemIDs, cid.String())
			}
		}
		if li.UnitPrice != nil {
			lc = lc.SetUnitPrice(decimal.NewFromFloat(*li.UnitPrice))
		}
		if _, err := lc.Save(r.Context()); err != nil {
			h.log.Warn("failed to create prescription line", zap.Error(err))
		}
	}

	// Automatic pre-approval safety check: run the DDI/allergy engine over every
	// catalog-linked line the moment the prescription is captured, so the pharmacist sees
	// flagged status immediately rather than needing to trigger it separately.
	if len(catalogItemIDs) > 1 || (len(catalogItemIDs) >= 1 && len(input.AllergyFlags) > 0) {
		result, details, severity := h.runInteractionCheckByItemIDs(r, tid, catalogItemIDs, input.AllergyFlags)
		checkCreator := h.db.DrugInteractionCheck.Create().
			SetTenantID(tid).
			SetPrescriptionID(px.ID).
			SetDrugSkus(catalogItemIDs).
			SetResult(result).
			SetDetails(details).
			SetCheckedAt(time.Now())
		if check, err := checkCreator.Save(r.Context()); err == nil {
			h.recordInteractionCheckOnPrescription(r, px.ID, check.ID, severity)
			px, _ = h.db.Prescription.Get(r.Context(), px.ID) // reflect status="flagged" if set
		} else {
			h.log.Warn("auto interaction check failed to save", zap.Error(err))
		}
	}

	lines, _ := h.db.PrescriptionLine.Query().
		Where(entpxl.PrescriptionID(px.ID)).
		All(r.Context())

	jsonOK(w, map[string]any{"prescription": px, "lines": lines})
}

// ListPrescriptions handles GET /{tenantID}/pos/pharmacy/prescriptions
func (h *PharmacyHandler) ListPrescriptions(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.Prescription.Query().Where(entpx.TenantID(tid))

	if s := r.URL.Query().Get("status"); s != "" {
		q = q.Where(entpx.StatusEQ(s))
	}
	if name := r.URL.Query().Get("patient_name"); name != "" {
		q = q.Where(entpx.PatientNameContainsFold(name))
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	prescriptions, err := q.Order(ent.Desc(entpx.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list prescriptions failed", zap.Error(err))
		jsonError(w, "failed to list prescriptions", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(prescriptions, total, p))
}

// GetPrescription handles GET /{tenantID}/pos/pharmacy/prescriptions/{id}
func (h *PharmacyHandler) GetPrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}

	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}

	lines, _ := h.db.PrescriptionLine.Query().
		Where(entpxl.PrescriptionID(pxID)).
		All(r.Context())

	jsonOK(w, map[string]any{"prescription": px, "lines": lines})
}

// Dispense handles POST /{tenantID}/pos/pharmacy/prescriptions/{id}/dispense
func (h *PharmacyHandler) Dispense(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}

	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}
	if px.Status != "approved" && px.Status != "locked" {
		jsonError(w, "prescription must be approved (and locked, if required) before dispensing", http.StatusConflict)
		return
	}

	var body struct {
		DispensedBy *string `json:"dispensed_by"`
		Notes       string  `json:"notes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	now := time.Now()
	upd := h.db.Prescription.UpdateOneID(pxID).
		SetStatus("dispensed").
		SetDispensedAt(now)
	if body.DispensedBy != nil {
		if dbID, err := uuid.Parse(*body.DispensedBy); err == nil {
			upd = upd.SetDispensedBy(dbID)
		}
	}
	if body.Notes != "" {
		upd = upd.SetNotes(body.Notes)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("dispense prescription failed", zap.Error(err))
		jsonError(w, "failed to dispense prescription", http.StatusInternalServerError)
		return
	}

	h.db.PrescriptionLine.Update().
		Where(entpxl.PrescriptionID(pxID), entpxl.StatusEQ("pending")).
		SetStatus("dispensed").
		SaveX(r.Context())

	// Dispense is the moment stock physically leaves the shelf — convert the hold created at
	// approval (Phase 3) into an actual depletion via inventory-api's existing
	// ConsumeReservation, rather than a fresh ad-hoc decrement. Best-effort: a failure here is
	// logged, not fatal — the prescription is already marked dispensed since the physical
	// handover already happened.
	if resID, ok := updated.Metadata["reservation_id"].(string); ok && resID != "" && h.inventory != nil {
		if err := h.inventory.ConsumeReservation(r.Context(), tid.String(), resID); err != nil {
			h.log.Warn("consume reservation on dispense failed", zap.Error(err))
		}
	}

	jsonOK(w, updated)
}

// ListPatients handles GET /{tenantID}/pos/pharmacy/patients
// Derives distinct patient records from the prescriptions table.
func (h *PharmacyHandler) ListPatients(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.Prescription.Query().Where(entpx.TenantID(tid))
	if search := r.URL.Query().Get("q"); search != "" {
		q = q.Where(entpx.PatientNameContainsFold(search))
	}

	prescriptions, err := q.Order(ent.Desc(entpx.FieldCreatedAt)).All(r.Context())
	if err != nil {
		h.log.Error("list patients failed", zap.Error(err))
		jsonError(w, "failed to list patients", http.StatusInternalServerError)
		return
	}

	// Deduplicate by patient_id_number (fallback: patient_name)
	seen := make(map[string]struct{})
	type patientRow struct {
		PatientName     string `json:"patient_name"`
		PatientDOB      string `json:"patient_dob"`
		PatientIDNumber string `json:"patient_id_number"`
		VisitCount      int    `json:"visit_count"`
		LastVisit       string `json:"last_visit"`
	}
	countMap := make(map[string]*patientRow)
	for _, px := range prescriptions {
		key := px.PatientIDNumber
		if key == "" {
			key = px.PatientName
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			countMap[key] = &patientRow{
				PatientName:     px.PatientName,
				PatientDOB:      px.PatientDob,
				PatientIDNumber: px.PatientIDNumber,
				LastVisit:       px.CreatedAt.Format(time.RFC3339),
			}
		}
		countMap[key].VisitCount++
	}

	allPatients := make([]*patientRow, 0, len(countMap))
	for _, row := range countMap {
		allPatients = append(allPatients, row)
	}

	pg := pagination.Parse(r)
	total := len(allPatients)
	start := pg.Offset
	if start > total {
		start = total
	}
	end := start + pg.Limit
	if end > total {
		end = total
	}
	jsonOK(w, pagination.NewResponse(allPatients[start:end], total, pg))
}

// CreateInteractionCheck handles POST /{tenantID}/pos/pharmacy/interaction-checks
// Runs the real inventory-api-backed DDI/allergy engine (S2S) and records the result.
// When PrescriptionID is set and a moderate+ finding comes back, the prescription is
// moved to status="flagged", blocking Approve until a pharmacist override is recorded.
func (h *PharmacyHandler) CreateInteractionCheck(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input interactionCheckInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(input.DrugSKUs) == 0 {
		jsonError(w, "drug_skus is required", http.StatusBadRequest)
		return
	}

	result, resultDetails, severity := h.runInteractionCheck(r, tid, input.DrugSKUs, nil)

	creator := h.db.DrugInteractionCheck.Create().
		SetTenantID(tid).
		SetDrugSkus(input.DrugSKUs).
		SetResult(result).
		SetDetails(resultDetails).
		SetCheckedAt(time.Now())

	var pxID *uuid.UUID
	if input.PrescriptionID != nil {
		if pid, err := uuid.Parse(*input.PrescriptionID); err == nil {
			creator = creator.SetPrescriptionID(pid)
			pxID = &pid
		}
	}
	if input.OrderID != nil {
		if oid, err := uuid.Parse(*input.OrderID); err == nil {
			creator = creator.SetOrderID(oid)
		}
	}

	check, err := creator.Save(r.Context())
	if err != nil {
		h.log.Error("create interaction check failed", zap.Error(err))
		jsonError(w, "failed to create interaction check", http.StatusInternalServerError)
		return
	}

	if pxID != nil {
		h.recordInteractionCheckOnPrescription(r, *pxID, check.ID, severity)
	}

	_ = entdic.TenantID(tid)
	jsonOK(w, check)
}

// runInteractionCheck calls inventory-api's check-interactions S2S endpoint for the given
// item IDs (falls back to SKUs when itemIDs is empty) plus the patient's allergy flags, and
// reduces the response to a DrugInteractionCheck-compatible (result, details, severity) triple.
// Best-effort: an unreachable inventory-api returns result="clear" with an explanatory detail
// rather than blocking dispensing on an infrastructure hiccup.
func (h *PharmacyHandler) runInteractionCheck(r *http.Request, tenantID uuid.UUID, skus []string, allergyFlags []string) (result, details, severity string) {
	if h.inventory == nil {
		return "clear", "interaction engine not configured", ""
	}
	resp, err := h.inventory.CheckInteractions(r.Context(), tenantID.String(), inventory.CheckInteractionsRequest{
		SKUs:         skus,
		AllergyFlags: allergyFlags,
	})
	if err != nil {
		h.log.Warn("interaction check S2S call failed", zap.Error(err))
		return "clear", "interaction check unavailable: " + err.Error(), ""
	}
	return reduceInteractionFindings(resp)
}

// runInteractionCheckByItemIDs is the PrescriptionLine-facing variant (lines carry
// catalog_item_id, not SKU).
func (h *PharmacyHandler) runInteractionCheckByItemIDs(r *http.Request, tenantID uuid.UUID, itemIDs []string, allergyFlags []string) (result, details, severity string) {
	if h.inventory == nil {
		return "clear", "interaction engine not configured", ""
	}
	resp, err := h.inventory.CheckInteractions(r.Context(), tenantID.String(), inventory.CheckInteractionsRequest{
		ItemIDs:      itemIDs,
		AllergyFlags: allergyFlags,
	})
	if err != nil {
		h.log.Warn("interaction check S2S call failed", zap.Error(err))
		return "clear", "interaction check unavailable: " + err.Error(), ""
	}
	return reduceInteractionFindings(resp)
}

var severityRank = map[string]int{"minor": 1, "moderate": 2, "major": 3, "contraindicated": 4}

func reduceInteractionFindings(resp *inventory.CheckInteractionsResponse) (result, details, severity string) {
	if len(resp.Interactions) == 0 && len(resp.AllergyMatches) == 0 {
		return "clear", "", ""
	}
	lines := make([]string, 0, len(resp.Interactions)+len(resp.AllergyMatches))
	worst := ""
	for _, f := range resp.Interactions {
		lines = append(lines, f.Severity+": "+f.ClassA+" + "+f.ClassB+" ("+f.SKUA+", "+f.SKUB+") — "+f.Description)
		if severityRank[f.Severity] > severityRank[worst] {
			worst = f.Severity
		}
	}
	for _, m := range resp.AllergyMatches {
		lines = append(lines, "allergy: "+m.SKU+" matches declared allergy "+m.AllergyFlag)
		if severityRank["major"] > severityRank[worst] {
			worst = "major" // allergy hits are treated as at least major severity
		}
	}
	sort.Strings(lines)
	detail := ""
	for i, l := range lines {
		if i > 0 {
			detail += "; "
		}
		detail += l
	}
	return "flagged", detail, worst
}

// recordInteractionCheckOnPrescription stamps the check reference into Prescription.metadata
// and, on a moderate+ finding, moves status to "flagged" (best-effort; logged, not fatal).
func (h *PharmacyHandler) recordInteractionCheckOnPrescription(r *http.Request, pxID, checkID uuid.UUID, severity string) {
	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil {
		return
	}
	md := cloneMetadata(px.Metadata)
	md["interaction_check_id"] = checkID.String()
	upd := h.db.Prescription.UpdateOneID(pxID).SetMetadata(md)
	if severityRank[severity] >= severityRank["moderate"] && px.Status == "pending" {
		upd = upd.SetStatus("flagged")
	}
	if _, err := upd.Save(r.Context()); err != nil {
		h.log.Warn("failed to record interaction check on prescription", zap.Error(err))
	}
}

func cloneMetadata(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

type approvePrescriptionInput struct {
	ApprovedBy     *string `json:"approved_by"`
	OverrideReason string  `json:"override_reason"`
}

// ApprovePrescription handles POST /{tenantID}/pos/pharmacy/prescriptions/{id}/approve.
// Blocks on status="flagged" unless an override_reason is supplied (the pharmacist's
// documented clinical judgment call), per the Phase 1 approval workflow.
func (h *PharmacyHandler) ApprovePrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}
	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}
	if px.Status != "pending" && px.Status != "flagged" && px.Status != "pharmacist_review" {
		jsonError(w, "prescription is not in an approvable state", http.StatusConflict)
		return
	}

	var input approvePrescriptionInput
	_ = json.NewDecoder(r.Body).Decode(&input)

	if px.Status == "flagged" && input.OverrideReason == "" {
		jsonError(w, "prescription is flagged for a drug interaction/allergy — an override_reason is required to approve", http.StatusUnprocessableEntity)
		return
	}

	md := cloneMetadata(px.Metadata)
	now := time.Now()
	md["approved_at"] = now.Format(time.RFC3339)
	if input.OverrideReason != "" {
		md["approval_override_reason"] = input.OverrideReason
	}
	upd := h.db.Prescription.UpdateOneID(pxID).
		SetStatus("approved").
		SetMetadata(md)
	if input.ApprovedBy != nil {
		if aid, err := uuid.Parse(*input.ApprovedBy); err == nil {
			md["approved_by"] = aid.String()
			upd = upd.SetMetadata(md)
		}
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("approve prescription failed", zap.Error(err))
		jsonError(w, "failed to approve prescription", http.StatusInternalServerError)
		return
	}

	// Phase 3: hold the stock the moment a pharmacist signs off, so it can't sell out from
	// under this prescription before checkout. Best-effort — a reservation failure doesn't
	// block the approval itself (the sale-finalize backflush still runs the ad-hoc path).
	if resID := h.createReservationForPrescription(r, tid, updated); resID != "" {
		md2 := cloneMetadata(updated.Metadata)
		md2["reservation_id"] = resID
		if updated2, err := h.db.Prescription.UpdateOneID(pxID).SetMetadata(md2).Save(r.Context()); err == nil {
			updated = updated2
		}
	}

	jsonOK(w, updated)
}

// createReservationForPrescription reserves stock for every catalog-linked PrescriptionLine
// via inventory-api's existing Reservation state machine (Available→Reserved) — reused as-is,
// not a new hold mechanism. Returns the reservation ID, or "" on any failure (logged, non-fatal).
func (h *PharmacyHandler) createReservationForPrescription(r *http.Request, tenantID uuid.UUID, px *ent.Prescription) string {
	if h.inventory == nil {
		return ""
	}
	lines, err := h.db.PrescriptionLine.Query().Where(entpxl.PrescriptionID(px.ID)).All(r.Context())
	if err != nil || len(lines) == 0 {
		return ""
	}
	itemIDs := make([]string, 0, len(lines))
	qtyByItemID := make(map[string]float64, len(lines))
	for _, l := range lines {
		if l.CatalogItemID == nil {
			continue
		}
		id := l.CatalogItemID.String()
		itemIDs = append(itemIDs, id)
		qtyByItemID[id] = float64(l.QuantityPrescribed - l.QuantityDispensed)
	}
	if len(itemIDs) == 0 {
		return ""
	}

	// Reuse the drug-classification resolver purely for its item_id→sku mapping (it already
	// resolves every requested item; interactions/allergies are irrelevant here).
	resolved, err := h.inventory.CheckInteractions(r.Context(), tenantID.String(), inventory.CheckInteractionsRequest{
		ItemIDs: itemIDs,
	})
	if err != nil {
		h.log.Warn("prescription reservation: sku resolution failed", zap.Error(err))
		return ""
	}
	items := make([]inventory.ReservationItem, 0, len(resolved.Resolved))
	for _, it := range resolved.Resolved {
		qty := qtyByItemID[it.ItemID]
		if qty <= 0 || it.SKU == "" {
			continue
		}
		items = append(items, inventory.ReservationItem{SKU: it.SKU, Quantity: qty})
	}
	if len(items) == 0 {
		return ""
	}

	expiresAt := time.Now().Add(48 * time.Hour) // default hold window; see Open Items in the build plan
	res, err := h.inventory.CreateReservation(r.Context(), tenantID.String(), inventory.CreateReservationRequest{
		OrderID:        px.ID.String(),
		Items:          items,
		ExpiresAt:      &expiresAt,
		IdempotencyKey: "prescription:" + px.ID.String(),
	})
	if err != nil {
		h.log.Warn("prescription reservation: create failed", zap.Error(err))
		return ""
	}
	return res.ID
}

// RejectPrescription handles POST /{tenantID}/pos/pharmacy/prescriptions/{id}/reject.
// Releases any stock reservation held for this prescription back to available.
func (h *PharmacyHandler) RejectPrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}
	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}
	if px.Status == "dispensed" || px.Status == "partially_dispensed" {
		jsonError(w, "a dispensed prescription cannot be rejected", http.StatusConflict)
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if resID, ok := px.Metadata["reservation_id"].(string); ok && resID != "" && h.inventory != nil {
		if err := h.inventory.ReleaseReservation(r.Context(), tid.String(), resID, body.Reason); err != nil {
			h.log.Warn("release reservation on reject failed", zap.Error(err))
		}
	}

	updated, err := h.db.Prescription.UpdateOneID(pxID).SetStatus("rejected").SetNotes(body.Reason).Save(r.Context())
	if err != nil {
		h.log.Error("reject prescription failed", zap.Error(err))
		jsonError(w, "failed to reject prescription", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

// LockPrescription handles POST /{tenantID}/pos/pharmacy/prescriptions/{id}/lock.
// Optional final pharmacist sign-off step (tenant-configurable, off by default — see
// OutletSetting.metadata["pharmacy"]["require_dispense_lock"]) between approval and dispense.
func (h *PharmacyHandler) LockPrescription(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	pxID, err := uuid.Parse(chi.URLParam(r, "prescriptionID"))
	if err != nil {
		jsonError(w, "invalid prescription_id", http.StatusBadRequest)
		return
	}
	px, err := h.db.Prescription.Get(r.Context(), pxID)
	if err != nil || px.TenantID != tid {
		jsonError(w, "prescription not found", http.StatusNotFound)
		return
	}
	if px.Status != "approved" {
		jsonError(w, "only an approved prescription can be locked", http.StatusConflict)
		return
	}
	updated, err := h.db.Prescription.UpdateOneID(pxID).SetStatus("locked").Save(r.Context())
	if err != nil {
		h.log.Error("lock prescription failed", zap.Error(err))
		jsonError(w, "failed to lock prescription", http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

type ageVerifyInput struct {
	SKU         string `json:"sku"`          // item SKU to check
	CustomerDOB string `json:"customer_dob"` // YYYY-MM-DD
}

// AgeVerify handles POST /{tenantID}/pos/pharmacy/age-verify
// Returns 200 if the customer meets the item's minimum age requirement, 422 if blocked.
func (h *PharmacyHandler) AgeVerify(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input ageVerifyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.SKU == "" || input.CustomerDOB == "" {
		jsonError(w, "sku and customer_dob are required", http.StatusBadRequest)
		return
	}

	dob, err := time.Parse("2006-01-02", input.CustomerDOB)
	if err != nil {
		jsonError(w, "customer_dob must be in YYYY-MM-DD format", http.StatusBadRequest)
		return
	}

	override, err := h.db.POSCatalogOverride.Query().
		Where(
			entoverride.TenantID(tid),
			entoverride.InventorySku(input.SKU),
			entoverride.RequiresAgeVerificationEQ(true),
			entoverride.MinimumAgeNotNil(),
		).First(r.Context())
	if err != nil {
		// No age-restricted override found — allow
		jsonOK(w, map[string]any{"allowed": true, "message": "no age restriction for this item"})
		return
	}

	minimumAge := *override.MinimumAge
	ageYears := int(time.Since(dob).Hours() / 8766) // approx years
	if ageYears < minimumAge {
		jsonError(w, "customer does not meet the minimum age requirement", http.StatusUnprocessableEntity)
		return
	}

	jsonOK(w, map[string]any{
		"allowed":      true,
		"minimum_age":  minimumAge,
		"customer_age": ageYears,
	})
}

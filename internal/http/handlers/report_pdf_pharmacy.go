package handlers

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcsl "github.com/bengobox/pos-service/internal/ent/controlledsubstancelog"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/modules/docs"
)

// ControlledSubstanceExport handles GET /{tenantID}/pos/reports/controlled-substances?from=&to=
//
// A regulator-ready export of the dual-person controlled-substance dispensing register
// (ControlledSubstanceLog), including the lot/expiry batch traceability Phase 2/5 wired onto
// each log entry — so a recalled/expired batch can be traced to exactly who it was dispensed to,
// by whom, and under which witness. Reuses the same docs.Report primitive (Cards/Sections) every
// other POS report document uses, via ReportPDFHandler's existing newReport/write helpers.
func (h *ReportPDFHandler) ControlledSubstanceExport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	oid := h.outletScope(r)
	from, to := parseReportRange(r, requestTenantLocation(r, h.db))

	q := h.db.ControlledSubstanceLog.Query().
		Where(
			entcsl.TenantID(tid),
			entcsl.DispensedAtGTE(from),
			entcsl.DispensedAtLTE(to),
		).
		Order(ent.Asc(entcsl.FieldDispensedAt))
	if oid != nil {
		q = q.Where(entcsl.OutletID(*oid))
	}
	logs, err := q.All(ctx)
	if err != nil {
		h.log.Error("controlled-substance export: query failed", zap.Error(err))
		jsonError(w, "failed to generate controlled substance export", http.StatusInternalServerError)
		return
	}

	// Batch-resolve dispensing/witness staff names (not N+1).
	staffIDSet := make(map[uuid.UUID]bool)
	for _, l := range logs {
		staffIDSet[l.DispensedBy] = true
		if l.WitnessStaffID != nil {
			staffIDSet[*l.WitnessStaffID] = true
		}
	}
	staffNames := map[string]string{}
	if len(staffIDSet) > 0 {
		ids := make([]uuid.UUID, 0, len(staffIDSet))
		for id := range staffIDSet {
			ids = append(ids, id)
		}
		staff, _ := h.db.StaffMember.Query().Where(entstaff.IDIn(ids...)).All(ctx)
		for _, s := range staff {
			staffNames[s.ID.String()] = s.Name
		}
	}

	rows := make([][]docs.Cell, 0, len(logs))
	var totalQty float64
	for _, l := range logs {
		witness := "—"
		if l.WitnessStaffID != nil {
			if n, ok := staffNames[l.WitnessStaffID.String()]; ok && n != "" {
				witness = n
			}
		}
		dispensedBy := staffNames[l.DispensedBy.String()]
		lot := l.LotNumber
		if lot == "" {
			lot = "—"
		}
		expiry := "—"
		if l.LotExpiryDate != nil {
			expiry = l.LotExpiryDate.Format("2006-01-02")
		}
		rows = append(rows, []docs.Cell{
			docs.Text(l.DispensedAt.Format("2006-01-02 15:04")),
			docs.Text(l.ItemSku),
			docs.Text(l.ItemName),
			docs.Text(fmtAmount(l.QuantityDispensed)),
			docs.Text(lot),
			docs.Text(expiry),
			docs.Text(l.PatientName),
			docs.Text(dispensedBy),
			docs.Text(witness),
		})
		totalQty += l.QuantityDispensed
	}

	report := h.newReport(ctx, tid, oid, "Controlled Substance Register", "Regulator export", from, to, true)
	report.Cards = []docs.Card{
		{Label: "Dispensing Events", Value: strconv.Itoa(len(logs))},
		{Label: "Total Quantity", Value: fmtAmount(totalQty)},
	}
	report.Sections = []docs.Section{{
		Kind:  docs.SectionTable,
		Title: "Dispensing Register",
		Columns: []docs.Column{
			{Header: "Dispensed At", Weight: 1.4},
			{Header: "SKU", Weight: 1},
			{Header: "Drug", Weight: 1.4},
			{Header: "Qty", Weight: 0.8, Align: "R"},
			{Header: "Lot #", Weight: 1},
			{Header: "Expiry", Weight: 1},
			{Header: "Patient", Weight: 1.4},
			{Header: "Dispensed By", Weight: 1.2},
			{Header: "Witness", Weight: 1.2},
		},
		Rows: rows,
	}}
	h.write(w, r, report, "controlled-substances")
}

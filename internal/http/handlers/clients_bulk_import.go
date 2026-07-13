package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
)

type bulkClientRow struct {
	ExternalRef  string         `json:"external_ref"`
	OutletID     string         `json:"outlet_id,omitempty"`
	Phone        string         `json:"phone"`
	CRMContactID string         `json:"crm_contact_id,omitempty"`
	Notes        string         `json:"notes,omitempty"`
	Preferences  map[string]any `json:"preferences,omitempty"`
}

type bulkClientImportRequest struct {
	Clients []bulkClientRow `json:"clients"`
}

type bulkClientRowResult struct {
	ExternalRef string `json:"external_ref"`
	ClientID    string `json:"client_id,omitempty"`
	Status      string `json:"status"` // created | updated | failed
	Error       string `json:"error,omitempty"`
}

type bulkClientImportResult struct {
	Created int                   `json:"created"`
	Updated int                   `json:"updated"`
	Failed  int                   `json:"failed"`
	Results []bulkClientRowResult `json:"results"`
}

// BulkImport handles POST /{tenantID}/pos/clients/bulk-import.
// Upserts a batch of ClientRecords by phone (same rule as CreateOrUpsert), so a migration can
// re-run the same payload safely. phone is required and must be unique per tenant — this is
// POS-specific data only (notes/preferences/outlet + the crm_contact_id link); contact master
// data (name/email) must already exist in MarketFlow CRM, created via its own bulk-import first.
//
//	@Summary  Bulk import/upsert POS client records
//	@Tags     clients
//	@Accept   json
//	@Produce  json
//	@Success  200  {object}  bulkClientImportResult
//	@Router   /{tenantID}/pos/clients/bulk-import [post]
func (h *ClientHandler) BulkImport(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var req bulkClientImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Clients) == 0 {
		jsonError(w, "clients array is required and must be non-empty", http.StatusBadRequest)
		return
	}

	result := bulkClientImportResult{Results: make([]bulkClientRowResult, 0, len(req.Clients))}
	for _, row := range req.Clients {
		id, status, err := h.upsertClientRow(r.Context(), tid, row)
		rr := bulkClientRowResult{ExternalRef: row.ExternalRef, Status: status}
		if err != nil {
			result.Failed++
			rr.Status = "failed"
			rr.Error = err.Error()
			h.log.Warn("bulk client import: row failed",
				zap.String("external_ref", row.ExternalRef), zap.Error(err))
		} else {
			rr.ClientID = id.String()
			switch status {
			case "created":
				result.Created++
			case "updated":
				result.Updated++
			}
		}
		result.Results = append(result.Results, rr)
	}

	jsonOK(w, result)
}

// upsertClientRow applies the same phone-based upsert rule as CreateOrUpsert.
func (h *ClientHandler) upsertClientRow(ctx context.Context, tid uuid.UUID, row bulkClientRow) (uuid.UUID, string, error) {
	if row.Phone == "" {
		return uuid.Nil, "failed", fmt.Errorf("phone is required")
	}

	existing, _ := h.db.ClientRecord.Query().
		Where(entclient.TenantID(tid), entclient.Phone(row.Phone)).
		Only(ctx)

	if existing != nil {
		upd := existing.Update()
		if row.Notes != "" {
			upd = upd.SetNotes(row.Notes)
		}
		if len(row.Preferences) > 0 {
			upd = upd.SetPreferences(row.Preferences)
		}
		if row.CRMContactID != "" {
			crmID, parseErr := uuid.Parse(row.CRMContactID)
			if parseErr != nil {
				return uuid.Nil, "failed", fmt.Errorf("invalid crm_contact_id: %w", parseErr)
			}
			upd = upd.SetCrmContactID(crmID)
		}
		if row.OutletID != "" {
			oid, parseErr := uuid.Parse(row.OutletID)
			if parseErr != nil {
				return uuid.Nil, "failed", fmt.Errorf("invalid outlet_id: %w", parseErr)
			}
			upd = upd.SetOutletID(oid)
		}
		updated, saveErr := upd.Save(ctx)
		if saveErr != nil {
			return uuid.Nil, "failed", fmt.Errorf("update: %w", saveErr)
		}
		return updated.ID, "updated", nil
	}

	c := h.db.ClientRecord.Create().
		SetTenantID(tid).
		SetPhone(row.Phone)
	if row.OutletID != "" {
		oid, parseErr := uuid.Parse(row.OutletID)
		if parseErr != nil {
			return uuid.Nil, "failed", fmt.Errorf("invalid outlet_id: %w", parseErr)
		}
		c = c.SetOutletID(oid)
	}
	if row.CRMContactID != "" {
		crmID, parseErr := uuid.Parse(row.CRMContactID)
		if parseErr != nil {
			return uuid.Nil, "failed", fmt.Errorf("invalid crm_contact_id: %w", parseErr)
		}
		c = c.SetCrmContactID(crmID)
	}
	if row.Notes != "" {
		c = c.SetNotes(row.Notes)
	}
	if len(row.Preferences) > 0 {
		c = c.SetPreferences(row.Preferences)
	}

	created, err := c.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return uuid.Nil, "failed", fmt.Errorf("phone already registered under a different record: %w", err)
		}
		return uuid.Nil, "failed", fmt.Errorf("create: %w", err)
	}
	return created.ID, "created", nil
}

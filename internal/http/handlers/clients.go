package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
)

// ClientHandler manages POS client records. Contact master data lives in MarketFlow CRM.
// This handler stores only POS-specific data (notes, preferences) keyed by phone + crm_contact_id.
type ClientHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewClientHandler(log *zap.Logger, db *ent.Client) *ClientHandler {
	return &ClientHandler{log: log, db: db}
}

// List handles GET /{tenantID}/pos/clients?phone={phone}&crm_contact_id={id}
func (h *ClientHandler) List(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.ClientRecord.Query().Where(entclient.TenantID(tid))

	if phone := r.URL.Query().Get("phone"); phone != "" {
		q = q.Where(entclient.Phone(phone))
	}
	if crmIDStr := r.URL.Query().Get("crm_contact_id"); crmIDStr != "" {
		if crmID, parseErr := uuid.Parse(crmIDStr); parseErr == nil {
			q = q.Where(entclient.CrmContactID(crmID))
		}
	}

	clients, err := q.Order(ent.Asc(entclient.FieldCreatedAt)).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": clients, "total": len(clients)})
}

// CreateOrUpsert handles POST /{tenantID}/pos/clients
// Upserts by phone — creates if not exists, updates POS-specific fields if phone already registered.
// Contact master data (name, email, dob, gender) must be managed in MarketFlow CRM directly.
func (h *ClientHandler) CreateOrUpsert(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		OutletID     string         `json:"outlet_id,omitempty"`
		Phone        string         `json:"phone"`
		CRMContactID string         `json:"crm_contact_id,omitempty"`
		Notes        string         `json:"notes,omitempty"`
		Preferences  map[string]any `json:"preferences,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Phone == "" {
		jsonError(w, "phone is required", http.StatusBadRequest)
		return
	}

	existing, _ := h.db.ClientRecord.Query().
		Where(entclient.TenantID(tid), entclient.Phone(input.Phone)).
		Only(r.Context())

	if existing != nil {
		upd := existing.Update()
		if input.Notes != "" {
			upd = upd.SetNotes(input.Notes)
		}
		if len(input.Preferences) > 0 {
			upd = upd.SetPreferences(input.Preferences)
		}
		if input.CRMContactID != "" {
			if crmID, parseErr := uuid.Parse(input.CRMContactID); parseErr == nil {
				upd = upd.SetCrmContactID(crmID)
			}
		}
		updated, saveErr := upd.Save(r.Context())
		if saveErr != nil {
			jsonError(w, "update failed: "+saveErr.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, updated)
		return
	}

	c := h.db.ClientRecord.Create().
		SetTenantID(tid).
		SetPhone(input.Phone)

	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			c = c.SetOutletID(oid)
		}
	}
	if input.CRMContactID != "" {
		if crmID, parseErr := uuid.Parse(input.CRMContactID); parseErr == nil {
			c = c.SetCrmContactID(crmID)
		}
	}
	if input.Notes != "" {
		c = c.SetNotes(input.Notes)
	}
	if len(input.Preferences) > 0 {
		c = c.SetPreferences(input.Preferences)
	}

	client, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create client failed", zap.Error(err))
		jsonError(w, "failed to create client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, client)
}

// Get handles GET /{tenantID}/pos/clients/{clientID}
func (h *ClientHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	clientID, err := uuid.Parse(chi.URLParam(r, "clientID"))
	if err != nil {
		jsonError(w, "invalid client_id", http.StatusBadRequest)
		return
	}

	client, err := h.db.ClientRecord.Query().
		Where(entclient.ID(clientID), entclient.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "client not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, client)
}

// Update handles PATCH /{tenantID}/pos/clients/{clientID}
// Only POS-specific fields (notes, preferences, crm_contact_id link) can be updated here.
func (h *ClientHandler) Update(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	clientID, err := uuid.Parse(chi.URLParam(r, "clientID"))
	if err != nil {
		jsonError(w, "invalid client_id", http.StatusBadRequest)
		return
	}

	client, err := h.db.ClientRecord.Query().
		Where(entclient.ID(clientID), entclient.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "client not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	var input struct {
		CRMContactID string         `json:"crm_contact_id,omitempty"`
		Notes        string         `json:"notes,omitempty"`
		Preferences  map[string]any `json:"preferences,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	upd := client.Update()
	if input.CRMContactID != "" {
		if crmID, parseErr := uuid.Parse(input.CRMContactID); parseErr == nil {
			upd = upd.SetCrmContactID(crmID)
		}
	}
	if input.Notes != "" {
		upd = upd.SetNotes(input.Notes)
	}
	if len(input.Preferences) > 0 {
		upd = upd.SetPreferences(input.Preferences)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		jsonError(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, updated)
}

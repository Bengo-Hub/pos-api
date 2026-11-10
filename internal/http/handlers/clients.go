package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entclient "github.com/bengobox/pos-service/internal/ent/clientrecord"
	entorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/customers"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// ClientHandler manages POS client records. Contact master data lives in MarketFlow CRM.
// This handler stores only POS-specific data (notes, preferences) keyed by phone + crm_contact_id.
type ClientHandler struct {
	log       *zap.Logger
	db        *ent.Client
	customers *customers.Resolver
	treasury  *treasury.Client // credit-terms proxy (clients_credit.go)
}

func NewClientHandler(log *zap.Logger, db *ent.Client) *ClientHandler {
	return &ClientHandler{log: log, db: db}
}

// SetCustomerResolver wires the resolver that links new client records to the CRM.
func (h *ClientHandler) SetCustomerResolver(res *customers.Resolver) {
	h.customers = res
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
		// Every stored spelling of the number, since records are now kept in E.164.
		matches, findErr := customers.FindClientsIn(r.Context(), h.db, tid, phone, 50)
		if findErr != nil {
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}
		ids := make([]uuid.UUID, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		q = q.Where(entclient.IDIn(ids...))
	}
	if crmIDStr := r.URL.Query().Get("crm_contact_id"); crmIDStr != "" {
		if crmID, parseErr := uuid.Parse(crmIDStr); parseErr == nil {
			q = q.Where(entclient.CrmContactID(crmID))
		}
	}

	p := pagination.Parse(r)
	total, _ := q.Clone().Count(r.Context())
	clients, err := q.Order(ent.Asc(entclient.FieldCreatedAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(clients, total, p))
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

	// When staff picked a CRM contact, prefer the record already linked to it.
	var preferID *uuid.UUID
	if id, parseErr := uuid.Parse(input.CRMContactID); parseErr == nil {
		preferID = &id
	}
	existing, _ := customers.FindClientForContact(r.Context(), h.db, tid, input.Phone, preferID)

	if existing != nil {
		// The CRM contact was deleted earlier and staff are registering the client again, so
		// link it again (or take the contact id they picked).
		if customers.IsUnlinked(existing.Metadata) && input.CRMContactID == "" {
			relinked, relinkErr := h.customers.RelinkClient(r.Context(), existing, input.Phone)
			if relinkErr != nil {
				if msg, rejected := customers.RejectionMessage(relinkErr); rejected {
					jsonError(w, msg, http.StatusBadRequest)
					return
				}
				h.log.Error("client: relink failed", zap.Error(relinkErr))
				jsonError(w, "failed to update client", http.StatusInternalServerError)
				return
			}
			existing = relinked
		}
		upd := existing.Update()
		if input.CRMContactID != "" && customers.IsUnlinked(existing.Metadata) {
			upd = upd.SetMetadata(customers.WithoutUnlinked(existing.Metadata))
		}
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

	phone := customers.NormalisePhone(input.Phone)
	var crmID *uuid.UUID
	if input.CRMContactID != "" {
		if id, parseErr := uuid.Parse(input.CRMContactID); parseErr == nil {
			crmID = &id
		}
	}
	if crmID == nil {
		// Link to the CRM before saving. An outage saves the record unlinked for the reconciler.
		ident, linkErr := h.customers.Link(r.Context(), tid, input.Phone, "", "")
		if linkErr != nil {
			if msg, rejected := customers.RejectionMessage(linkErr); rejected {
				jsonError(w, msg, http.StatusBadRequest)
				return
			}
			h.log.Error("client: crm link failed", zap.Error(linkErr))
			jsonError(w, "failed to create client", http.StatusInternalServerError)
			return
		}
		crmID = ident.CrmContactID
		if ident.Phone != "" {
			phone = ident.Phone
		}
	}

	c := h.db.ClientRecord.Create().
		SetTenantID(tid).
		SetPhone(phone).
		SetNillableCrmContactID(crmID)

	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			c = c.SetOutletID(oid)
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

// GetOrdersByPhone handles GET /{tenantID}/pos/clients/{phone}/orders?page=&limit=
// Returns paginated purchase history for a customer identified by phone number.
// Permission required: pos.clients.view
func (h *ClientHandler) GetOrdersByPhone(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	phone := chi.URLParam(r, "phone")
	if phone == "" {
		jsonError(w, "phone is required", http.StatusBadRequest)
		return
	}
	p := pagination.Parse(r)
	q := h.db.POSOrder.Query().
		Where(entorder.TenantID(tid), entorder.CustomerPhone(phone)).
		Order(ent.Desc(entorder.FieldCreatedAt))
	total, _ := q.Clone().Count(r.Context())
	orders, err := q.Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("get orders by phone failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, pagination.NewResponse(orders, total, p))
}

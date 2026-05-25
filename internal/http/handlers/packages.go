package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entpkg "github.com/bengobox/pos-service/internal/ent/servicepackage"
	entpurch "github.com/bengobox/pos-service/internal/ent/servicepackagepurchase"
	entredeem "github.com/bengobox/pos-service/internal/ent/servicepackageredemption"
)

// PackageHandler handles service package CRUD, selling, and redemption.
type PackageHandler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewPackageHandler(log *zap.Logger, db *ent.Client) *PackageHandler {
	return &PackageHandler{log: log, db: db}
}

// ListPackages handles GET /{tenantID}/pos/packages
func (h *PackageHandler) ListPackages(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.ServicePackage.Query().Where(entpkg.TenantID(tid))
	if r.URL.Query().Get("is_active") != "false" {
		q = q.Where(entpkg.IsActive(true))
	}

	pkgs, err := q.Order(ent.Asc(entpkg.FieldName)).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": pkgs})
}

// CreatePackage handles POST /{tenantID}/pos/packages
func (h *PackageHandler) CreatePackage(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	var input struct {
		OutletID           string   `json:"outlet_id,omitempty"`
		Name               string   `json:"name"`
		Description        string   `json:"description,omitempty"`
		Price              float64  `json:"price"`
		Currency           string   `json:"currency,omitempty"`
		SessionsTotal      int      `json:"sessions_total"`
		ValidityDays       int      `json:"validity_days,omitempty"`
		ApplicableServices []string `json:"applicable_services,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.Name == "" || input.SessionsTotal == 0 {
		jsonError(w, "name and sessions_total are required", http.StatusBadRequest)
		return
	}

	c := h.db.ServicePackage.Create().
		SetTenantID(tid).
		SetName(input.Name).
		SetPrice(input.Price).
		SetSessionsTotal(input.SessionsTotal)

	if input.OutletID != "" {
		if oid, parseErr := uuid.Parse(input.OutletID); parseErr == nil {
			c.SetOutletID(oid)
		}
	}
	if input.Description != "" {
		c.SetDescription(input.Description)
	}
	if input.Currency != "" {
		c.SetCurrency(input.Currency)
	}
	if input.ValidityDays > 0 {
		c.SetValidityDays(input.ValidityDays)
	}
	if len(input.ApplicableServices) > 0 {
		c.SetApplicableServices(input.ApplicableServices)
	}

	pkg, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("create package failed", zap.Error(err))
		jsonError(w, "failed to create package: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, pkg)
}

// ListPurchases handles GET /{tenantID}/pos/packages/purchases?phone={phone}
func (h *PackageHandler) ListPurchases(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	q := h.db.ServicePackagePurchase.Query().Where(entpurch.TenantID(tid))

	if phone := r.URL.Query().Get("phone"); phone != "" {
		q = q.Where(entpurch.ClientPhone(phone))
	}
	if status := r.URL.Query().Get("status"); status != "" {
		q = q.Where(entpurch.Status(status))
	}

	purchases, err := q.Order(ent.Desc(entpurch.FieldCreatedAt)).All(r.Context())
	if err != nil {
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": purchases, "total": len(purchases)})
}

// SellPackage handles POST /{tenantID}/pos/packages/{packageID}/sell
func (h *PackageHandler) SellPackage(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	pkgID, err := uuid.Parse(chi.URLParam(r, "packageID"))
	if err != nil {
		jsonError(w, "invalid package_id", http.StatusBadRequest)
		return
	}

	pkg, err := h.db.ServicePackage.Query().
		Where(entpkg.ID(pkgID), entpkg.TenantID(tid), entpkg.IsActive(true)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "package not found or inactive", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	var input struct {
		ClientName  string `json:"client_name"`
		ClientPhone string `json:"client_phone"`
		POSOrderID  string `json:"pos_order_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if input.ClientName == "" || input.ClientPhone == "" {
		jsonError(w, "client_name and client_phone are required", http.StatusBadRequest)
		return
	}

	expiresAt := time.Now().AddDate(0, 0, pkg.ValidityDays)

	c := h.db.ServicePackagePurchase.Create().
		SetTenantID(tid).
		SetPackageID(pkgID).
		SetClientName(input.ClientName).
		SetClientPhone(input.ClientPhone).
		SetSessionsUsed(0).
		SetSessionsRemaining(pkg.SessionsTotal).
		SetExpiresAt(expiresAt)

	if input.POSOrderID != "" {
		if oid, parseErr := uuid.Parse(input.POSOrderID); parseErr == nil {
			c.SetPosOrderID(oid)
		}
	}

	purchase, err := c.Save(r.Context())
	if err != nil {
		h.log.Error("sell package failed", zap.Error(err))
		jsonError(w, "failed to sell package: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, purchase)
}

// RedeemSession handles POST /{tenantID}/pos/packages/purchases/{purchaseID}/redeem
func (h *PackageHandler) RedeemSession(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	purchaseID, err := uuid.Parse(chi.URLParam(r, "purchaseID"))
	if err != nil {
		jsonError(w, "invalid purchase_id", http.StatusBadRequest)
		return
	}

	purchase, err := h.db.ServicePackagePurchase.Query().
		Where(entpurch.ID(purchaseID), entpurch.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			jsonError(w, "purchase not found", http.StatusNotFound)
			return
		}
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if purchase.Status != "active" {
		jsonError(w, "package is "+purchase.Status, http.StatusBadRequest)
		return
	}
	if purchase.SessionsRemaining <= 0 {
		jsonError(w, "no sessions remaining", http.StatusBadRequest)
		return
	}
	if time.Now().After(purchase.ExpiresAt) {
		_, _ = purchase.Update().SetStatus("expired").Save(r.Context())
		jsonError(w, "package has expired", http.StatusBadRequest)
		return
	}

	var input struct {
		RedeemedBy           string `json:"redeemed_by"`
		ServiceCatalogItemID string `json:"service_catalog_item_id,omitempty"`
		POSOrderID           string `json:"pos_order_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	staffID, err := uuid.Parse(input.RedeemedBy)
	if err != nil {
		jsonError(w, "invalid redeemed_by", http.StatusBadRequest)
		return
	}

	// Create redemption record
	rc := h.db.ServicePackageRedemption.Create().
		SetTenantID(tid).
		SetPurchaseID(purchaseID).
		SetRedeemedBy(staffID)

	if input.ServiceCatalogItemID != "" {
		if cid, parseErr := uuid.Parse(input.ServiceCatalogItemID); parseErr == nil {
			rc.SetServiceCatalogItemID(cid)
		}
	}
	if input.POSOrderID != "" {
		if oid, parseErr := uuid.Parse(input.POSOrderID); parseErr == nil {
			rc.SetPosOrderID(oid)
		}
	}

	redemption, err := rc.Save(r.Context())
	if err != nil {
		h.log.Error("redeem session failed", zap.Error(err))
		jsonError(w, "failed to redeem session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update purchase balances
	newUsed := purchase.SessionsUsed + 1
	newRemaining := purchase.SessionsRemaining - 1
	newStatus := "active"
	if newRemaining == 0 {
		newStatus = "exhausted"
	}

	updatedPurchase, err := purchase.Update().
		SetSessionsUsed(newUsed).
		SetSessionsRemaining(newRemaining).
		SetStatus(newStatus).
		Save(r.Context())
	if err != nil {
		h.log.Error("update purchase balances failed", zap.Error(err))
	}

	_ = entredeem.FieldID // reference to confirm import used
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"redemption": redemption, "purchase": updatedPurchase})
}

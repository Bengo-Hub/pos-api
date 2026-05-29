package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
)

// ServiceSettingsHandler manages tenant/outlet POS configuration.
type ServiceSettingsHandler struct {
	log *zap.Logger
	db  *ent.Client
}

// requireConfigPermission checks that the caller holds pos.config.change (or pos.config.manage
// when requireManage is true). Platform owners and superuser/admin roles bypass the check.
// Returns true if the check passes; on failure it writes a 401/403 response and returns false.
func requireConfigPermission(w http.ResponseWriter, r *http.Request, requireManage bool) bool {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return false
	}
	if claims.IsPlatformOwner {
		return true
	}
	for _, role := range claims.Roles {
		switch role {
		case "superuser", "admin", "pos_admin", "super_admin":
			return true
		}
	}
	for _, p := range claims.Permissions {
		if p == "pos.config.manage" {
			return true
		}
		if !requireManage && p == "pos.config.change" {
			return true
		}
	}
	jsonError(w, "insufficient permissions — pos.config.change or pos.config.manage required", http.StatusForbidden)
	return false
}

func NewServiceSettingsHandler(log *zap.Logger, db *ent.Client) *ServiceSettingsHandler {
	return &ServiceSettingsHandler{log: log, db: db}
}

// settingsResponse is the API shape for outlet settings.
type settingsResponse struct {
	OutletID string `json:"outlet_id"`
	UseCase  string `json:"use_case"`
	// display
	DisplayMode       string `json:"display_mode"`
	ShowImages        bool   `json:"show_images"`
	ShowBarcodeScanner bool  `json:"show_barcode_scanner"`
	DefaultView       string `json:"default_view"`
	// receipt
	ReceiptHeader  *string `json:"receipt_header"`
	ReceiptFooter  *string `json:"receipt_footer"`
	Currency       string  `json:"currency"`
	VATEnabled     bool    `json:"vat_enabled"`
	VATRate        float64 `json:"vat_rate"`
	// printer
	PrinterType       string  `json:"printer_type"`
	PrinterIP         *string `json:"printer_ip"`
	PaperWidth        string  `json:"paper_width"`
	AutoPrintOrder    bool    `json:"auto_print_order"`
	AutoPrintKitchen  bool    `json:"auto_print_kitchen"`
	// modules
	EnableKDS             bool    `json:"enable_kds"`
	EnableAppointments    bool    `json:"enable_appointments"`
	HotelModuleEnabled    bool    `json:"hotel_module_enabled"`
	LayawayEnabled        bool    `json:"layaway_enabled"`
	ShiftReportsEnabled   bool    `json:"shift_reports_enabled"`
	// shift settings
	ShiftAutoEndEnabled bool `json:"shift_auto_end_enabled"`
	ShiftMaxHours       int  `json:"shift_max_hours"`
	// table settings
	TableMaxOccupationMinutes int `json:"table_max_occupation_minutes"`
	// returns policy
	ReturnWindowDays int `json:"return_window_days"`
	// printer profiles (multi-printer support)
	PrinterProfiles []map[string]any `json:"printer_profiles"`
	// terminal
	PINLoginMessage *string `json:"pin_login_message"`
	ScreensaverURL  *string `json:"screensaver_url"`
	UpdatedAt       string  `json:"updated_at"`
}

func toSettingsResponse(outlet *ent.Outlet, s *ent.OutletSetting) settingsResponse {
	useCase := ""
	if outlet.UseCase != nil {
		useCase = *outlet.UseCase
	}
	r := settingsResponse{
		OutletID:           outlet.ID.String(),
		UseCase:            useCase,
		DisplayMode:        s.DisplayMode,
		ShowImages:         s.ShowImages,
		ShowBarcodeScanner: s.ShowBarcodeScanner,
		DefaultView:        s.DefaultView,
		Currency:           s.Currency,
		VATEnabled:         s.VatEnabled,
		VATRate:            s.VatRate,
		PrinterType:        s.PrinterType,
		PaperWidth:         s.PaperWidth,
		AutoPrintOrder:     s.AutoPrintOrder,
		AutoPrintKitchen:   s.AutoPrintKitchen,
		EnableKDS:          s.EnableKds,
		EnableAppointments: s.EnableAppointments,
		HotelModuleEnabled: s.HotelModuleEnabled,
		LayawayEnabled:     s.LayawayEnabled,
		ShiftReportsEnabled: s.ShiftReportsEnabled,
		ReceiptHeader:      s.ReceiptHeader,
		ReceiptFooter:      s.ReceiptFooter,
		PrinterIP:          s.PrinterIP,
		ShiftAutoEndEnabled:       s.ShiftAutoEndEnabled,
		ShiftMaxHours:             s.ShiftMaxHours,
		TableMaxOccupationMinutes: s.TableMaxOccupationMinutes,
		ReturnWindowDays:          s.ReturnWindowDays,
		PrinterProfiles:           s.PrinterProfiles,
		PINLoginMessage:     s.PinLoginMessage,
		ScreensaverURL:      s.ScreensaverURL,
		UpdatedAt:           s.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
	return r
}

// getOrCreateSetting fetches the OutletSetting for outletID or creates a default one.
func (h *ServiceSettingsHandler) getOrCreateSetting(r *http.Request, outletID uuid.UUID) (*ent.OutletSetting, error) {
	ctx := r.Context()
	s, err := h.db.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(ctx)
	if err == nil {
		return s, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	// auto-create default settings
	return h.db.OutletSetting.Create().
		SetOutletID(outletID).
		Save(ctx)
}

// resolveOutlet returns the outlet for the request. Resolution order:
//  1. URL param outletID (explicit outlet-scoped endpoint)
//  2. Middleware-resolved outlet context (X-Outlet-ID header or staff assignment)
//  3. JWT claims OutletID (terminal PIN sessions embed the outlet in the token)
//  4. HQ outlet for the tenant
//  5. Any active outlet for the tenant (last resort)
func (h *ServiceSettingsHandler) resolveOutlet(r *http.Request, tenantID uuid.UUID) (*ent.Outlet, error) {
	ctx := r.Context()
	// Try URL param first (outlet-specific settings endpoint)
	if raw := chi.URLParam(r, "outletID"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, err
		}
		return h.db.Outlet.Query().
			Where(entoutlet.ID(id), entoutlet.TenantID(tenantID)).
			Only(ctx)
	}
	// Middleware-resolved outlet context
	oc := outletmw.OutletFromContext(ctx)
	if oc != nil {
		return h.db.Outlet.Query().
			Where(entoutlet.ID(oc.ID), entoutlet.TenantID(tenantID)).
			Only(ctx)
	}
	// JWT claims outlet_id (terminal sessions)
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims.OutletID != "" {
		if id, err := uuid.Parse(claims.OutletID); err == nil {
			o, err := h.db.Outlet.Query().
				Where(entoutlet.ID(id), entoutlet.TenantID(tenantID)).
				Only(ctx)
			if err == nil {
				return o, nil
			}
		}
	}
	// HQ outlet
	o, err := h.db.Outlet.Query().
		Where(entoutlet.TenantID(tenantID), entoutlet.IsHq(true)).
		First(ctx)
	if err == nil {
		return o, nil
	}
	// Any outlet for the tenant
	return h.db.Outlet.Query().
		Where(entoutlet.TenantID(tenantID)).
		First(ctx)
}

// GetSettings handles GET /{tenantID}/pos/settings and GET /{tenantID}/pos/outlets/{outletID}/settings
func (h *ServiceSettingsHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outlet, err := h.resolveOutlet(r, tid)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}
	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		h.log.Error("get settings", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toSettingsResponse(outlet, setting))
}

// updateSettingsInput covers all writable fields.
type updateSettingsInput struct {
	DisplayMode        *string  `json:"display_mode"`
	ShowImages         *bool    `json:"show_images"`
	ShowBarcodeScanner *bool    `json:"show_barcode_scanner"`
	DefaultView        *string  `json:"default_view"`
	ReceiptHeader      *string  `json:"receipt_header"`
	ReceiptFooter      *string  `json:"receipt_footer"`
	Currency           *string  `json:"currency"`
	VATEnabled         *bool    `json:"vat_enabled"`
	VATRate            *float64 `json:"vat_rate"`
	PrinterType        *string  `json:"printer_type"`
	PrinterIP          *string  `json:"printer_ip"`
	PaperWidth         *string  `json:"paper_width"`
	AutoPrintOrder     *bool              `json:"auto_print_order"`
	AutoPrintKitchen   *bool              `json:"auto_print_kitchen"`
	PrinterProfiles    []map[string]any   `json:"printer_profiles"`
	PINLoginMessage    *string            `json:"pin_login_message"`
	ScreensaverURL     *string            `json:"screensaver_url"`
	ReturnWindowDays   *int               `json:"return_window_days"`
}

// PutSettings handles PUT /{tenantID}/pos/settings and PUT /{tenantID}/pos/outlets/{outletID}/settings
func (h *ServiceSettingsHandler) PutSettings(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outlet, err := h.resolveOutlet(r, tid)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}

	var input updateSettingsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		h.log.Error("get settings for update", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}

	upd := setting.Update()
	if input.DisplayMode != nil {
		upd = upd.SetDisplayMode(*input.DisplayMode)
	}
	if input.ShowImages != nil {
		upd = upd.SetShowImages(*input.ShowImages)
	}
	if input.ShowBarcodeScanner != nil {
		upd = upd.SetShowBarcodeScanner(*input.ShowBarcodeScanner)
	}
	if input.DefaultView != nil {
		upd = upd.SetDefaultView(*input.DefaultView)
	}
	if input.ReceiptHeader != nil {
		upd = upd.SetReceiptHeader(*input.ReceiptHeader)
	}
	if input.ReceiptFooter != nil {
		upd = upd.SetReceiptFooter(*input.ReceiptFooter)
	}
	if input.Currency != nil {
		upd = upd.SetCurrency(*input.Currency)
	}
	if input.VATEnabled != nil {
		upd = upd.SetVatEnabled(*input.VATEnabled)
	}
	if input.VATRate != nil {
		upd = upd.SetVatRate(*input.VATRate)
	}
	if input.PrinterType != nil {
		upd = upd.SetPrinterType(*input.PrinterType)
	}
	if input.PrinterIP != nil {
		upd = upd.SetPrinterIP(*input.PrinterIP)
	}
	if input.PaperWidth != nil {
		upd = upd.SetPaperWidth(*input.PaperWidth)
	}
	if input.AutoPrintOrder != nil {
		upd = upd.SetAutoPrintOrder(*input.AutoPrintOrder)
	}
	if input.AutoPrintKitchen != nil {
		upd = upd.SetAutoPrintKitchen(*input.AutoPrintKitchen)
	}
	if input.PrinterProfiles != nil {
		upd = upd.SetPrinterProfiles(input.PrinterProfiles)
	}
	if input.PINLoginMessage != nil {
		upd = upd.SetPinLoginMessage(*input.PINLoginMessage)
	}
	if input.ScreensaverURL != nil {
		upd = upd.SetScreensaverURL(*input.ScreensaverURL)
	}
	if input.ReturnWindowDays != nil {
		upd = upd.SetReturnWindowDays(*input.ReturnWindowDays)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("update settings", zap.Error(err))
		jsonError(w, "failed to save settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toSettingsResponse(outlet, updated))
}

// modulesInput for PATCH /settings/modules — enables tenant admins to toggle modules.
type modulesInput struct {
	EnableKDS           *bool `json:"enable_kds"`
	EnableAppointments  *bool `json:"enable_appointments"`
	HotelModuleEnabled  *bool `json:"hotel_module_enabled"`
	LayawayEnabled      *bool `json:"layaway_enabled"`
	ShiftReportsEnabled *bool `json:"shift_reports_enabled"`
}

// PatchModules handles PATCH /{tenantID}/pos/settings/modules
// Requires pos.config.manage — toggling modules is a high-impact action restricted to admins.
func (h *ServiceSettingsHandler) PatchModules(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, true) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outlet, err := h.resolveOutlet(r, tid)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}

	var input modulesInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		h.log.Error("get settings for module patch", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}

	upd := setting.Update()
	if input.EnableKDS != nil {
		upd = upd.SetEnableKds(*input.EnableKDS)
	}
	if input.EnableAppointments != nil {
		upd = upd.SetEnableAppointments(*input.EnableAppointments)
	}
	if input.HotelModuleEnabled != nil {
		upd = upd.SetHotelModuleEnabled(*input.HotelModuleEnabled)
	}
	if input.LayawayEnabled != nil {
		upd = upd.SetLayawayEnabled(*input.LayawayEnabled)
	}
	if input.ShiftReportsEnabled != nil {
		upd = upd.SetShiftReportsEnabled(*input.ShiftReportsEnabled)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("patch modules", zap.Error(err))
		jsonError(w, "failed to save module settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toSettingsResponse(outlet, updated))
}

// shiftSettingsInput for PATCH /settings/shifts
type shiftSettingsInput struct {
	ShiftAutoEndEnabled *bool `json:"shift_auto_end_enabled"`
	ShiftMaxHours       *int  `json:"shift_max_hours"`
}

// PatchShiftSettings handles PATCH /{tenantID}/pos/settings/shifts
func (h *ServiceSettingsHandler) PatchShiftSettings(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outlet, err := h.resolveOutlet(r, tid)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}

	var input shiftSettingsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		h.log.Error("get settings for shift patch", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}

	upd := setting.Update()
	if input.ShiftAutoEndEnabled != nil {
		upd = upd.SetShiftAutoEndEnabled(*input.ShiftAutoEndEnabled)
	}
	if input.ShiftMaxHours != nil {
		hours := *input.ShiftMaxHours
		if hours < 1 {
			hours = 1
		}
		if hours > 24 {
			hours = 24
		}
		upd = upd.SetShiftMaxHours(hours)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("patch shift settings", zap.Error(err))
		jsonError(w, "failed to save shift settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toSettingsResponse(outlet, updated))
}

// switchOutletInput for POST /outlets/{outletID}/switch — TruLoad-inspired outlet context switch.
type switchOutletInput struct {
	OutletID string `json:"outlet_id"`
}

// SwitchOutlet validates that the requesting user can access the target outlet,
// then returns the outlet + its settings so the frontend can update its local context.
// For terminal (PIN) sessions, the client should re-issue a PIN login against the new outlet.
// GET /{tenantID}/pos/outlets/{outletID}/switch
func (h *ServiceSettingsHandler) SwitchOutlet(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	rawID := chi.URLParam(r, "outletID")
	outletID, err := uuid.Parse(rawID)
	if err != nil {
		jsonError(w, "invalid outlet_id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	claims, hasClaims := authclient.ClaimsFromContext(ctx)

	outlet, err := h.db.Outlet.Query().
		Where(entoutlet.ID(outletID), entoutlet.TenantID(tid)).
		WithSettings().
		Only(ctx)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}

	// HQ users and superusers can switch to any outlet.
	// Regular staff must be assigned to the target outlet.
	if hasClaims && !claims.IsPlatformOwner {
		isHQ := claims.CanAccessAllOutlets()
		isSuper := false
		for _, role := range claims.Roles {
			if role == "superuser" || role == "admin" || role == "pos_admin" {
				isSuper = true
				break
			}
		}
		if !isHQ && !isSuper {
			userID, parseErr := uuid.Parse(claims.Subject)
			if parseErr != nil {
				jsonError(w, "invalid user identity", http.StatusForbidden)
				return
			}
			assigned, _ := h.db.StaffMember.Query().
				Where(
					entstaff.TenantID(tid),
					entstaff.UserID(userID),
					entstaff.HasOutletsWith(entstaffoutlet.OutletID(outletID)),
				).Exist(ctx)
			if !assigned {
				jsonError(w, "you are not assigned to this outlet", http.StatusForbidden)
				return
			}
		}
	}

	useCase := ""
	if outlet.UseCase != nil {
		useCase = *outlet.UseCase
	}

	var settings *settingsResponse
	if outlet.Edges.Settings != nil {
		sr := toSettingsResponse(outlet, outlet.Edges.Settings)
		settings = &sr
	}

	jsonOK(w, map[string]any{
		"outlet": map[string]any{
			"id":       outlet.ID.String(),
			"code":     outlet.Code,
			"name":     outlet.Name,
			"use_case": useCase,
			"is_hq":    outlet.IsHq,
			"status":   outlet.Status,
		},
		"settings": settings,
	})
}

// outletConfigInput for PATCH /settings/outlet — updates outlet-level config (e.g. use_case).
type outletConfigInput struct {
	UseCase *string `json:"use_case"`
}

// PatchOutletConfig handles PATCH /{tenantID}/pos/settings/outlet
// Updates outlet-level configuration such as use_case. Requires pos.config.manage
// because changing use_case reconfigures the entire outlet feature set.
func (h *ServiceSettingsHandler) PatchOutletConfig(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, true) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outlet, err := h.resolveOutlet(r, tid)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}

	var input outletConfigInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	upd := outlet.Update()
	if input.UseCase != nil {
		upd = upd.SetNillableUseCase(input.UseCase)
	}
	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("patch outlet config", zap.Error(err))
		jsonError(w, "failed to update outlet", http.StatusInternalServerError)
		return
	}

	setting, err := h.getOrCreateSetting(r, updated.ID)
	if err != nil {
		h.log.Error("get settings after outlet config patch", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toSettingsResponse(updated, setting))
}

// tableSettingsInput for PATCH /settings/tables
type tableSettingsInput struct {
	TableMaxOccupationMinutes *int `json:"table_max_occupation_minutes"`
}

// PatchTableSettings handles PATCH /{tenantID}/pos/settings/tables
func (h *ServiceSettingsHandler) PatchTableSettings(w http.ResponseWriter, r *http.Request) {
	if !requireConfigPermission(w, r, false) {
		return
	}
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	outlet, err := h.resolveOutlet(r, tid)
	if err != nil {
		jsonError(w, "outlet not found", http.StatusNotFound)
		return
	}

	var input tableSettingsInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		h.log.Error("get settings for table patch", zap.Error(err))
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}

	upd := setting.Update()
	if input.TableMaxOccupationMinutes != nil {
		mins := *input.TableMaxOccupationMinutes
		if mins < 0 {
			mins = 0
		}
		upd = upd.SetTableMaxOccupationMinutes(mins)
	}

	updated, err := upd.Save(r.Context())
	if err != nil {
		h.log.Error("patch table settings", zap.Error(err))
		jsonError(w, "failed to save table settings", http.StatusInternalServerError)
		return
	}
	jsonOK(w, toSettingsResponse(outlet, updated))
}

// RegisterRoutes registers settings routes under the tenant router group.
func (h *ServiceSettingsHandler) RegisterRoutes(r chi.Router) {
	r.Get("/pos/settings", h.GetSettings)
	r.Put("/pos/settings", h.PutSettings)
	r.Patch("/pos/settings/modules", h.PatchModules)
	r.Patch("/pos/settings/shifts", h.PatchShiftSettings)
	r.Patch("/pos/settings/tables", h.PatchTableSettings)
	r.Patch("/pos/settings/outlet", h.PatchOutletConfig)
	r.Get("/pos/outlets/{outletID}/settings", h.GetSettings)
	r.Put("/pos/outlets/{outletID}/settings", h.PutSettings)
	// TruLoad-inspired outlet switch endpoint
	r.Post("/pos/outlets/{outletID}/switch", h.SwitchOutlet)
}

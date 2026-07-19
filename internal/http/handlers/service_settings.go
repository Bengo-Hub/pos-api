package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entprintagent "github.com/bengobox/pos-service/internal/ent/printagent"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/printing"
	"github.com/bengobox/pos-service/internal/modules/printing/layouts"
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
		case "superuser", "admin", "pos_admin", "super_admin", "manager", "store_manager", "outlet_manager", "owner":
			return true
		}
	}
	for _, p := range claims.Permissions {
		if p == "pos.config.manage" {
			return true
		}
		// config.change satisfies both view and manage-level config edits (settings/policy);
		// managers/cashiers granted config.change can save settings.
		if p == "pos.config.change" {
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
	DisplayMode        string `json:"display_mode"`
	ShowImages         bool   `json:"show_images"`
	ShowBarcodeScanner bool   `json:"show_barcode_scanner"`
	DefaultView        string `json:"default_view"`
	// receipt
	ReceiptHeader *string `json:"receipt_header"`
	ReceiptFooter *string `json:"receipt_footer"`
	// ShowLogoOnReceipt: include the tenant/outlet logo on generated receipts (HTML/PDF/client).
	// Stored in the freeform metadata (receipt_show_logo); defaults to true when unset.
	ShowLogoOnReceipt bool    `json:"show_logo_on_receipt"`
	Currency          string  `json:"currency"`
	VATEnabled        bool    `json:"vat_enabled"`
	VATRate           float64 `json:"vat_rate"`
	// printer
	PrinterType      string  `json:"printer_type"`
	PrinterIP        *string `json:"printer_ip"`
	PaperWidth       string  `json:"paper_width"`
	AutoPrintOrder   bool    `json:"auto_print_order"`
	AutoPrintKitchen bool    `json:"auto_print_kitchen"`
	// receipt layout — the outlet's receipt_format setting ("auto" = best layout per use
	// case) plus the selectable layouts from the printing/layouts registry, so the settings
	// UI picker is registry-driven rather than hardcoded.
	ReceiptFormat           string           `json:"receipt_format"`
	AvailableReceiptFormats []layouts.Layout `json:"available_receipt_formats"`
	// modules
	EnableKDS           bool `json:"enable_kds"`
	EnableAppointments  bool `json:"enable_appointments"`
	HotelModuleEnabled  bool `json:"hotel_module_enabled"`
	LayawayEnabled      bool `json:"layaway_enabled"`
	ShiftReportsEnabled bool `json:"shift_reports_enabled"`
	// sidebar visibility — outlet admins hide whole modules (by moduleKey) or individual sidebar
	// items (by href) to declutter the app to only the screens they use. Stored in metadata.
	// The flat lists apply to ALL roles (tenant-wide); the *ByRole maps hide additionally for a
	// specific role code only. Platform-owner/superuser users are exempt from both (enforced in the UI).
	DisabledModules       []string            `json:"disabled_modules"`
	HiddenItems           []string            `json:"hidden_items"`
	DisabledModulesByRole map[string][]string `json:"disabled_modules_by_role"`
	HiddenItemsByRole     map[string][]string `json:"hidden_items_by_role"`
	// shift settings
	ShiftAutoEndEnabled bool `json:"shift_auto_end_enabled"`
	ShiftMaxHours       int  `json:"shift_max_hours"`
	// table settings
	TableMaxOccupationMinutes int `json:"table_max_occupation_minutes"`
	// returns policy
	ReturnWindowDays int `json:"return_window_days"`
	// discount control (exceeding EITHER limit triggers the manager step-up). DiscountLimitType
	// is the Settings-page UI selector for which field is the ACTIVE one (percent XOR amount —
	// the page shows a single input at a time); the inactive field is always saved at its
	// no-op sentinel (100% / 0), so the order-create enforcement gate is unaffected.
	MaxDiscountPercent float64 `json:"max_discount_percent"`
	MaxDiscountAmount  float64 `json:"max_discount_amount"`
	DiscountLimitType  string  `json:"discount_limit_type"`
	// pricing policy — cashier price-edit rules (see OutletSetting schema comments)
	AllowPriceAboveBase      bool `json:"allow_price_above_base"`
	RequireApprovalBelowBase bool `json:"require_approval_below_base"`
	// printer profiles (multi-printer support)
	PrinterProfiles []map[string]any `json:"printer_profiles"`
	// cash drawer (ESC/POS drawer kick via assigned printer)
	CashDrawerEnabled  bool    `json:"cash_drawer_enabled"`
	CashDrawerPrinter  *string `json:"cash_drawer_printer"`
	CashDrawerAutoOpen bool    `json:"cash_drawer_auto_open"`
	CashDrawerKickCode string  `json:"cash_drawer_kick_code"`
	// card terminal / PDQ
	CardTerminalMode       string  `json:"card_terminal_mode"`
	CardTerminalProvider   *string `json:"card_terminal_provider"`
	CardTerminalTID        *string `json:"card_terminal_tid"`
	CardTerminalRequireRef bool    `json:"card_terminal_require_ref"`
	// terminal
	PINLoginMessage *string `json:"pin_login_message"`
	ScreensaverURL  *string `json:"screensaver_url"`
	// Up to 3 admin-managed screensaver media URLs (relative /media/... paths, stored in
	// metadata; files managed via the /settings/screensavers endpoints).
	ScreensaverURLs []string `json:"screensaver_urls"`
	// payment display — shown on receipts when ShowPaymentInfoOnReceipt is true
	MpesaPaybill             *string `json:"mpesa_paybill"`
	MpesaAccountReference    *string `json:"mpesa_account_reference"`
	AirtelMoneyNumber        *string `json:"airtel_money_number"`
	MpesaTill                *string `json:"mpesa_till"`
	MpesaPochi               *string `json:"mpesa_pochi"`
	BankName                 *string `json:"bank_name"`
	BankAccountNumber        *string `json:"bank_account_number"`
	BankAccountName          *string `json:"bank_account_name"`
	ShowPaymentInfoOnReceipt bool    `json:"show_payment_info_on_receipt"`
	// print_agent_online: a paired Local Print Agent polled recently — the till should rely on
	// server-side background print jobs instead of its client-side transports.
	PrintAgentOnline bool   `json:"print_agent_online"`
	UpdatedAt        string `json:"updated_at"`
}

func toSettingsResponse(outlet *ent.Outlet, s *ent.OutletSetting) settingsResponse {
	useCase := ""
	if outlet.UseCase != nil {
		useCase = *outlet.UseCase
	}
	r := settingsResponse{
		OutletID:                  outlet.ID.String(),
		UseCase:                   useCase,
		DisplayMode:               s.DisplayMode,
		ShowImages:                s.ShowImages,
		ShowBarcodeScanner:        s.ShowBarcodeScanner,
		DefaultView:               s.DefaultView,
		Currency:                  s.Currency,
		VATEnabled:                s.VatEnabled,
		VATRate:                   s.VatRate,
		PrinterType:               s.PrinterType,
		PaperWidth:                s.PaperWidth,
		ReceiptFormat:             string(s.ReceiptFormat),
		AvailableReceiptFormats:   layouts.All(),
		AutoPrintOrder:            s.AutoPrintOrder,
		AutoPrintKitchen:          s.AutoPrintKitchen,
		EnableKDS:                 s.EnableKds,
		EnableAppointments:        s.EnableAppointments,
		HotelModuleEnabled:        s.HotelModuleEnabled,
		LayawayEnabled:            s.LayawayEnabled,
		ShiftReportsEnabled:       s.ShiftReportsEnabled,
		ReceiptHeader:             s.ReceiptHeader,
		ReceiptFooter:             s.ReceiptFooter,
		ShowLogoOnReceipt:         metaBoolDefault(s.Metadata, "receipt_show_logo", true),
		PrinterIP:                 s.PrinterIP,
		ShiftAutoEndEnabled:       s.ShiftAutoEndEnabled,
		ShiftMaxHours:             s.ShiftMaxHours,
		TableMaxOccupationMinutes: s.TableMaxOccupationMinutes,
		ReturnWindowDays:          s.ReturnWindowDays,
		MaxDiscountPercent:        s.MaxDiscountPercent,
		MaxDiscountAmount:         s.MaxDiscountAmount,
		DiscountLimitType:         string(s.DiscountLimitType),
		AllowPriceAboveBase:       s.AllowPriceAboveBase,
		RequireApprovalBelowBase:  s.RequireApprovalBelowBase,
		PrinterProfiles:           s.PrinterProfiles,
		CashDrawerEnabled:         s.CashDrawerEnabled,
		CashDrawerPrinter:         s.CashDrawerPrinter,
		CashDrawerAutoOpen:        s.CashDrawerAutoOpen,
		CashDrawerKickCode:        s.CashDrawerKickCode,
		CardTerminalMode:          s.CardTerminalMode,
		CardTerminalProvider:      s.CardTerminalProvider,
		CardTerminalTID:           s.CardTerminalTid,
		CardTerminalRequireRef:    s.CardTerminalRequireRef,
		PINLoginMessage:           s.PinLoginMessage,
		ScreensaverURL:            s.ScreensaverURL,
		ScreensaverURLs:           metaStringSlice(s.Metadata, "screensaver_urls"),
		MpesaPaybill:              s.MpesaPaybill,
		MpesaAccountReference:     s.MpesaAccountReference,
		AirtelMoneyNumber:         s.AirtelMoneyNumber,
		MpesaTill:                 s.MpesaTill,
		MpesaPochi:                s.MpesaPochi,
		BankName:                  s.BankName,
		BankAccountNumber:         s.BankAccountNumber,
		BankAccountName:           s.BankAccountName,
		ShowPaymentInfoOnReceipt:  s.ShowPaymentInfoOnReceipt,
		DisabledModules:           metaStringSlice(s.Metadata, "disabled_modules"),
		HiddenItems:               metaStringSlice(s.Metadata, "hidden_items"),
		DisabledModulesByRole:     metaStringSliceMap(s.Metadata, "disabled_modules_by_role"),
		HiddenItemsByRole:         metaStringSliceMap(s.Metadata, "hidden_items_by_role"),
		UpdatedAt:                 s.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
	return r
}

// metaBoolDefault reads a bool from the freeform metadata JSON, falling back to def when the key
// is absent or not a bool (settings rows predating the key keep the historical behaviour).
func metaBoolDefault(meta map[string]any, key string, def bool) bool {
	if meta == nil {
		return def
	}
	if b, ok := meta[key].(bool); ok {
		return b
	}
	return def
}

// metaStringSlice reads a string list stored in the freeform metadata JSON. It handles both shapes:
// []any (after a DB/JSON round-trip) and []string (the in-memory value right after SetMetadata).
// Always returns a non-nil slice so the API emits [] not null.
func metaStringSlice(meta map[string]any, key string) []string {
	out := []string{}
	if meta == nil {
		return out
	}
	switch raw := meta[key].(type) {
	case []string:
		for _, s := range raw {
			if s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// metaStringSliceMap reads a map[roleCode][]string stored in the freeform metadata JSON (used by the
// per-role sidebar-hiding lists). Handles the map[string]any shape after a DB/JSON round-trip as well
// as the in-memory map[string][]string. Always returns a non-nil map so the API emits {} not null.
func metaStringSliceMap(meta map[string]any, key string) map[string][]string {
	out := map[string][]string{}
	if meta == nil {
		return out
	}
	switch raw := meta[key].(type) {
	case map[string][]string:
		for role, list := range raw {
			if vals := dedupeStrings(list); len(vals) > 0 {
				out[role] = vals
			}
		}
	case map[string]any:
		for role, v := range raw {
			list := []string{}
			switch vv := v.(type) {
			case []string:
				list = vv
			case []any:
				for _, e := range vv {
					if s, ok := e.(string); ok {
						list = append(list, s)
					}
				}
			}
			if vals := dedupeStrings(list); len(vals) > 0 {
				out[role] = vals
			}
		}
	}
	return out
}

// dedupeStrings trims, drops empties, and removes duplicates while preserving first-seen order.
func dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// dedupeStringSliceMap dedupes each role's list and drops roles left with no entries, so the stored
// per-role hiding map stays clean (no empty buckets, no duplicate hrefs).
func dedupeStringSliceMap(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for role, list := range in {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		if vals := dedupeStrings(list); len(vals) > 0 {
			out[role] = vals
		}
	}
	return out
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
	// auto-create default settings. KDS is core operations for hospitality/quick_service outlets
	// (kitchen/bar ticket routing) — default it on so a fresh outlet doesn't need a manual toggle
	// before the KDS Stations screen becomes usable.
	create := h.db.OutletSetting.Create().SetOutletID(outletID)
	if outlet, oErr := h.db.Outlet.Get(ctx, outletID); oErr == nil {
		if uc := outlet.UseCase; uc != nil && (*uc == "hospitality" || *uc == "quick_service") {
			create = create.SetEnableKds(true)
		}
	}
	return create.Save(ctx)
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
	resp := toSettingsResponse(outlet, setting)
	resp.PrintAgentOnline = h.printAgentOnline(r.Context(), tid, outlet.ID)
	jsonOK(w, resp)
}

// printAgentOnline reports whether a paired, non-revoked Local Print Agent for the outlet polled
// within the liveness window (same rule as printing.Queue.AgentOnline).
func (h *ServiceSettingsHandler) printAgentOnline(ctx context.Context, tenantID, outletID uuid.UUID) bool {
	ok, err := h.db.PrintAgent.Query().
		Where(
			entprintagent.TenantID(tenantID),
			entprintagent.OutletID(outletID),
			entprintagent.Revoked(false),
			entprintagent.LastSeenAtGTE(time.Now().Add(-printing.AgentOnlineWindow)),
		).
		Exist(ctx)
	return err == nil && ok
}

// updateSettingsInput covers all writable fields.
type updateSettingsInput struct {
	DisplayMode        *string          `json:"display_mode"`
	ShowImages         *bool            `json:"show_images"`
	ShowBarcodeScanner *bool            `json:"show_barcode_scanner"`
	DefaultView        *string          `json:"default_view"`
	ReceiptHeader      *string          `json:"receipt_header"`
	ReceiptFooter      *string          `json:"receipt_footer"`
	ShowLogoOnReceipt  *bool            `json:"show_logo_on_receipt"`
	Currency           *string          `json:"currency"`
	VATEnabled         *bool            `json:"vat_enabled"`
	VATRate            *float64         `json:"vat_rate"`
	PrinterType        *string          `json:"printer_type"`
	PrinterIP          *string          `json:"printer_ip"`
	PaperWidth         *string          `json:"paper_width"`
	ReceiptFormat      *string          `json:"receipt_format"`
	AutoPrintOrder     *bool            `json:"auto_print_order"`
	AutoPrintKitchen   *bool            `json:"auto_print_kitchen"`
	PrinterProfiles    []map[string]any `json:"printer_profiles"`
	PINLoginMessage    *string          `json:"pin_login_message"`
	ScreensaverURL     *string          `json:"screensaver_url"`
	ReturnWindowDays   *int             `json:"return_window_days"`
	MaxDiscountPercent *float64         `json:"max_discount_percent"`
	MaxDiscountAmount  *float64         `json:"max_discount_amount"`
	DiscountLimitType  *string          `json:"discount_limit_type"`
	// pricing policy
	AllowPriceAboveBase      *bool `json:"allow_price_above_base"`
	RequireApprovalBelowBase *bool `json:"require_approval_below_base"`
	// cash drawer
	CashDrawerEnabled  *bool   `json:"cash_drawer_enabled"`
	CashDrawerPrinter  *string `json:"cash_drawer_printer"`
	CashDrawerAutoOpen *bool   `json:"cash_drawer_auto_open"`
	CashDrawerKickCode *string `json:"cash_drawer_kick_code"`
	// card terminal / PDQ
	CardTerminalMode       *string `json:"card_terminal_mode"`
	CardTerminalProvider   *string `json:"card_terminal_provider"`
	CardTerminalTID        *string `json:"card_terminal_tid"`
	CardTerminalRequireRef *bool   `json:"card_terminal_require_ref"`
	// payment display fields
	MpesaPaybill             *string `json:"mpesa_paybill"`
	MpesaAccountReference    *string `json:"mpesa_account_reference"`
	AirtelMoneyNumber        *string `json:"airtel_money_number"`
	MpesaTill                *string `json:"mpesa_till"`
	MpesaPochi               *string `json:"mpesa_pochi"`
	BankName                 *string `json:"bank_name"`
	BankAccountNumber        *string `json:"bank_account_number"`
	BankAccountName          *string `json:"bank_account_name"`
	ShowPaymentInfoOnReceipt *bool   `json:"show_payment_info_on_receipt"`
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
	if input.MaxDiscountPercent != nil {
		upd = upd.SetMaxDiscountPercent(*input.MaxDiscountPercent)
	}
	if input.MaxDiscountAmount != nil {
		upd = upd.SetMaxDiscountAmount(*input.MaxDiscountAmount)
	}
	if input.DiscountLimitType != nil {
		switch *input.DiscountLimitType {
		case "percent":
			upd = upd.SetDiscountLimitType(entoutletsetting.DiscountLimitTypePercent)
		case "amount":
			upd = upd.SetDiscountLimitType(entoutletsetting.DiscountLimitTypeAmount)
		default:
			jsonError(w, "invalid discount_limit_type", http.StatusBadRequest)
			return
		}
	}
	if input.AllowPriceAboveBase != nil {
		upd = upd.SetAllowPriceAboveBase(*input.AllowPriceAboveBase)
	}
	if input.RequireApprovalBelowBase != nil {
		upd = upd.SetRequireApprovalBelowBase(*input.RequireApprovalBelowBase)
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
	if input.ReceiptFormat != nil {
		// Validate against the layouts registry so an unknown/misspelled format can never be
		// stored ("" and "auto" both mean the auto default).
		if !layouts.Valid(*input.ReceiptFormat) {
			jsonError(w, "invalid receipt_format", http.StatusBadRequest)
			return
		}
		v := *input.ReceiptFormat
		if v == "" {
			v = layouts.FormatAuto
		}
		upd = upd.SetReceiptFormat(entoutletsetting.ReceiptFormat(v))
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
	if input.CashDrawerEnabled != nil {
		upd = upd.SetCashDrawerEnabled(*input.CashDrawerEnabled)
	}
	if input.CashDrawerPrinter != nil {
		upd = upd.SetNillableCashDrawerPrinter(input.CashDrawerPrinter)
	}
	if input.CashDrawerAutoOpen != nil {
		upd = upd.SetCashDrawerAutoOpen(*input.CashDrawerAutoOpen)
	}
	if input.CashDrawerKickCode != nil {
		upd = upd.SetCashDrawerKickCode(*input.CashDrawerKickCode)
	}
	if input.CardTerminalMode != nil {
		upd = upd.SetCardTerminalMode(*input.CardTerminalMode)
	}
	if input.CardTerminalProvider != nil {
		upd = upd.SetNillableCardTerminalProvider(input.CardTerminalProvider)
	}
	if input.CardTerminalTID != nil {
		upd = upd.SetNillableCardTerminalTid(input.CardTerminalTID)
	}
	if input.CardTerminalRequireRef != nil {
		upd = upd.SetCardTerminalRequireRef(*input.CardTerminalRequireRef)
	}
	if input.MpesaPaybill != nil {
		upd = upd.SetNillableMpesaPaybill(input.MpesaPaybill)
	}
	if input.MpesaAccountReference != nil {
		upd = upd.SetNillableMpesaAccountReference(input.MpesaAccountReference)
	}
	if input.AirtelMoneyNumber != nil {
		upd = upd.SetNillableAirtelMoneyNumber(input.AirtelMoneyNumber)
	}
	if input.MpesaTill != nil {
		upd = upd.SetNillableMpesaTill(input.MpesaTill)
	}
	if input.MpesaPochi != nil {
		upd = upd.SetNillableMpesaPochi(input.MpesaPochi)
	}
	if input.BankName != nil {
		upd = upd.SetNillableBankName(input.BankName)
	}
	if input.BankAccountNumber != nil {
		upd = upd.SetNillableBankAccountNumber(input.BankAccountNumber)
	}
	if input.BankAccountName != nil {
		upd = upd.SetNillableBankAccountName(input.BankAccountName)
	}
	if input.ShowPaymentInfoOnReceipt != nil {
		upd = upd.SetShowPaymentInfoOnReceipt(*input.ShowPaymentInfoOnReceipt)
	}
	// Logo on receipts — freeform-metadata key (no schema migration), merged into a copy so
	// other metadata keys (sidebar lists, booking policy, screensavers) are preserved.
	if input.ShowLogoOnReceipt != nil {
		meta := map[string]any{}
		for k, v := range setting.Metadata {
			meta[k] = v
		}
		meta["receipt_show_logo"] = *input.ShowLogoOnReceipt
		upd = upd.SetMetadata(meta)
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
	// Sidebar visibility (whole-module by moduleKey / individual sidebar item by href). A nil
	// pointer leaves the stored list untouched; a non-nil (possibly empty) list replaces it.
	// The flat lists apply tenant-wide; the *ByRole maps (role code → hrefs/module keys) hide
	// additionally for a specific role only. A nil map pointer leaves the stored map untouched.
	DisabledModules       *[]string             `json:"disabled_modules"`
	HiddenItems           *[]string             `json:"hidden_items"`
	DisabledModulesByRole *map[string][]string  `json:"disabled_modules_by_role"`
	HiddenItemsByRole     *map[string][]string  `json:"hidden_items_by_role"`
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

	// Sidebar visibility lists live in the freeform metadata (no schema migration). Merge into a
	// copy of the existing metadata so other keys (e.g. booking_policy) are preserved.
	if input.DisabledModules != nil || input.HiddenItems != nil ||
		input.DisabledModulesByRole != nil || input.HiddenItemsByRole != nil {
		meta := map[string]any{}
		for k, v := range setting.Metadata {
			meta[k] = v
		}
		if input.DisabledModules != nil {
			meta["disabled_modules"] = dedupeStrings(*input.DisabledModules)
		}
		if input.HiddenItems != nil {
			meta["hidden_items"] = dedupeStrings(*input.HiddenItems)
		}
		if input.DisabledModulesByRole != nil {
			meta["disabled_modules_by_role"] = dedupeStringSliceMap(*input.DisabledModulesByRole)
		}
		if input.HiddenItemsByRole != nil {
			meta["hidden_items_by_role"] = dedupeStringSliceMap(*input.HiddenItemsByRole)
		}
		upd = upd.SetMetadata(meta)
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

// bookingPolicyInput is the editable amendment/cancellation policy (stored in
// OutletSetting.metadata["booking_policy"]). Mirrors the shape pos-api enforces in
// hotel booking amend/cancel (free windows + fees keyed off proximity to arrival).
type bookingPolicyInput struct {
	FreeAmendmentWindowHours *float64 `json:"free_amendment_window_hours"`
	CancellationWindowHours  *float64 `json:"cancellation_window_hours"`
	AmendmentFee             *float64 `json:"amendment_fee"`
	CancellationFee          *float64 `json:"cancellation_fee"`
	Currency                 *string  `json:"currency"`
	// PaymentTiming controls when the room charge is taken: settle_at_checkout | pay_upfront |
	// per_day_split. Folio extras are ALWAYS settled at checkout regardless.
	PaymentTiming *string `json:"payment_timing"`
}

func defaultPolicyMap() map[string]any {
	return map[string]any{
		"free_amendment_window_hours": 48.0,
		"cancellation_window_hours":   72.0,
		"amendment_fee":               0.0,
		"cancellation_fee":            0.0,
		"currency":                    "KES",
		// Default to the long-standing behaviour: room is settled together with extras at checkout.
		"payment_timing": "settle_at_checkout",
	}
}

// GetBookingPolicy handles GET /{tenantID}/pos/settings/booking-policy.
func (h *ServiceSettingsHandler) GetBookingPolicy(w http.ResponseWriter, r *http.Request) {
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
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}
	policy := defaultPolicyMap()
	if setting.Metadata != nil {
		if bp, ok := setting.Metadata["booking_policy"].(map[string]any); ok {
			for k, v := range bp {
				policy[k] = v
			}
		}
	}
	jsonOK(w, policy)
}

// PatchBookingPolicy handles PATCH /{tenantID}/pos/settings/booking-policy —
// merges the provided fields into OutletSetting.metadata["booking_policy"].
func (h *ServiceSettingsHandler) PatchBookingPolicy(w http.ResponseWriter, r *http.Request) {
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
	var in bookingPolicyInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	setting, err := h.getOrCreateSetting(r, outlet.ID)
	if err != nil {
		jsonError(w, "failed to load settings", http.StatusInternalServerError)
		return
	}

	meta := map[string]any{}
	for k, v := range setting.Metadata {
		meta[k] = v
	}
	policy := defaultPolicyMap()
	if existing, ok := meta["booking_policy"].(map[string]any); ok {
		for k, v := range existing {
			policy[k] = v
		}
	}
	if in.FreeAmendmentWindowHours != nil {
		policy["free_amendment_window_hours"] = *in.FreeAmendmentWindowHours
	}
	if in.CancellationWindowHours != nil {
		policy["cancellation_window_hours"] = *in.CancellationWindowHours
	}
	if in.AmendmentFee != nil {
		policy["amendment_fee"] = *in.AmendmentFee
	}
	if in.CancellationFee != nil {
		policy["cancellation_fee"] = *in.CancellationFee
	}
	if in.Currency != nil && *in.Currency != "" {
		policy["currency"] = *in.Currency
	}
	if in.PaymentTiming != nil {
		switch *in.PaymentTiming {
		case "settle_at_checkout", "pay_upfront", "per_day_split":
			policy["payment_timing"] = *in.PaymentTiming
		}
	}
	meta["booking_policy"] = policy

	if _, err := setting.Update().SetMetadata(meta).Save(r.Context()); err != nil {
		h.log.Error("patch booking policy", zap.Error(err))
		jsonError(w, "failed to save booking policy", http.StatusInternalServerError)
		return
	}
	jsonOK(w, policy)
}

// RegisterRoutes registers settings routes under the tenant router group.
func (h *ServiceSettingsHandler) RegisterRoutes(r chi.Router) {
	r.Get("/pos/settings", h.GetSettings)
	r.Put("/pos/settings", h.PutSettings)
	r.Patch("/pos/settings/modules", h.PatchModules)
	r.Patch("/pos/settings/shifts", h.PatchShiftSettings)
	r.Patch("/pos/settings/tables", h.PatchTableSettings)
	r.Patch("/pos/settings/outlet", h.PatchOutletConfig)
	r.Get("/pos/settings/booking-policy", h.GetBookingPolicy)
	r.Patch("/pos/settings/booking-policy", h.PatchBookingPolicy)
	r.Get("/pos/outlets/{outletID}/settings", h.GetSettings)
	r.Put("/pos/outlets/{outletID}/settings", h.PutSettings)
	// TruLoad-inspired outlet switch endpoint
	r.Post("/pos/outlets/{outletID}/switch", h.SwitchOutlet)
}

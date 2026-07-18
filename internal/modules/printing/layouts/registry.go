package layouts

// Layout ids. "auto" is a SETTING value, not a layout — Resolve maps it to a concrete
// layout per use case. Every id here has an HTML and a PDF renderer (render.go) and a
// matching client-side renderer in pos-ui (offline-first browser printing).
const (
	// FormatAuto is the default outlet setting: the platform picks the best layout for the
	// outlet's use case (thermal — the layout that prints crisp on receipt printers).
	FormatAuto = "auto"
	// ThermalClassic is the receipt-roll layout in bold monospace (Courier) — the proven
	// hospitality look: dashed separators, high contrast, 58/80mm paper.
	ThermalClassic = "thermal_classic"
	// ThermalModern is the same receipt-roll layout in a bold sans-serif (Helvetica/Arial)
	// — crisper glyphs on browser/PDF prints (the reference ETR-style retail receipt).
	// ESC/POS hardware output is identical to classic (thermal heads have one font).
	ThermalModern = "thermal_modern"
	// A4Invoice is the boxed invoice-style sheet (bordered tables, Code 128 barcode) for
	// outlets that print receipts on regular A4/letter office printers.
	A4Invoice = "a4_invoice"
)

// Layout describes a selectable receipt layout — the registry is the single source the
// settings API exposes to the UI picker, so adding a layout here (plus its renderers)
// makes it configurable everywhere.
type Layout struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// Paper is the print geometry the layout is designed for: "thermal" (58/80mm roll,
	// honours the outlet paper_width setting) or "a4" (portrait sheet).
	Paper string `json:"paper"`
}

// All returns the selectable layouts in display order. "auto" is deliberately not in the
// list — the settings UI renders it as its own "recommended" option resolving per use case.
func All() []Layout {
	return []Layout{
		{
			ID:          ThermalModern,
			Label:       "Thermal — Modern",
			Description: "Receipt-roll layout in a bold sans-serif. Crisp high-contrast print (recommended for retail).",
			Paper:       "thermal",
		},
		{
			ID:          ThermalClassic,
			Label:       "Thermal — Classic",
			Description: "Receipt-roll layout in bold monospace with dashed separators (the classic POS look).",
			Paper:       "thermal",
		},
		{
			ID:          A4Invoice,
			Label:       "A4 Invoice",
			Description: "Boxed invoice-style sheet with bordered tables and barcode, for regular A4 printers.",
			Paper:       "a4",
		},
	}
}

// Valid reports whether v is an acceptable receipt_format SETTING value ("auto" or a
// concrete layout id). Used by the settings PUT validation.
func Valid(v string) bool {
	if v == "" || v == FormatAuto {
		return true
	}
	for _, l := range All() {
		if l.ID == v {
			return true
		}
	}
	return false
}

// Resolve maps the outlet's receipt_format setting to a concrete layout id.
// "auto" (or empty/unknown) picks the best layout for the use case: thermal always —
// receipts print on receipt printers; the A4 sheet is strictly opt-in. Retail gets the
// modern sans variant (the crisp ETR-style look), everything else keeps the classic
// monospace look hospitality tenants already print today.
func Resolve(setting, useCase string) string {
	switch setting {
	case ThermalClassic, ThermalModern, A4Invoice:
		return setting
	}
	if useCase == "retail" {
		return ThermalModern
	}
	return ThermalClassic
}

// IsThermal reports whether the layout renders on receipt-roll geometry (58/80mm).
func IsThermal(layout string) bool { return layout != A4Invoice }

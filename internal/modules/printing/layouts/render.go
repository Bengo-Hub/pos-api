package layouts

// RenderHTML renders the receipt with its resolved layout (rec.Layout; a concrete id from
// Resolve — unknown/empty falls back through Resolve again so old callers never 500).
func RenderHTML(rec Receipt, logoURL string) []byte {
	switch Resolve(rec.Layout, rec.UseCase) {
	case A4Invoice:
		return renderA4HTML(rec, logoURL)
	case ThermalModern:
		return renderThermalHTML(rec, logoURL, ThermalModern)
	default:
		return renderThermalHTML(rec, logoURL, ThermalClassic)
	}
}

// RenderPDF renders the receipt PDF with its resolved layout.
func RenderPDF(rec Receipt, brand Brand) ([]byte, error) {
	switch Resolve(rec.Layout, rec.UseCase) {
	case A4Invoice:
		return renderA4PDF(rec, brand)
	case ThermalModern:
		return renderThermalPDF(rec, brand, ThermalModern)
	default:
		return renderThermalPDF(rec, brand, ThermalClassic)
	}
}

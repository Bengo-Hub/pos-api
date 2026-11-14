package orders

// PaymentStatusOwed is the All-Sales filter for every sale with money still owed: due, partial
// and overdue together. The dashboard's Sales due card opens All-Sales with it.
const PaymentStatusOwed = "owed"

// PaymentStatusesFor lists the derived payment statuses a filter value covers: "owed" covers
// due, partial and overdue, any other value only itself.
func PaymentStatusesFor(filter string) []string {
	if filter == PaymentStatusOwed {
		return []string{"due", "partial", "overdue"}
	}
	return []string{filter}
}

// PaymentStatusMatches reports whether a sale's derived payment status passes the filter. An
// empty filter passes everything.
func PaymentStatusMatches(filter, status string) bool {
	if filter == "" {
		return true
	}
	for _, s := range PaymentStatusesFor(filter) {
		if s == status {
			return true
		}
	}
	return false
}

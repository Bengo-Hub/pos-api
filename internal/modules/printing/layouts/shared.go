package layouts

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// money renders "KSh 16,200" / "KSh -1,000" style amounts: thousands separators,
// decimals only when the amount isn't whole.
func money(currency string, v float64) string {
	unit := currency
	if unit == "" || strings.EqualFold(unit, "KES") {
		unit = "KSh"
	}
	return unit + " " + amount(v)
}

func amount(v float64) string {
	neg := v < 0
	av := math.Abs(v)
	var s string
	if av == math.Trunc(av) {
		s = groupThousands(fmt.Sprintf("%.0f", av))
	} else {
		s = groupThousands(fmt.Sprintf("%.2f", av))
	}
	if neg {
		return "-" + s
	}
	return s
}

// groupThousands inserts comma separators into the integer part of a plain numeric string.
func groupThousands(s string) string {
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	if len(intPart) <= 3 {
		return intPart + frac
	}
	var b strings.Builder
	lead := len(intPart) % 3
	if lead > 0 {
		b.WriteString(intPart[:lead])
	}
	for i := lead; i < len(intPart); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(intPart[i : i+3])
	}
	return b.String() + frac
}

// timeIn converts a stored-UTC timestamp into the outlet's local timezone. Without this the
// printed time is offset by the UTC delta (the "wrong time, correct date" bug). Falls back
// to Africa/Nairobi, then UTC.
func timeIn(t time.Time, tz string) time.Time {
	if tz == "" {
		tz = "Africa/Nairobi"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc)
}

// shortDate renders dd-mm-yyyy (the ETR receipt date style) in the outlet timezone.
func shortDate(t time.Time, tz string) string { return timeIn(t, tz).Format("02-01-2006") }

func shortDateTime(t time.Time, tz string) string { return timeIn(t, tz).Format("02-01-2006 15:04") }

// receiptTime renders the receipt header timestamp ("02 Jan 2006  15:04") in outlet time.
func receiptTime(t time.Time, tz string) string { return timeIn(t, tz).Format("02 Jan 2006  15:04") }

// chargeRows returns the named charge breakdown as sorted display rows
// ("Shipping(+)", 200), falling back to one aggregate "Charges(+)" row.
func chargeRows(rec Receipt) [][2]interface{} {
	var rows [][2]interface{}
	if len(rec.Charges) > 0 {
		keys := make([]string, 0, len(rec.Charges))
		for k := range rec.Charges {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			label := strings.ToUpper(k[:1]) + k[1:]
			rows = append(rows, [2]interface{}{label + "(+)", rec.Charges[k]})
		}
		return rows
	}
	if rec.ChargesTotal > 0 {
		rows = append(rows, [2]interface{}{"Charges(+)", rec.ChargesTotal})
	}
	return rows
}

// paymentMethodLabel renders "Cash (14-07-2026)" — the tender name plus the settle date.
func paymentMethodLabel(rec Receipt) string {
	m := strings.TrimSpace(strings.ReplaceAll(rec.PaymentMethod, "_", " "))
	if m == "" {
		return ""
	}
	m = strings.ToUpper(m[:1]) + m[1:]
	if rec.PaymentDate != nil {
		return fmt.Sprintf("%s (%s)", m, shortDate(*rec.PaymentDate, rec.Timezone))
	}
	return m
}

// truncate clamps a string to n bytes with an ellipsis (PDF cell overflow guard).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// taxLabel is the shared "VAT (16%)" / "Tax" label used by every layout.
func taxLabel(rec Receipt) string {
	if rec.VatRate > 0 {
		return fmt.Sprintf("VAT (%g%%)", rec.VatRate)
	}
	return "Tax"
}

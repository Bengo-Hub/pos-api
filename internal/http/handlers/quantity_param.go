package handlers

import "strconv"

// parseQuantityParam parses a ?quantity= query value into a positive float64, defaulting to 1
// for an empty, invalid, or non-positive input. float64 (not int) because a fractional
// quantity is the normal case for continuous-unit items sold by the ml/kg (e.g. a perfume
// refill) — strconv.Atoi would error on "1.5" and silently fall back to 1, resolving tier
// pricing against the wrong quantity. Mirrors inventory-api's identically-named helper.
func parseQuantityParam(qStr string) float64 {
	if qStr == "" {
		return 1
	}
	q, err := strconv.ParseFloat(qStr, 64)
	if err != nil || q <= 0 {
		return 1
	}
	return q
}

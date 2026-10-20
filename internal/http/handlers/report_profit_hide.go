package handlers

import "github.com/bengobox/pos-service/internal/modules/docs"

// hideProfitColumns removes the last n columns (the profit and margin ones) from a table section,
// along with each row's and the total row's matching cells. The documents that mix profit into an
// otherwise legitimate cashier report call it when outletmw.ProfitHidden(ctx) is true.
func hideProfitColumns(sec *docs.Section, n int) {
	if n <= 0 || len(sec.Columns) < n {
		return
	}
	sec.Columns = sec.Columns[:len(sec.Columns)-n]
	for i := range sec.Rows {
		if len(sec.Rows[i]) >= n {
			sec.Rows[i] = sec.Rows[i][:len(sec.Rows[i])-n]
		}
	}
	if len(sec.Total) >= n {
		sec.Total = sec.Total[:len(sec.Total)-n]
	}
}

// withoutProfitCards drops the "Gross Profit" summary card.
func withoutProfitCards(cards []docs.Card) []docs.Card {
	out := make([]docs.Card, 0, len(cards))
	for _, c := range cards {
		if c.Label != "Gross Profit" {
			out = append(out, c)
		}
	}
	return out
}

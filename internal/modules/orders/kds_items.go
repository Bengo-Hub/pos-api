package orders

import (
	"fmt"
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
)

// kdsTicketItem renders one order line as a KDS ticket / kitchen chit item. Besides the SKU,
// name and quantity the station needs what makes this plate different from the menu default: the
// selected modifiers ("Oat milk", "No onions") and any free-text notes. Modifiers come from the
// persisted POSLineModifier rows when the caller loaded them, otherwise from the raw selection in
// line metadata (POS terminal wire format, or the snapshot an online order carries). "qty" is
// kept alongside "quantity" for clients that read the short key.
func kdsTicketItem(l *ent.POSOrderLine) map[string]any {
	item := map[string]any{
		"line_id":  l.ID.String(),
		"sku":      l.Sku,
		"name":     l.Name,
		"quantity": l.Quantity,
		"qty":      l.Quantity,
	}
	if mods := lineModifierLabels(l); len(mods) > 0 {
		item["modifiers"] = mods
	}
	if notes := lineNotes(l); notes != "" {
		item["notes"] = notes
	}
	return item
}

// lineModifierLabels returns human-readable modifier labels for a line.
func lineModifierLabels(l *ent.POSOrderLine) []string {
	if l == nil {
		return nil
	}
	if len(l.Edges.Modifiers) > 0 {
		out := make([]string, 0, len(l.Edges.Modifiers))
		for _, m := range l.Edges.Modifiers {
			if n := strings.TrimSpace(m.Name); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	raw, ok := l.Metadata["modifiers"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		switch v := r.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				out = append(out, s)
			}
		case map[string]any:
			label := firstString(v, "option_name", "name", "optionName")
			if label == "" {
				continue
			}
			if group := firstString(v, "group_name", "groupName"); group != "" && !strings.EqualFold(group, label) {
				label = fmt.Sprintf("%s: %s", group, label)
			}
			out = append(out, label)
		}
	}
	return out
}

// lineNotes returns the free-text instruction attached to a line, if any.
func lineNotes(l *ent.POSOrderLine) string {
	if l == nil || l.Metadata == nil {
		return ""
	}
	return strings.TrimSpace(firstString(l.Metadata, "notes", "note", "kitchen_note", "special_instructions"))
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

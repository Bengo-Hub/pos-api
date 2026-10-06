// Package orderchannel classifies a POS order by how it reaches the customer. It is the one place
// that decides whether an order came from the online store and whether it is eaten in, collected or
// delivered, so the KDS board, the kitchen chit banner and the online-orders queue always agree.
// It depends only on the order's subtype and metadata, never on a database lookup, so it can be
// used while the order is still being created (before its OrderLink row exists).
package orderchannel

import (
	"fmt"
	"strings"
)

// Channel is the customer-facing route of an order.
type Channel string

const (
	DineIn         Channel = "dine_in"
	Takeaway       Channel = "takeaway"
	Delivery       Channel = "delivery"
	RoomService    Channel = "room_service"
	BarTab         Channel = "bar_tab"
	Retail         Channel = "retail"
	ServiceJob     Channel = "service_job"
	OnlinePickup   Channel = "online_pickup"
	OnlineDelivery Channel = "online_delivery"
)

// Source values reported to clients: where the order was taken.
const (
	SourcePOS    = "pos"
	SourceOnline = "online"
)

// IsOnline reports whether the order was placed on the online store. The ordering consumer stamps
// metadata.online_order_id on every order it ingests, at creation time.
func IsOnline(meta map[string]any) bool {
	id, _ := meta["online_order_id"].(string)
	return strings.TrimSpace(id) != ""
}

// Source returns SourceOnline or SourcePOS for the order metadata.
func Source(meta map[string]any) string {
	if IsOnline(meta) {
		return SourceOnline
	}
	return SourcePOS
}

// Of returns the channel of an order from its subtype and metadata. An online order is split into
// pickup and delivery by its fulfilment type (falling back to the delivery subtype); every other
// order keeps its POS subtype, with an empty subtype treated as dine-in like the schema default.
func Of(subtype string, meta map[string]any) Channel {
	if IsOnline(meta) {
		if isDelivery(subtype, meta) {
			return OnlineDelivery
		}
		return OnlinePickup
	}
	switch Channel(subtype) {
	case Takeaway, Delivery, RoomService, BarTab, Retail, ServiceJob:
		return Channel(subtype)
	}
	return DineIn
}

// IsCounterHandover reports whether the order leaves through the counter or a rider rather than
// being served at a table or room: takeaway, delivery and both online channels.
func (c Channel) IsCounterHandover() bool {
	switch c {
	case Takeaway, Delivery, OnlinePickup, OnlineDelivery:
		return true
	}
	return false
}

// OnlineLabel is the short line a kitchen reads on an online order ("Online pickup", "Online
// delivery for Fri 18:30"). Empty for orders that did not come from the online store.
func OnlineLabel(subtype string, meta map[string]any) string {
	if !IsOnline(meta) {
		return ""
	}
	label := "Online pickup"
	if isDelivery(subtype, meta) {
		label = "Online delivery"
	}
	if at, _ := meta["scheduled_for_label"].(string); strings.TrimSpace(at) != "" {
		label = fmt.Sprintf("%s for %s", label, strings.TrimSpace(at))
	}
	return label
}

func isDelivery(subtype string, meta map[string]any) bool {
	if ft, _ := meta["fulfillment_type"].(string); ft != "" {
		return ft == "delivery"
	}
	return subtype == string(Delivery)
}

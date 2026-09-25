package orders

import "testing"

func TestCollectionCode(t *testing.T) {
	meta := onlineOrderMetadata(map[string]interface{}{
		"order_number":    "ORD-1",
		"payment_status":  "paid",
		"collection_code": "482913",
	}, "0b4f5f9e-9b53-4b4e-9d5a-1c2f0e7a1111", "online_ordering", "takeaway", "pickup", nil)

	if _, leaked := meta["collection_code"]; leaked {
		t.Fatalf("the plain code must never be stored on the POS order")
	}
	if !CollectionCodeRequired(meta) {
		t.Fatalf("pickup order with a code must require it")
	}
	if !CollectionCodeMatches(meta, "482913") || !CollectionCodeMatches(meta, " 482-913 ") {
		t.Fatalf("the customer's code (with spaces or dashes) must match")
	}
	if CollectionCodeMatches(meta, "482914") || CollectionCodeMatches(meta, "") {
		t.Fatalf("a wrong or empty code must not match")
	}

	other := onlineOrderMetadata(map[string]interface{}{"collection_code": "482913"},
		"7c1d2e3f-0000-4000-8000-000000000002", "online_ordering", "takeaway", "pickup", nil)
	if other[MetaCollectionCodeHash] == meta[MetaCollectionCodeHash] {
		t.Fatalf("the same code on two orders must hash differently")
	}

	delivery := onlineOrderMetadata(map[string]interface{}{"payment_status": "paid"},
		"7c1d2e3f-0000-4000-8000-000000000003", "online_ordering", "delivery", "delivery", nil)
	if CollectionCodeRequired(delivery) {
		t.Fatalf("orders without a collection code must not require one")
	}
}

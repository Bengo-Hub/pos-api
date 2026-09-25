package orders

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// MetaCollectionCodeHash is the POS order metadata key holding the hashed pickup collection code
// (ordering generates the 6-digit code and shows it to the customer).
const MetaCollectionCodeHash = "collection_code_hash"

// HashCollectionCode hashes a collection code salted with the online order id, so the same code
// on two orders never looks the same.
func HashCollectionCode(onlineOrderID, code string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(onlineOrderID) + ":" + normalizeCollectionCode(code)))
	return hex.EncodeToString(sum[:])
}

// CollectionCodeRequired reports whether the order carries a collection code to check.
func CollectionCodeRequired(meta map[string]any) bool {
	hash, _ := meta[MetaCollectionCodeHash].(string)
	return hash != ""
}

// CollectionCodeMatches compares the code the customer showed against the stored hash.
func CollectionCodeMatches(meta map[string]any, code string) bool {
	hash, _ := meta[MetaCollectionCodeHash].(string)
	orderID, _ := meta["online_order_id"].(string)
	if hash == "" || orderID == "" || normalizeCollectionCode(code) == "" {
		return false
	}
	got := HashCollectionCode(orderID, code)
	return subtle.ConstantTimeCompare([]byte(got), []byte(hash)) == 1
}

// normalizeCollectionCode drops the spaces and dashes people type when reading a code out.
func normalizeCollectionCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, strings.TrimSpace(code))
}

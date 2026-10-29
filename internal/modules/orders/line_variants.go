package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/inventory"
)

// LineVariantKey is the order line metadata key holding the variant snapshot
// ({id, sku, name, attributes, parent_id, parent_sku, parent_name}). It is written only by the
// server: a client-sent value is dropped before stamping.
const LineVariantKey = "variant"

const (
	variantCacheTTL = 10 * time.Minute
	// variantLookupTimeout bounds the S2S lookup on the sale path. The stamp is a display and
	// reporting snapshot, never a reason to slow or fail a sale.
	variantLookupTimeout = 3 * time.Second
)

// VariantSnapshot is the metadata.variant value for a variant child (also returned by the
// barcode lookup so the till shows the same thing the server stamps).
func VariantSnapshot(v inventory.VariantChild) map[string]any {
	snap := map[string]any{
		"id":   v.ID,
		"sku":  v.SKU,
		"name": v.Name,
	}
	if len(v.Attributes) > 0 {
		snap["attributes"] = v.Attributes
	}
	if v.ParentID != "" {
		snap["parent_id"] = v.ParentID
	}
	if v.ParentSKU != "" {
		snap["parent_sku"] = v.ParentSKU
	}
	if v.ParentName != "" {
		snap["parent_name"] = v.ParentName
	}
	return snap
}

// variantChildLookup is the inventory S2S call the resolver wraps (an interface so tests can
// stub it without an HTTP server).
type variantChildLookup interface {
	LookupVariantChildren(ctx context.Context, tenantID string, skus []string) (map[string]inventory.VariantChild, error)
}

// VariantResolver tells which order line SKUs are variant children, caching each answer
// (including "not a variant") in Redis for 10 minutes so a till selling the same items
// repeatedly makes no S2S call at all. Cache key: pos:variant:{tenantID}:{sku}.
type VariantResolver struct {
	inv   variantChildLookup
	redis *redis.Client
	log   *zap.Logger
}

// NewVariantResolver builds a resolver. redisClient may be nil (every lookup goes to inventory).
func NewVariantResolver(inv variantChildLookup, redisClient *redis.Client, log *zap.Logger) *VariantResolver {
	return &VariantResolver{inv: inv, redis: redisClient, log: log.Named("variant.resolver")}
}

func variantCacheKey(tenantID uuid.UUID, sku string) string {
	return fmt.Sprintf("pos:variant:%s:%s", tenantID, sku)
}

// Resolve returns the variant children among skus, keyed by SKU.
func (r *VariantResolver) Resolve(ctx context.Context, tenantID uuid.UUID, skus []string) (map[string]inventory.VariantChild, error) {
	out := map[string]inventory.VariantChild{}
	if r == nil || r.inv == nil || len(skus) == 0 {
		return out, nil
	}
	missing := make([]string, 0, len(skus))
	for _, sku := range skus {
		if r.redis == nil {
			missing = append(missing, sku)
			continue
		}
		raw, err := r.redis.Get(ctx, variantCacheKey(tenantID, sku)).Bytes()
		if err != nil {
			missing = append(missing, sku)
			continue
		}
		var v inventory.VariantChild
		if json.Unmarshal(raw, &v) != nil {
			missing = append(missing, sku)
			continue
		}
		if v.ID != "" {
			out[sku] = v
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	found, err := r.inv.LookupVariantChildren(ctx, tenantID.String(), missing)
	if err != nil {
		return out, err
	}
	for _, sku := range missing {
		v, ok := found[sku]
		if ok {
			out[sku] = v
		}
		if r.redis != nil {
			// A miss caches as an empty object so ordinary items are not looked up again.
			if b, mErr := json.Marshal(v); mErr == nil {
				r.redis.Set(ctx, variantCacheKey(tenantID, sku), b, variantCacheTTL)
			}
		}
	}
	return out, nil
}

// SetVariantResolver wires the variant lookup used to stamp metadata.variant on order lines.
// Nil leaves lines unstamped (client-sent values are still dropped).
func (s *Service) SetVariantResolver(r *VariantResolver) { s.variantResolver = r }

// StampLineVariants drops any client-sent metadata.variant and stamps the server's snapshot on
// every line whose SKU is a variant child. A line with no catalog item id gets the child id.
// Best effort: when inventory is slow or down the sale goes through unstamped. Runs before the
// order transaction opens so no S2S call ever holds a DB transaction.
func (s *Service) StampLineVariants(ctx context.Context, tenantID uuid.UUID, lines []OrderLineInput) {
	skus := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for i := range lines {
		if lines[i].Metadata != nil {
			delete(lines[i].Metadata, LineVariantKey)
		}
		sku := lines[i].SKU
		if sku == "" {
			continue
		}
		if _, dup := seen[sku]; dup {
			continue
		}
		seen[sku] = struct{}{}
		skus = append(skus, sku)
	}
	if s.variantResolver == nil || len(skus) == 0 {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, variantLookupTimeout)
	defer cancel()
	found, err := s.variantResolver.Resolve(lookupCtx, tenantID, skus)
	if err != nil {
		s.log.Warn("orders: variant lookup failed, lines left unstamped", zap.Error(err))
	}
	for i := range lines {
		v, ok := found[lines[i].SKU]
		if !ok {
			continue
		}
		if lines[i].Metadata == nil {
			lines[i].Metadata = map[string]any{}
		}
		lines[i].Metadata[LineVariantKey] = VariantSnapshot(v)
		if lines[i].CatalogItemID == uuid.Nil {
			if id, perr := uuid.Parse(v.ID); perr == nil {
				lines[i].CatalogItemID = id
			}
		}
	}
}

package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/treasury"
)

const taxCodeCacheTTL = 10 * time.Minute

// TaxCodeInfo holds the resolved tax rate and KRA code for a single tax code.
type TaxCodeInfo struct {
	Code      string  `json:"code"`
	Rate      float64 `json:"rate"`      // e.g. 16.0
	KRACode   string  `json:"kra_code"`  // KRA TaxTyCd: A=16%VAT, B=8%VAT, D=exempt, E=zero
	TaxType   string  `json:"tax_type"`  // "vat", "excise", etc.
}

// TaxResolver fetches TaxCode definitions from treasury-api with Redis caching.
// Cache key pattern: pos:tax:{tenantSlug}:{taxCodeID}
// TTL: 10 minutes — tax rates rarely change; short enough to propagate updates.
type TaxResolver struct {
	treasury *treasury.Client
	redis    *redis.Client
	log      *zap.Logger
}

// NewTaxResolver creates a TaxResolver. redis may be nil (disables caching, always fetches).
func NewTaxResolver(treasuryClient *treasury.Client, redisClient *redis.Client, log *zap.Logger) *TaxResolver {
	return &TaxResolver{
		treasury: treasuryClient,
		redis:    redisClient,
		log:      log.Named("tax.resolver"),
	}
}

// Resolve returns the TaxCodeInfo for a given code string (e.g. "VAT-16").
// Returns nil, nil when the code is not found in treasury — callers should treat as tax-exempt.
// VAT is suppressed (rate→0, KRA code E=zero-rated) when the business is not VAT-active
// (not VAT-registered) — a non-registered business must not charge VAT to its customers.
func (r *TaxResolver) Resolve(ctx context.Context, tenantSlug, taxCodeID string) (*TaxCodeInfo, error) {
	if r.treasury == nil || taxCodeID == "" {
		return nil, nil
	}

	info, err := r.resolveCode(ctx, tenantSlug, taxCodeID)
	if err != nil || info == nil {
		return info, err
	}
	// Gate VAT on the tenant's registration status. Non-VAT taxes (excise etc.) pass through.
	if info.TaxType == "vat" && info.Rate > 0 && !r.vatActive(ctx, tenantSlug) {
		gated := *info
		gated.Rate = 0
		gated.KRACode = "E" // zero-rated
		return &gated, nil
	}
	return info, nil
}

// resolveCode fetches/caches a single TaxCode definition (rate/KRA code) from treasury.
func (r *TaxResolver) resolveCode(ctx context.Context, tenantSlug, taxCodeID string) (*TaxCodeInfo, error) {
	cacheKey := fmt.Sprintf("pos:tax:%s:%s", tenantSlug, taxCodeID)

	// Check Redis cache first
	if r.redis != nil {
		if cached, err := r.redis.Get(ctx, cacheKey).Bytes(); err == nil {
			var info TaxCodeInfo
			if json.Unmarshal(cached, &info) == nil {
				return &info, nil
			}
		}
	}

	// Fetch from treasury S2S
	tc, err := r.treasury.GetTaxCode(ctx, tenantSlug, taxCodeID)
	if err != nil {
		r.log.Warn("tax resolver: failed to fetch tax code from treasury",
			zap.String("tenant", tenantSlug),
			zap.String("code", taxCodeID),
			zap.Error(err),
		)
		return nil, nil // Non-fatal: fall back to no tax
	}
	if tc == nil {
		return nil, nil // Not configured — treat as no tax
	}

	info := &TaxCodeInfo{
		Code:    tc.Code,
		Rate:    float64(tc.Rate),
		KRACode: tc.KRACode,
		TaxType: tc.TaxType,
	}

	// Cache the result
	if r.redis != nil {
		if b, merr := json.Marshal(info); merr == nil {
			_ = r.redis.Set(ctx, cacheKey, b, taxCodeCacheTTL).Err()
		}
	}

	return info, nil
}

// vatActive reports whether the tenant should charge VAT (defaults TRUE on any error so we
// never silently stop a registered business from charging). Cached briefly in Redis.
func (r *TaxResolver) vatActive(ctx context.Context, tenantSlug string) bool {
	cacheKey := fmt.Sprintf("pos:vatactive:%s", tenantSlug)
	if r.redis != nil {
		if v, err := r.redis.Get(ctx, cacheKey).Result(); err == nil {
			return v == "1"
		}
	}
	profile, err := r.treasury.GetTaxProfile(ctx, tenantSlug)
	if err != nil || profile == nil {
		return true // permissive fallback — charge VAT per existing config
	}
	if r.redis != nil {
		val := "0"
		if profile.VATActive {
			val = "1"
		}
		_ = r.redis.Set(ctx, cacheKey, val, taxCodeCacheTTL).Err()
	}
	return profile.VATActive
}

// InvalidateTenant deletes all cached tax data for a single tenant slug: every
// per-code rate (pos:tax:{slug}:*) plus the VAT-active switch (pos:vatactive:{slug}).
// Safe to call when Redis is not configured (no-op).
func (r *TaxResolver) InvalidateTenant(ctx context.Context, tenantSlug string) {
	if r.redis == nil || tenantSlug == "" {
		return
	}
	r.deleteByPattern(ctx, fmt.Sprintf("pos:tax:%s:*", tenantSlug))
	if err := r.redis.Del(ctx, fmt.Sprintf("pos:vatactive:%s", tenantSlug)).Err(); err != nil {
		r.log.Debug("tax resolver: failed to delete vatactive key", zap.String("tenant", tenantSlug), zap.Error(err))
	}
	r.log.Info("tax resolver: invalidated cached tax for tenant", zap.String("tenant", tenantSlug))
}

// InvalidateCode invalidates the cached rate for a single tax code under one tenant
// (pos:tax:{slug}:{code}) and the tenant VAT-active switch (a VAT-code change may flip
// the effective rate). Safe to call when Redis is not configured (no-op).
func (r *TaxResolver) InvalidateCode(ctx context.Context, tenantSlug, code string) {
	if r.redis == nil || tenantSlug == "" {
		return
	}
	if code != "" {
		key := fmt.Sprintf("pos:tax:%s:%s", tenantSlug, code)
		if err := r.redis.Del(ctx, key).Err(); err != nil {
			r.log.Debug("tax resolver: failed to delete tax key", zap.String("key", key), zap.Error(err))
		}
	}
	if err := r.redis.Del(ctx, fmt.Sprintf("pos:vatactive:%s", tenantSlug)).Err(); err != nil {
		r.log.Debug("tax resolver: failed to delete vatactive key", zap.String("tenant", tenantSlug), zap.Error(err))
	}
	r.log.Info("tax resolver: invalidated cached tax code",
		zap.String("tenant", tenantSlug), zap.String("code", code))
}

// InvalidateCodeAllTenants is the fallback path when the event's tenant UUID cannot be
// mapped to a slug. It SCANs for pos:tax:*:{code} across every tenant and deletes the
// matches, then flushes all per-tenant VAT-active switches (pos:vatactive:*) since a
// code change may affect the effective VAT rate. Safe when Redis is nil (no-op).
func (r *TaxResolver) InvalidateCodeAllTenants(ctx context.Context, code string) {
	if r.redis == nil || code == "" {
		return
	}
	r.deleteByPattern(ctx, fmt.Sprintf("pos:tax:*:%s", code))
	r.deleteByPattern(ctx, "pos:vatactive:*")
	r.log.Info("tax resolver: invalidated cached tax code across all tenants (no slug resolved)",
		zap.String("code", code))
}

// deleteByPattern SCANs Redis for keys matching pattern and deletes them in batches.
func (r *TaxResolver) deleteByPattern(ctx context.Context, pattern string) {
	if r.redis == nil {
		return
	}
	var cursor uint64
	for {
		keys, next, err := r.redis.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			r.log.Debug("tax resolver: SCAN failed", zap.String("pattern", pattern), zap.Error(err))
			return
		}
		if len(keys) > 0 {
			if derr := r.redis.Del(ctx, keys...).Err(); derr != nil {
				r.log.Debug("tax resolver: DEL failed", zap.String("pattern", pattern), zap.Error(derr))
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

// ComputeLineTax calculates the tax_amount for a single order line.
// If priceIncludesTax=true (inclusive): tax is back-calculated from the total.
// If priceIncludesTax=false (exclusive): tax is added on top of the total.
// Returns (taxAmount, baseAmount, ok).
func ComputeLineTax(lineTotal float64, taxRate float64, priceIncludesTax bool) (taxAmount, baseAmount float64) {
	if taxRate <= 0 || lineTotal <= 0 {
		return 0, lineTotal
	}
	rate := taxRate / 100.0
	if priceIncludesTax {
		// Inclusive: tax = total - (total / (1 + rate))
		divisor := 1.0 + rate
		base := lineTotal / divisor
		tax := lineTotal - base
		return roundTo2(tax), roundTo2(base)
	}
	// Exclusive: tax = total * rate
	tax := lineTotal * rate
	return roundTo2(tax), roundTo2(lineTotal)
}

func roundTo2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100.0
}

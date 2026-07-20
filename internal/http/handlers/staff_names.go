package handlers

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entuser "github.com/bengobox/pos-service/internal/ent/user"
)

// resolveStaffNames maps POS order user_ids to human staff names. Order.user_id is the auth
// service user id (JWT subject); the local User projection carries full_name keyed by BOTH
// its own id and auth_service_user_id, so match on either. Returns id → name (best effort;
// missing ids are simply absent from the map).
//
// This is THE shared implementation — the reports handlers, the PDF/export builders and the
// order-list enrichment all resolve cashier display names through here so the mapping can
// never drift between surfaces (it used to be duplicated on ReportsHandler and
// ReportPDFHandler).
func resolveStaffNames(ctx context.Context, db *ent.Client, log *zap.Logger, tid uuid.UUID, ids []uuid.UUID) map[uuid.UUID]string {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	users, err := db.User.Query().
		Where(
			entuser.TenantID(tid),
			entuser.Or(entuser.IDIn(ids...), entuser.AuthServiceUserIDIn(ids...)),
		).
		All(ctx)
	if err != nil {
		log.Warn("resolve staff names failed", zap.Error(err))
		return out
	}
	for _, u := range users {
		name := strings.TrimSpace(u.FullName)
		if name == "" {
			name = strings.TrimSpace(u.Email)
		}
		if name == "" {
			continue
		}
		out[u.ID] = name
		out[u.AuthServiceUserID] = name
	}
	return out
}

package audit

import (
	"context"

	entgo "entgo.io/ent"
	"github.com/google/uuid"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/platform/clientinfo"
)

// Metadata keys the client trail writes. Readers (reports, the Loss Prevention page, the order
// detail view) use the same keys.
const (
	// KeyClient holds clientinfo.Info.Map() for the request that created the row: on an order
	// metadata, a payment's payment_data and an audit entry's metadata.
	KeyClient = "client"
	// KeyCollectedBy is the user id of the logged-in staff member who recorded a payment. It is
	// what a shift's expected cash is attributed by (shifts.Summarize).
	KeyCollectedBy = "collected_by"
	// KeyOpenedFrom and KeyClosedFrom hold the client info of the requests that opened and
	// closed a shift session.
	KeyOpenedFrom = "opened_from"
	KeyClosedFrom = "closed_from"
)

// TrailHook stamps the request's client info (clientinfo.FromContext) onto every order,
// payment, shift session and audit entry at create time, whichever code path writes it. It is
// registered once on the ent client (app.New), so no handler can forget it. Rows written by
// background work with no originating request are left untouched.
//
// The server-side value always overwrites any client-supplied "client" key, so a request body
// can't forge where it came from.
func TrailHook() ent.Hook {
	return func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if !m.Op().Is(entgo.OpCreate) {
				return next.Mutate(ctx, m)
			}
			info, ok := clientinfo.FromContext(ctx)
			if !ok {
				return next.Mutate(ctx, m)
			}
			trail := info.Map()
			switch mu := m.(type) {
			case *ent.POSOrderMutation:
				meta, _ := mu.Metadata()
				meta = cloneMap(meta)
				meta[KeyClient] = trail
				mu.SetMetadata(meta)
			case *ent.POSPaymentMutation:
				data, _ := mu.PaymentData()
				data = cloneMap(data)
				data[KeyClient] = trail
				if actor := actorID(ctx); actor != uuid.Nil {
					if _, set := data[KeyCollectedBy]; !set {
						data[KeyCollectedBy] = actor.String()
					}
				}
				mu.SetPaymentData(data)
			case *ent.POSDeviceSessionMutation:
				meta, _ := mu.Metadata()
				meta = cloneMap(meta)
				meta[KeyOpenedFrom] = trail
				mu.SetMetadata(meta)
			case *ent.AuditLogMutation:
				meta, _ := mu.Metadata()
				meta = cloneMap(meta)
				meta[KeyClient] = trail
				mu.SetMetadata(meta)
			}
			return next.Mutate(ctx, m)
		})
	}
}

type collectorKey struct{}

// WithCollector overrides who a payment created under ctx is recorded as collected by. Only the
// offline sync uses it, after checking the person is the verified creator of the offline order.
func WithCollector(ctx context.Context, userID uuid.UUID) context.Context {
	return context.WithValue(ctx, collectorKey{}, userID)
}

// actorID is who a created row is attributed to: a WithCollector override, else the logged-in
// user behind the request, else uuid.Nil.
func actorID(ctx context.Context) uuid.UUID {
	if id, ok := ctx.Value(collectorKey{}).(uuid.UUID); ok && id != uuid.Nil {
		return id
	}
	claims, ok := authclient.ClaimsFromContext(ctx)
	if !ok || claims.Subject == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// cloneMap copies m so the hook never mutates a map the caller still holds.
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

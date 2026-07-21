package payments

import (
	"context"
	"encoding/json"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/customerbalancecache"
)

// balanceUpdatedEvent is the wire shape of treasury.customer.balance_updated /
// treasury.vendor.balance_updated (shared-events envelope: business fields under "payload").
type balanceUpdatedEvent struct {
	TenantID string         `json:"tenant_id"`
	Payload  map[string]any `json:"payload"`
}

// subscribeCustomerBalanceUpdated keeps CustomerBalanceCache fresh — a self-healing FALLBACK
// for the GetCredit S2S proxy (clients_credit.go), used only when the live call to treasury
// fails. Closes the one-way sync gap: a payment/refund/credit action recorded directly in
// treasury-ui (RecordARPayment, ApplyCustomerCredit, ProcessRefund) previously never reached
// POS at all — the terminal's credit hint and any offline read would just be stale forever
// instead of self-healing once the event lands. Durable + idempotent: the cache write is a pure
// upsert (last-write-wins on synced_at), safe to redeliver.
func (s *TreasurySubscriber) subscribeCustomerBalanceUpdated(js nats.JetStreamContext) error {
	sharedevents.SubscribeQueueWithRebind(s.log, js, "treasury", "treasury.customer.balance_updated", "pos-treasury-customer-balance-updated", func(msg *nats.Msg) {
		defer func() { _ = msg.Ack() }()

		var evt balanceUpdatedEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			s.log.Error("treasury.customer.balance_updated: unmarshal", zap.Error(err))
			return
		}
		tenantID, err := uuid.Parse(evt.TenantID)
		if err != nil {
			s.log.Warn("treasury.customer.balance_updated: invalid tenant id", zap.String("tenant_id", evt.TenantID))
			return
		}

		crmContactStr, _ := evt.Payload["crm_contact_id"].(string)
		identifier, _ := evt.Payload["customer_identifier"].(string)
		var crmContactID *uuid.UUID
		if crmContactStr != "" {
			if id, perr := uuid.Parse(crmContactStr); perr == nil {
				crmContactID = &id
			}
		}
		if crmContactID == nil && identifier == "" {
			return // can't key the cache row — nothing to reconcile against
		}

		name, _ := evt.Payload["customer_name"].(string)
		balanceDue, _ := evt.Payload["balance_due"].(string)
		outstandingDebit, _ := evt.Payload["outstanding_debit"].(string)
		storeCreditBalance, _ := evt.Payload["store_credit_balance"].(string)
		currency, _ := evt.Payload["currency"].(string)

		ctx := context.Background()
		q := s.client.CustomerBalanceCache.Query().Where(customerbalancecache.TenantID(tenantID))
		if crmContactID != nil {
			q = q.Where(customerbalancecache.CrmContactID(*crmContactID))
		} else {
			q = q.Where(customerbalancecache.CustomerIdentifier(identifier))
		}
		existing, ferr := q.First(ctx)
		if ferr != nil && !ent.IsNotFound(ferr) {
			s.log.Error("treasury.customer.balance_updated: lookup cache row", zap.Error(ferr))
			return
		}
		if existing != nil {
			_, err = existing.Update().
				SetNillableCrmContactID(crmContactID).
				SetCustomerIdentifier(identifier).
				SetCustomerName(name).
				SetBalanceDue(balanceDue).
				SetOutstandingDebit(outstandingDebit).
				SetStoreCreditBalance(storeCreditBalance).
				SetCurrency(currencyOrDefault(currency)).
				Save(ctx)
		} else {
			create := s.client.CustomerBalanceCache.Create().
				SetTenantID(tenantID).
				SetCustomerIdentifier(identifier).
				SetCustomerName(name).
				SetBalanceDue(balanceDue).
				SetOutstandingDebit(outstandingDebit).
				SetStoreCreditBalance(storeCreditBalance).
				SetCurrency(currencyOrDefault(currency))
			if crmContactID != nil {
				create = create.SetCrmContactID(*crmContactID)
			}
			_, err = create.Save(ctx)
		}
		if err != nil {
			s.log.Error("treasury.customer.balance_updated: upsert cache row", zap.Error(err))
		}
	}, nats.Durable("pos-treasury-customer-balance-updated"), nats.ManualAck())
	return nil
}

func currencyOrDefault(c string) string {
	if c == "" {
		return "KES"
	}
	return c
}

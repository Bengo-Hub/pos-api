package events

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// crmContactMergeTables lists every pos-api table carrying its own crm_contact_id reference that
// needs repointing when marketflow folds a duplicate contact into a survivor. None of these carry
// a UNIQUE constraint on crm_contact_id (verified against prod), so a plain multi-row UPDATE is
// safe — unlike treasury's customer_balances, no merge arithmetic is needed here, just a repoint.
// customer_balance_caches is handled separately (cleanup, not repoint — see handle).
var crmContactMergeTables = []string{
	"client_records",
	"loyalty_accounts",
	"appointments",
	"event_bookings",
	"room_bookings",
	"room_guests",
}

// CRMContactMergedSubscriber consumes marketflow's crm.contact.merged (published when a
// duplicate CRM contact — created by the phone/email dedup gap since fixed in marketflow-api's
// contacts.Service.Create — is folded into a survivor) and repoints pos-api's OWN
// crm_contact_id-bearing rows accordingly. pos-api owns this data (feedback_service_data_
// ownership); treasury gets its own separate subscriber for customer_balances, which DOES need
// real merge arithmetic (a unique constraint there means two rows can collide) — pos-api's rows
// have no such constraint, so a repoint is the whole fix.
type CRMContactMergedSubscriber struct {
	db  *sql.DB
	log *zap.Logger
}

// NewCRMContactMergedSubscriber builds the subscriber. db must be the same *sql.DB backing the
// ent client so updates hit the live schema.
func NewCRMContactMergedSubscriber(db *sql.DB, log *zap.Logger) *CRMContactMergedSubscriber {
	return &CRMContactMergedSubscriber{db: db, log: log.Named("pos.crm_contact_merged")}
}

// Subscribe binds the durable JetStream consumer for "crm.contact.merged".
func (s *CRMContactMergedSubscriber) Subscribe(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("crm contact-merged subscriber: NATS connection is nil")
	}
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("crm contact-merged subscriber: jetstream init: %w", err)
	}

	// Ensure the "crm" stream exists — marketflow-api owns it, but this is the first event
	// marketflow has ever published, so unlike inventory/treasury streams it may not exist yet on
	// a fresh cluster (mirrors how StockSubscriber/TenantPurgeSubscriber ensure their streams).
	if _, err := js.StreamInfo("crm"); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name:      "crm",
			Subjects:  []string{"crm.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		}); addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("crm contact-merged subscriber: ensure crm stream: %w", addErr)
		}
	}

	sharedevents.SubscribeQueueWithRebind(s.log, js, "crm", "crm.contact.merged", "pos-contact-merged",
		s.handle,
		nats.Durable("pos-contact-merged"),
		nats.AckExplicit(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(5),
		nats.DeliverNew(),
	)

	s.log.Info("crm.contact.merged subscription registered")
	return nil
}

func (s *CRMContactMergedSubscriber) handle(msg *nats.Msg) {
	evt, err := sharedevents.FromJSON(msg.Data)
	if err != nil {
		s.log.Warn("crm.contact.merged: unmarshal failed — acking (cannot process)", zap.Error(err))
		_ = msg.Ack()
		return
	}

	tenantID := evt.TenantID
	survivorID, _ := uuid.Parse(strFromPayloadAny(evt.Payload["survivor_id"]))
	if tenantID == uuid.Nil || survivorID == uuid.Nil {
		s.log.Warn("crm.contact.merged: missing tenant_id/survivor_id — acking, skipping")
		_ = msg.Ack()
		return
	}
	rawIDs, _ := evt.Payload["merged_ids"].([]any)
	mergedIDs := make([]uuid.UUID, 0, len(rawIDs))
	for _, v := range rawIDs {
		if id, perr := uuid.Parse(strFromPayloadAny(v)); perr == nil {
			mergedIDs = append(mergedIDs, id)
		}
	}
	if len(mergedIDs) == 0 {
		s.log.Warn("crm.contact.merged: no valid merged_ids — acking, skipping")
		_ = msg.Ack()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.repoint(ctx, tenantID, survivorID, mergedIDs); err != nil {
		s.log.Error("crm.contact.merged: repoint failed — naking for retry", zap.Error(err))
		_ = msg.Nak()
		return
	}

	s.log.Info("crm.contact.merged: repointed pos-api records",
		zap.String("tenant_id", tenantID.String()),
		zap.String("survivor_id", survivorID.String()),
		zap.Int("merged_count", len(mergedIDs)))
	_ = msg.Ack()
}

// repoint runs every table's UPDATE (+ the customer_balance_caches cleanup) inside one
// transaction. Idempotent: re-running matches zero rows on a redelivery and still succeeds.
func (s *CRMContactMergedSubscriber) repoint(ctx context.Context, tenantID, survivorID uuid.UUID, mergedIDs []uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, table := range crmContactMergeTables {
		q := fmt.Sprintf(`UPDATE %q SET crm_contact_id = $1 WHERE tenant_id = $2 AND crm_contact_id = $3`, table)
		for _, dupID := range mergedIDs {
			if _, err := tx.ExecContext(ctx, q, survivorID, tenantID, dupID); err != nil {
				if isUndefinedTable(err) {
					s.log.Warn("crm.contact.merged: table absent in this env — skipping", zap.String("table", table))
					break
				}
				return fmt.Errorf("repoint %s: %w", table, err)
			}
		}
	}

	// customer_balance_caches is unique on (tenant_id, crm_contact_id) and is a pure CACHE of
	// treasury's customer_balances (refreshed via the existing treasury.customer.balance_updated
	// subscriber, which treasury's own contact-merge subscriber already re-publishes for the
	// survivor) — so the correct fix for a duplicate's stale cache row is deletion, not a repoint
	// that could collide with a cache row the survivor already has.
	for _, dupID := range mergedIDs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM customer_balance_caches WHERE tenant_id = $1 AND crm_contact_id = $2`,
			tenantID, dupID,
		); err != nil && !isUndefinedTable(err) {
			return fmt.Errorf("delete stale customer_balance_caches row: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

func strFromPayloadAny(v any) string {
	s, _ := v.(string)
	return s
}

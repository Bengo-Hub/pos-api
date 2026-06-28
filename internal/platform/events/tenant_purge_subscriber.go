package events

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// TenantPurgeSubscriber consumes the platform-owner-confirmed dormancy purge event
// (subject "tenant.purge") and IRREVERSIBLY deletes every row this service owns for
// the purged tenant.
//
// Subject derivation: subscriptions-api emits an outbox event with
// aggregate_type="tenant", event_type="purge". The shared-events outbox publisher
// derives the NATS subject as {aggregate_type}.{event_type} (see Event.Subject() in
// github.com/Bengo-Hub/shared-events), i.e. "tenant.purge". This matches the
// convention already relied on by other pos-api subscribers (e.g.
// "treasury.payment.succeeded", "inventory.stock.*").
//
// SAFETY: this handler performs hard, unrecoverable DELETEs. It runs ONLY when:
//   - tenant_id is present AND a valid, non-nil UUID, and
//   - payload.confirmed == true AND payload.reason == "dormancy".
//
// Any other condition Acks the message and does nothing (never a no-filter delete,
// never a poison-loop). A transient DB failure Naks for a bounded retry.
type TenantPurgeSubscriber struct {
	db  *sql.DB
	log *zap.Logger
}

// NewTenantPurgeSubscriber builds the subscriber. db must be the same *sql.DB used by
// the ent client so deletes hit the live schema.
func NewTenantPurgeSubscriber(db *sql.DB, log *zap.Logger) *TenantPurgeSubscriber {
	return &TenantPurgeSubscriber{
		db:  db,
		log: log.Named("pos.tenant_purge_subscriber"),
	}
}

// purgeStep describes one DELETE in the FK-safe purge order.
//
// kind=direct  → DELETE FROM <table> WHERE tenant_id = $1   (table owns tenant_id)
// kind=child   → DELETE FROM <table> WHERE <fk> IN (SELECT id FROM <parent> WHERE tenant_id = $1)
//
// Child steps are scoped through their parent's tenant_id so a child row is removed
// even on schemas where the child carries no tenant_id column. Steps are ordered so
// every child is deleted before its parent (all FKs are ON DELETE NO ACTION, so a
// parent delete fails while children exist — order matters).
type purgeStep struct {
	table  string
	kind   string // "direct" | "child"
	fkCol  string // child only
	parent string // child only
}

func direct(table string) purgeStep { return purgeStep{table: table, kind: "direct"} }
func child(table, fkCol, parent string) purgeStep {
	return purgeStep{table: table, kind: "child", fkCol: fkCol, parent: parent}
}

// purgePlan is the FK-safe deletion order for every pos-api-owned, tenant-scoped table.
// Children (and grandchildren) appear before their parents. GLOBAL/system catalogs
// (pos_permissions, pos_role_permissions, rate_limit_configs, feature_overrides,
// license_usage_snapshots) are intentionally excluded — they are not tenant-scoped.
// The tenant mirror row ("tenants") is deleted last.
//
// Idempotent: a redelivery simply deletes 0 rows everywhere and still Acks.
var purgePlan = []purgeStep{
	// ---- grandchildren / leaf children first ----
	child("pos_line_modifiers", "line_id", "pos_order_lines"), // via order lines (scoped below by order)
	child("pos_order_lines", "order_id", "pos_orders"),
	child("pos_order_events", "order_id", "pos_orders"),
	child("pos_payments", "order_id", "pos_orders"),
	child("pos_refunds", "order_id", "pos_orders"),
	child("order_links", "order_id", "pos_orders"),
	child("promotion_applications", "order_id", "pos_orders"),
	child("pos_return_lines", "return_id", "pos_returns"),
	child("prescription_lines", "prescription_id", "prescriptions"),
	child("gift_card_transactions", "gift_card_id", "gift_cards"),
	child("promotion_rules", "promotion_id", "promotions"),
	child("channel_sync_jobs", "integration_id", "channel_integrations"),
	child("modifiers", "modifier_group_id", "modifier_groups"),
	child("price_book_items", "price_book_id", "price_books"),
	child("staff_payroll_lines", "payroll_id", "staff_payrolls"),
	child("repair_job_events", "repair_job_id", "repair_jobs"),
	child("repair_job_parts", "repair_job_id", "repair_jobs"),
	child("bar_tab_events", "bar_tab_id", "bar_tabs"),
	child("cash_drawer_events", "drawer_id", "cash_drawers"),
	child("table_assignments", "table_id", "tables"),
	child("outlet_settings", "outlet_id", "outlets"),
	child("user_pos_roles", "user_id", "users"),

	// ---- tenant-scoped tables (carry tenant_id directly) ----
	// order: tables referenced by the children above come after those children;
	// tables that only reference outlets/tenants come before outlets/tenants.
	direct("pos_orders"),
	direct("pos_returns"),
	direct("prescriptions"),
	direct("gift_cards"),
	direct("promotions"),
	direct("channel_integrations"),
	direct("modifier_groups"),
	direct("price_books"),
	direct("staff_payrolls"),
	direct("repair_jobs"),
	direct("bar_tabs"),
	direct("cash_drawers"),
	direct("kds_tickets"),
	direct("kds_stations"),
	direct("kds_sync_failures"),
	direct("appointments"),
	direct("service_queue_entries"),
	direct("service_package_redemptions"),
	direct("service_package_purchases"),
	direct("service_packages"),
	direct("commission_records"),
	direct("commission_rules"),
	direct("client_records"),
	direct("loyalty_transactions"),
	direct("loyalty_accounts"),
	direct("loyalty_programs"),
	direct("layaway_payments"),
	direct("layaway_plans"),
	direct("bill_splits"),
	direct("drug_interaction_checks"),
	direct("controlled_substance_logs"),
	direct("serial_number_logs"),
	direct("weighing_scale_readings"),
	direct("daily_closings"),
	direct("stock_alert_subscriptions"),
	direct("stock_consumption_events"),
	direct("inventory_snapshots"),
	direct("pos_catalog_overrides"),
	direct("catalog_items"),
	direct("tenders"),
	direct("pos_notifications"),
	direct("audit_logs"),
	direct("integration_settings"),
	direct("webhook_subscriptions"),
	direct("backups"),
	direct("backup_settings"),
	direct("sync_failures"),
	direct("tenant_sync_events"),
	direct("idempotency_keys"),
	direct("outbox_events"),
	direct("meal_entitlements"),
	direct("event_bookings"),
	// hospitality: rooms & folios
	direct("housekeeping_tasks"),
	direct("room_amenity_assignments"),
	direct("room_amenities"),
	direct("room_folio_items"),
	direct("room_folio_payments"),
	direct("room_bookings"),
	direct("room_guests"),
	direct("rooms"),
	direct("facility_bookings"),
	direct("facilities"),
	// staffing & scheduling
	direct("staff_shift_overrides"),
	direct("shift_rotation_slots"),
	direct("shift_rotations"),
	direct("staff_schedules"),
	direct("leave_requests"),
	direct("staff_advances"),
	direct("staff_outlets"),
	direct("staff_members"),
	// tables / sections
	direct("table_reservations"),
	direct("tables"),
	direct("sections"),
	// resources / referrals
	direct("resources"),
	direct("referrals"),
	// rbac (tenant-scoped role assignments + custom roles; global permissions untouched)
	direct("pos_user_role_assignments"),
	direct("pos_roles"),
	// devices & sessions
	direct("pos_device_sessions"),
	direct("pos_devices"),
	// outlets, then users, then the tenant mirror row LAST
	direct("outlets"),
	direct("users"),
	direct("tenants"),
}

// Subscribe binds the durable JetStream consumer for "tenant.purge".
func (s *TenantPurgeSubscriber) Subscribe(nc *nats.Conn) error {
	if nc == nil {
		return fmt.Errorf("tenant purge subscriber: NATS connection is nil")
	}
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("tenant purge subscriber: jetstream init: %w", err)
	}

	// Ensure the "tenant" stream exists. subscriptions-api owns it; pos-api just
	// consumes. The tenant.purge subject lands on the "tenant" aggregate stream
	// (mirrors how StockSubscriber ensures the "inventory" stream).
	if _, err := js.StreamInfo("tenant"); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name:      "tenant",
			Subjects:  []string{"tenant.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		}); addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("tenant purge subscriber: ensure tenant stream: %w", addErr)
		}
	}

	sharedevents.SubscribeQueueWithRebind(s.log, js, "tenant", "tenant.purge", "pos-tenant-purge", func(msg *nats.Msg) {
		s.handle(msg)
	},
		nats.Durable("pos-tenant-purge"),
		nats.DeliverAll(),
		nats.AckExplicit(),
		nats.AckWait(2*time.Minute), // purge can touch many tables
		nats.MaxDeliver(5),          // bounded retry on transient DB errors
	)

	s.log.Info("tenant purge subscription registered", zap.String("subject", "tenant.purge"))
	return nil
}

// handle parses + validates the envelope, then runs the purge. It always Acks unless a
// transient DB error occurred, in which case it Naks for a bounded retry.
func (s *TenantPurgeSubscriber) handle(msg *nats.Msg) {
	evt, err := sharedevents.FromJSON(msg.Data)
	if err != nil {
		// Unparseable message: nothing safe to do with it. Ack so it never poison-loops.
		s.log.Error("tenant.purge: unmarshal failed — acking (cannot process)", zap.Error(err))
		_ = msg.Ack()
		return
	}

	// --- resolve tenant_id from the envelope, falling back to the payload ---
	tenantID := evt.TenantID
	if tenantID == uuid.Nil {
		if raw, _ := evt.Payload["tenant_id"].(string); raw != "" {
			if parsed, perr := uuid.Parse(raw); perr == nil {
				tenantID = parsed
			}
		}
	}

	// GUARD 1: never run a delete with an empty / nil tenant filter.
	if tenantID == uuid.Nil {
		s.log.Error("tenant.purge: missing/invalid tenant_id — acking, NOT purging",
			zap.Any("payload", evt.Payload))
		_ = msg.Ack()
		return
	}

	// GUARD 2: only a confirmed dormancy purge proceeds.
	confirmed, _ := evt.Payload["confirmed"].(bool)
	reason, _ := evt.Payload["reason"].(string)
	if !confirmed || reason != "dormancy" {
		s.log.Warn("tenant.purge: not a confirmed dormancy purge — acking, skipping",
			zap.String("tenant_id", tenantID.String()),
			zap.Bool("confirmed", confirmed),
			zap.String("reason", reason))
		_ = msg.Ack()
		return
	}

	s.log.Warn("tenant.purge: CONFIRMED dormancy purge — deleting ALL data for tenant",
		zap.String("tenant_id", tenantID.String()))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := s.purge(ctx, tenantID); err != nil {
		// Transient DB error: Nak for bounded retry (MaxDeliver caps the loop).
		s.log.Error("tenant.purge: purge failed — naking for retry",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		_ = msg.Nak()
		return
	}

	_ = msg.Ack()
}

// purge deletes every owned row for tenantID inside a single transaction. On any error
// the whole transaction rolls back (no partial deletes) and the error is returned so the
// caller can Nak. Idempotent: re-running deletes nothing more.
func (s *TenantPurgeSubscriber) purge(ctx context.Context, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		// Defense-in-depth: refuse a nil filter even if reached directly.
		return fmt.Errorf("refusing to purge with nil tenant_id")
	}
	if s.db == nil {
		return fmt.Errorf("tenant purge: nil db handle")
	}

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

	total := int64(0)
	counts := make([]zap.Field, 0, len(purgePlan)+2)
	for _, step := range purgePlan {
		var q string
		switch step.kind {
		case "direct":
			q = fmt.Sprintf(`DELETE FROM %q WHERE tenant_id = $1`, step.table)
		case "child":
			q = fmt.Sprintf(
				`DELETE FROM %q WHERE %q IN (SELECT id FROM %q WHERE tenant_id = $1)`,
				step.table, step.fkCol, step.parent)
		default:
			continue
		}

		res, err := tx.ExecContext(ctx, q, tenantID)
		if err != nil {
			// A missing table on a given environment (additive-migrate skew) must not abort
			// the whole purge — log and continue. Any other error aborts + rolls back.
			if isUndefinedTable(err) {
				s.log.Warn("tenant.purge: table absent in this env — skipping",
					zap.String("table", step.table), zap.Error(err))
				continue
			}
			return fmt.Errorf("delete %s: %w", step.table, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			counts = append(counts, zap.Int64(step.table, n))
			total += n
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit purge: %w", err)
	}
	committed = true

	fields := append([]zap.Field{
		zap.String("tenant_id", tenantID.String()),
		zap.Int64("rows_deleted_total", total),
	}, counts...)
	s.log.Warn("tenant.purge: COMPLETED — tenant data deleted", fields...)
	return nil
}

// isUndefinedTable reports whether err is a Postgres "undefined_table" (42P01) error,
// matched by message substring to avoid a hard dependency on a specific pg driver type.
func isUndefinedTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "42P01") ||
		(strings.Contains(msg, "does not exist") && strings.Contains(msg, "relation"))
}

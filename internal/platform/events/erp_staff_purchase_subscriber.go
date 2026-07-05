package events

import (
	"context"
	"fmt"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/modules/staffcredit"
)

// ERPStaffPurchaseSubscriber consumes erp.staff_purchase.recovered / recovery_reversed and pays down
// (or re-opens) the local StaffPurchaseLink + linked layaway as ERP payroll recovers the debt.
type ERPStaffPurchaseSubscriber struct {
	svc *staffcredit.Service
	log *zap.Logger
}

func NewERPStaffPurchaseSubscriber(svc *staffcredit.Service, log *zap.Logger) *ERPStaffPurchaseSubscriber {
	return &ERPStaffPurchaseSubscriber{svc: svc, log: log.Named("pos.erp_staff_purchase_subscriber")}
}

func (s *ERPStaffPurchaseSubscriber) Subscribe(nc *nats.Conn) error {
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("pos erp-staff-purchase subscriber: jetstream init: %w", err)
	}
	// Ensure the erp stream exists (erp-api owns it; pos-api just consumes).
	if _, err := js.StreamInfo("erp"); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name:      "erp",
			Subjects:  []string{"erp.>"},
			Retention: nats.LimitsPolicy,
			MaxAge:    72 * time.Hour,
			Storage:   nats.FileStorage,
		}); addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("pos erp-staff-purchase subscriber: ensure erp stream: %w", addErr)
		}
	}

	sharedevents.SubscribeQueueWithRebind(s.log, js, "erp", "erp.staff_purchase.recovered", "pos-erp-staff-purchase-recovered", func(msg *nats.Msg) {
		s.handle(msg, "recovered")
	}, nats.Durable("pos-erp-staff-purchase-recovered"), nats.AckWait(30*time.Second), nats.MaxDeliver(5))

	sharedevents.SubscribeQueueWithRebind(s.log, js, "erp", "erp.staff_purchase.recovery_reversed", "pos-erp-staff-purchase-reversed", func(msg *nats.Msg) {
		s.handle(msg, "reversed")
	}, nats.Durable("pos-erp-staff-purchase-reversed"), nats.AckWait(30*time.Second), nats.MaxDeliver(5))

	// Reverse staff↔employee sync: patch the local StaffMember with the HR employee number.
	sharedevents.SubscribeQueueWithRebind(s.log, js, "erp", "erp.employee.upserted", "pos-erp-employee-upserted", func(msg *nats.Msg) {
		s.handleEmployeeUpserted(msg)
	}, nats.Durable("pos-erp-employee-upserted"), nats.AckWait(30*time.Second), nats.MaxDeliver(5))

	s.log.Info("subscribed to erp staff-purchase settlement + employee-upserted events")
	return nil
}

func (s *ERPStaffPurchaseSubscriber) handleEmployeeUpserted(msg *nats.Msg) {
	evt, err := sharedevents.FromJSON(msg.Data)
	if err != nil {
		_ = msg.Ack()
		return
	}
	tenantID := evt.TenantID
	userIDStr, _ := evt.Payload["auth_user_id"].(string)
	userID, uerr := uuid.Parse(userIDStr)
	empNo, _ := evt.Payload["employee_number"].(string)
	name, _ := evt.Payload["name"].(string)
	if tenantID == uuid.Nil || uerr != nil {
		_ = msg.Ack()
		return
	}
	if err := s.svc.SyncEmployeeInfo(context.Background(), tenantID, userID, empNo, name); err != nil {
		s.log.Warn("employee-upserted sync failed (will retry)", zap.Error(err))
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

func (s *ERPStaffPurchaseSubscriber) handle(msg *nats.Msg, kind string) {
	evt, err := sharedevents.FromJSON(msg.Data)
	if err != nil {
		s.log.Warn("bad staff-purchase event", zap.Error(err))
		_ = msg.Ack() // poison message — drop
		return
	}
	tenantID := evt.TenantID
	if tenantID == uuid.Nil {
		_ = msg.Ack()
		return
	}
	sourceKey, _ := evt.Payload["source_key"].(string)
	if sourceKey == "" {
		_ = msg.Ack()
		return
	}
	totalStr, _ := evt.Payload["amount_recovered_total"].(string)
	total, derr := decimal.NewFromString(totalStr)
	if derr != nil {
		total = decimal.Zero
	}
	settled, _ := evt.Payload["settled"].(bool)

	if err := s.svc.Settle(context.Background(), tenantID, sourceKey, total, settled); err != nil {
		s.log.Warn("staff-purchase settle failed (will retry)", zap.String("kind", kind), zap.String("source_key", sourceKey), zap.Error(err))
		_ = msg.Nak() // transient — redeliver
		return
	}
	_ = msg.Ack()
}

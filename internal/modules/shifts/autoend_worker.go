package shifts

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevice"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/ent/outletsetting"
)

// AutoEndWorker periodically closes shift sessions that have exceeded the
// configured shift_max_hours for their outlet. Runs every 15 minutes.
type AutoEndWorker struct {
	client *ent.Client
	log    *zap.Logger
}

func NewAutoEndWorker(client *ent.Client, log *zap.Logger) *AutoEndWorker {
	return &AutoEndWorker{client: client, log: log.Named("shifts.autoend")}
}

func (w *AutoEndWorker) Start(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	w.log.Info("shift auto-end worker started")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil {
				w.log.Error("shift auto-end run failed", zap.Error(err))
			}
		}
	}
}

func (w *AutoEndWorker) runOnce(ctx context.Context) error {
	// Load all outlet settings that have auto-end enabled.
	settings, err := w.client.OutletSetting.Query().
		Where(outletsetting.ShiftAutoEndEnabled(true)).
		All(ctx)
	if err != nil {
		return err
	}
	if len(settings) == 0 {
		return nil
	}

	now := time.Now()
	for _, s := range settings {
		maxHours := s.ShiftMaxHours
		if maxHours <= 0 {
			maxHours = 12
		}
		cutoff := now.Add(-time.Duration(maxHours) * time.Hour)

		// Resolve device IDs for this outlet.
		devices, err := w.client.POSDevice.Query().
			Where(posdevice.OutletID(s.OutletID)).
			All(ctx)
		if err != nil || len(devices) == 0 {
			continue
		}
		deviceIDs := make([]uuid.UUID, len(devices))
		for i, d := range devices {
			deviceIDs[i] = d.ID
		}

		// Find open sessions on those devices that started before the cutoff.
		openSessions, err := w.client.POSDeviceSession.Query().
			Where(
				posdevicesession.DeviceIDIn(deviceIDs...),
				posdevicesession.SessionStatus("open"),
				posdevicesession.OpenedAtLT(cutoff),
			).
			All(ctx)
		if err != nil {
			w.log.Error("query open sessions failed", zap.String("outlet", s.OutletID.String()), zap.Error(err))
			continue
		}
		for _, sess := range openSessions {
			t := now
			_, err := w.client.POSDeviceSession.UpdateOneID(sess.ID).
				SetSessionStatus("closed").
				SetClosedAt(t).
				Save(ctx)
			if err != nil {
				w.log.Error("auto-close session failed", zap.String("session", sess.ID.String()), zap.Error(err))
				continue
			}
			w.log.Info("auto-ended shift session",
				zap.String("session_id", sess.ID.String()),
				zap.Duration("age", now.Sub(sess.OpenedAt)),
			)
		}
	}
	return nil
}

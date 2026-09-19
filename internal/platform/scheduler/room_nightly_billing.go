package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/roomfolioitem"
	"github.com/bengobox/pos-service/internal/ent/roomguest"
)

// RoomNightlyBillingScheduler posts the next night's room charge for every active hotel stay
// under a per_day_split booking policy (OutletSetting.metadata.booking_policy.payment_timing —
// see pos-api's resolveBookingPolicy). CheckIn posts only the FIRST night's charge up front for
// this policy; this scheduler posts each subsequent night as it actually occurs. That's the
// whole point of "per_day_split" as a real incremental-billing policy rather than just a
// per-night line-item label on one lump sum charged all at check-in — it also means an early
// checkout under this policy only ever owes the nights actually stayed.
//
// Runs once at startup, then hourly (frequent enough to catch up quickly after a restart or a
// missed run, without a tight loop hammering the DB — most iterations touch zero rows since a
// guest crosses a new elapsed night only once every 24h).
type RoomNightlyBillingScheduler struct {
	log *zap.Logger
	db  *ent.Client
}

func NewRoomNightlyBillingScheduler(log *zap.Logger, db *ent.Client) *RoomNightlyBillingScheduler {
	return &RoomNightlyBillingScheduler{log: log, db: db}
}

func (s *RoomNightlyBillingScheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	s.run(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.run(ctx)
		}
	}
}

func (s *RoomNightlyBillingScheduler) run(ctx context.Context) {
	guests, err := s.db.RoomGuest.Query().
		Where(roomguest.StatusEQ(roomguest.StatusActive)).
		All(ctx)
	if err != nil {
		s.log.Error("room nightly billing: query active guests failed", zap.Error(err))
		return
	}

	posted := 0
	for _, guest := range guests {
		room, rerr := s.db.Room.Get(ctx, guest.RoomID)
		if rerr != nil {
			continue
		}
		if !s.isPerDaySplit(ctx, room.OutletID) {
			continue
		}
		if guest.Nights <= 0 {
			continue
		}

		elapsedDays := int(time.Since(guest.CheckInDate).Hours() / 24)
		nightsDue := elapsedDays + 1
		if nightsDue > guest.Nights {
			nightsDue = guest.Nights
		}

		postedCount, cerr := s.db.RoomFolioItem.Query().
			Where(roomfolioitem.RoomGuestID(guest.ID), roomfolioitem.ChargeTypeEQ(roomfolioitem.ChargeTypeRoomCharge)).
			Count(ctx)
		if cerr != nil {
			s.log.Warn("room nightly billing: count folio items failed", zap.Stringer("guest_id", guest.ID), zap.Error(cerr))
			continue
		}
		if postedCount >= nightsDue {
			continue
		}

		nightlyRate := guest.TotalRoomCharge / float64(guest.Nights)
		if nightlyRate <= 0 {
			continue
		}

		for night := postedCount + 1; night <= nightsDue; night++ {
			_, ferr := s.db.RoomFolioItem.Create().
				SetTenantID(guest.TenantID).
				SetRoomID(guest.RoomID).
				SetRoomGuestID(guest.ID).
				SetDescription(fmt.Sprintf("Room charge — Night %d of %d", night, guest.Nights)).
				SetAmount(nightlyRate).
				SetCurrency(room.Currency).
				SetChargeType(roomfolioitem.ChargeTypeRoomCharge).
				SetCreatedBy(guest.CheckedInBy).
				Save(ctx)
			if ferr != nil {
				s.log.Warn("room nightly billing: post folio item failed",
					zap.Stringer("guest_id", guest.ID), zap.Int("night", night), zap.Error(ferr))
				break
			}
			posted++
		}
	}
	if posted > 0 {
		s.log.Info("room nightly billing: posted incremental night charges", zap.Int("count", posted))
	}
}

// isPerDaySplit reads the outlet's booking_policy.payment_timing straight from
// OutletSetting.metadata. Deliberately narrower than pos-api's resolveBookingPolicy (which also
// carries the amendment/cancellation fee fields the HTTP handlers need) — not worth importing
// the handlers package for one field, so this package keeps its own minimal read.
func (s *RoomNightlyBillingScheduler) isPerDaySplit(ctx context.Context, outletID uuid.UUID) bool {
	setting, err := s.db.OutletSetting.Query().Where(outletsetting.OutletID(outletID)).Only(ctx)
	if err != nil || setting.Metadata == nil {
		return false
	}
	bp, ok := setting.Metadata["booking_policy"].(map[string]any)
	if !ok {
		return false
	}
	timing, _ := bp["payment_timing"].(string)
	return timing == "per_day_split"
}

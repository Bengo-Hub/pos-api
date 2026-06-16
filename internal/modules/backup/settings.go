package backup

import (
	"context"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entbackup "github.com/bengobox/pos-service/internal/ent/backup"
	entbackupsetting "github.com/bengobox/pos-service/internal/ent/backupsetting"
)

// Settings is a tenant's auto-backup configuration. Auto-backup is OPT-IN: it runs only
// when AutoEnabled is true (default off — activated from the UI).
type Settings struct {
	AutoEnabled   bool `json:"auto_enabled"`
	ScheduleHour  int  `json:"schedule_hour"`
	RetentionDays int  `json:"retention_days"`
}

// DefaultSettings is the settings a tenant has before activating auto-backup: off, 02:00,
// default retention.
func DefaultSettings() Settings {
	return Settings{AutoEnabled: false, ScheduleHour: 2, RetentionDays: DefaultRetentionDays}
}

// GetSettings returns the tenant's stored settings, or DefaultSettings() when no row exists.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (Settings, error) {
	row, err := s.orm.BackupSetting.Query().
		Where(entbackupsetting.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return DefaultSettings(), nil
		}
		return DefaultSettings(), err
	}
	return Settings{
		AutoEnabled:   row.AutoEnabled,
		ScheduleHour:  row.ScheduleHour,
		RetentionDays: row.RetentionDays,
	}, nil
}

// UpsertSettings clamps + persists the tenant's settings (update if a row exists, else
// create) and returns the stored values.
func (s *Service) UpsertSettings(ctx context.Context, tenantID uuid.UUID, in Settings) (Settings, error) {
	if in.ScheduleHour < 0 || in.ScheduleHour > 23 {
		in.ScheduleHour = 2
	}
	if in.RetentionDays <= 0 {
		in.RetentionDays = DefaultRetentionDays
	}

	existing, err := s.orm.BackupSetting.Query().
		Where(entbackupsetting.TenantID(tenantID)).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return Settings{}, err
	}

	if existing != nil {
		_, err = s.orm.BackupSetting.UpdateOne(existing).
			SetAutoEnabled(in.AutoEnabled).
			SetScheduleHour(in.ScheduleHour).
			SetRetentionDays(in.RetentionDays).
			Save(ctx)
	} else {
		_, err = s.orm.BackupSetting.Create().
			SetTenantID(tenantID).
			SetAutoEnabled(in.AutoEnabled).
			SetScheduleHour(in.ScheduleHour).
			SetRetentionDays(in.RetentionDays).
			Save(ctx)
	}
	if err != nil {
		return Settings{}, err
	}
	return in, nil
}

// ActivatedTenant identifies a tenant that has opted in to auto-backup at a given hour.
type ActivatedTenant struct {
	TenantID      uuid.UUID
	RetentionDays int
}

// ListActivatedTenants returns the tenants that have opted in (AutoEnabled) AND whose
// configured schedule hour matches hour.
func (s *Service) ListActivatedTenants(ctx context.Context, hour int) ([]ActivatedTenant, error) {
	rows, err := s.orm.BackupSetting.Query().
		Where(
			entbackupsetting.AutoEnabled(true),
			entbackupsetting.ScheduleHour(hour),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ActivatedTenant, 0, len(rows))
	for _, r := range rows {
		out = append(out, ActivatedTenant{TenantID: r.TenantID, RetentionDays: r.RetentionDays})
	}
	return out, nil
}

// ChurnTenant deletes the given tenant's backups older than retentionDays (file + tracking
// row). Used by the per-tenant scheduled auto-backup so each tenant's retention is honored.
func (s *Service) ChurnTenant(ctx context.Context, tenantID uuid.UUID, retentionDays int) (int, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	rows, err := s.orm.Backup.Query().
		Where(
			entbackup.TenantID(tenantID),
			entbackup.CreatedAtLT(cutoff),
		).
		All(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, r := range rows {
		if err := os.Remove(r.Path); err != nil && !os.IsNotExist(err) {
			continue
		}
		if err := s.orm.Backup.DeleteOne(r).Exec(ctx); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

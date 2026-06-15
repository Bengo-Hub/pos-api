package backup

import (
	"context"
	"os"
	"time"

	"go.uber.org/zap"

	entbackup "github.com/bengobox/pos-service/internal/ent/backup"
)

// DefaultRetentionDays is the default backup retention window (configurable via
// BACKUP_RETENTION_DAYS). Backups older than this are deleted (file + tracking row).
const DefaultRetentionDays = 4

// Churn deletes backups older than retentionDays across ALL tenants — both the artifact
// file and its tracking row. Returns the number of artifacts removed. Idempotent.
func (s *Service) Churn(ctx context.Context, retentionDays int) (int, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	rows, err := s.orm.Backup.Query().
		Where(entbackup.CreatedAtLT(cutoff)).
		All(ctx)
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, r := range rows {
		if err := os.Remove(r.Path); err != nil && !os.IsNotExist(err) {
			s.log.Warn("churn: remove file failed", zap.String("path", r.Path), zap.Error(err))
			continue
		}
		if err := s.orm.Backup.DeleteOne(r).Exec(ctx); err != nil {
			s.log.Warn("churn: delete row failed", zap.String("id", r.ID.String()), zap.Error(err))
			continue
		}
		removed++
	}

	removed += s.churnOrphanFiles(cutoff)

	if removed > 0 {
		s.log.Info("backup churn complete", zap.Int("removed", removed), zap.Int("retention_days", retentionDays))
	}
	return removed, nil
}

// churnOrphanFiles removes on-disk backup files older than cutoff that have no DB row.
func (s *Service) churnOrphanFiles(cutoff time.Time) int {
	tenantDirs, err := os.ReadDir(s.root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, td := range tenantDirs {
		if !td.IsDir() {
			continue
		}
		dir := s.root + string(os.PathSeparator) + td.Name()
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			exists, _ := s.orm.Backup.Query().Where(entbackup.Name(e.Name())).Exist(context.Background())
			if exists {
				continue
			}
			if err := os.Remove(dir + string(os.PathSeparator) + e.Name()); err == nil {
				removed++
			}
		}
	}
	return removed
}

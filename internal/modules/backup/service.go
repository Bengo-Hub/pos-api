// Package backup produces TENANT-SCOPED logical exports of pos's own data. A backup
// contains only the requesting tenant's rows (every table that carries tenant_id), gzipped
// JSON, stored under a per-tenant backup path. It is NEVER a platform-wide DB dump (see the
// platform DR backup in auth-api). Artifacts are tracked in the `backups` ent table.
//
// Adapted from erp-api's reference backup module.
package backup

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entbackup "github.com/bengobox/pos-service/internal/ent/backup"
)

// Service generates + serves tenant-scoped backups, tracked in the `backups` ent table.
type Service struct {
	db   *sql.DB
	orm  *ent.Client
	root string
	log  *zap.Logger
}

// NewService wires the backup service. root is the directory backups are written under.
func NewService(db *sql.DB, orm *ent.Client, root string, log *zap.Logger) *Service {
	return &Service{db: db, orm: orm, root: filepath.Join(root, "backups"), log: log.Named("backup.Service")}
}

// Info describes one stored backup artifact (DB row + file).
type Info struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) tenantDir(tenantID uuid.UUID) string {
	return filepath.Join(s.root, tenantID.String())
}

// tenantScopedTables lists public tables that carry a tenant_id column.
func (s *Service) tenantScopedTables(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND column_name = 'tenant_id'
		 ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// Generate writes a gzipped JSON export of the tenant's rows, records a tracking row, and
// returns its Info.
func (s *Service) Generate(ctx context.Context, tenantID uuid.UUID) (Info, error) {
	tables, err := s.tenantScopedTables(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("discover tables: %w", err)
	}

	export := map[string]any{
		"service":      "pos",
		"tenant_id":    tenantID.String(),
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"tables":       map[string]json.RawMessage{},
	}
	tableData := export["tables"].(map[string]json.RawMessage)
	for _, t := range tables {
		q := fmt.Sprintf(`SELECT coalesce(json_agg(row_to_json(x)), '[]'::json) FROM %q x WHERE tenant_id = $1`, t)
		var raw []byte
		if err := s.db.QueryRowContext(ctx, q, tenantID).Scan(&raw); err != nil {
			s.log.Warn("backup: dump table failed (skipped)", zap.String("table", t), zap.Error(err))
			continue
		}
		tableData[t] = raw
	}

	dir := s.tenantDir(tenantID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Info{}, fmt.Errorf("mkdir: %w", err)
	}
	name := fmt.Sprintf("pos_backup_%s.json.gz", time.Now().UTC().Format("20060102_150405"))
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		return Info{}, fmt.Errorf("create: %w", err)
	}
	gz := gzip.NewWriter(f)
	if err := json.NewEncoder(gz).Encode(export); err != nil {
		_ = gz.Close()
		_ = f.Close()
		_ = os.Remove(path)
		return Info{}, fmt.Errorf("encode: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return Info{}, fmt.Errorf("gzip close: %w", err)
	}
	st, _ := f.Stat()
	size := sizeOf(st)
	_ = f.Close()

	row, err := s.orm.Backup.Create().
		SetTenantID(tenantID).
		SetName(name).
		SetPath(path).
		SetSizeBytes(size).
		SetStatus("completed").
		Save(ctx)
	if err != nil {
		s.log.Warn("backup: track row insert failed", zap.String("file", name), zap.Error(err))
		return Info{Name: name, Size: size, Status: "completed", CreatedAt: time.Now().UTC()}, nil
	}

	s.log.Info("tenant backup generated", zap.String("tenant", tenantID.String()), zap.String("file", name))
	return toInfo(row), nil
}

// List returns the tenant's tracked backups whose file still exists, newest first.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]Info, error) {
	rows, err := s.orm.Backup.Query().
		Where(entbackup.TenantID(tenantID)).
		Order(ent.Desc(entbackup.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(rows))
	for _, r := range rows {
		if _, statErr := os.Stat(r.Path); statErr != nil {
			continue
		}
		out = append(out, toInfo(r))
	}
	return out, nil
}

// Open returns a reader for one of the tenant's backups, rejecting path traversal and any
// name that does not resolve INSIDE that tenant's own backup directory.
func (s *Service) Open(tenantID uuid.UUID, name string) (io.ReadCloser, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid backup name")
	}
	dir := s.tenantDir(tenantID)
	path := filepath.Join(dir, name)
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(dir)+string(os.PathSeparator)) {
		return nil, fmt.Errorf("invalid backup path")
	}
	return os.Open(path)
}

// Delete removes one of the tenant's backups (file + tracking row), same ownership guard.
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, name string) error {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid backup name")
	}
	path := filepath.Join(s.tenantDir(tenantID), name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = s.orm.Backup.Delete().
		Where(entbackup.TenantID(tenantID), entbackup.Name(name)).
		Exec(ctx)
	return nil
}

func toInfo(r *ent.Backup) Info {
	return Info{
		ID:        r.ID.String(),
		Name:      r.Name,
		Size:      r.SizeBytes,
		Status:    r.Status,
		CreatedAt: r.CreatedAt.UTC(),
	}
}

func sizeOf(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}

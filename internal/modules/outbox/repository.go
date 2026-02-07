package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxRepository implements events.OutboxRepository using pgx.
type PgxRepository struct {
	pool *pgxpool.Pool
}

// NewPgxRepository creates a new outbox repository.
func NewPgxRepository(pool *pgxpool.Pool) *PgxRepository {
	return &PgxRepository{pool: pool}
}

// CreateOutboxRecord stores an event in the outbox within a transaction.
// Note: This uses database/sql.Tx for compatibility with shared-events interface.
// For pgx transactions, use CreateOutboxRecordPgx instead.
func (r *PgxRepository) CreateOutboxRecord(ctx context.Context, tx *sql.Tx, record *events.OutboxRecord) error {
	query := `
		INSERT INTO outbox_events (id, tenant_id, aggregate_type, aggregate_id, event_type, payload, status, attempts, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := tx.ExecContext(ctx, query,
		record.ID,
		record.TenantID,
		record.AggregateType,
		record.AggregateID,
		record.EventType,
		record.Payload,
		record.Status,
		record.Attempts,
		record.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert outbox record: %w", err)
	}
	return nil
}

// CreateOutboxRecordPgx creates an outbox record within a pgx transaction.
func (r *PgxRepository) CreateOutboxRecordPgx(ctx context.Context, tx pgx.Tx, record *events.OutboxRecord) error {
	query := `
		INSERT INTO outbox_events (id, tenant_id, aggregate_type, aggregate_id, event_type, payload, status, attempts, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := tx.Exec(ctx, query,
		record.ID,
		record.TenantID,
		record.AggregateType,
		record.AggregateID,
		record.EventType,
		record.Payload,
		record.Status,
		record.Attempts,
		record.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert outbox record: %w", err)
	}
	return nil
}

// GetPendingRecords fetches pending events for publishing.
func (r *PgxRepository) GetPendingRecords(ctx context.Context, limit int) ([]*events.OutboxRecord, error) {
	query := `
		SELECT id, tenant_id, aggregate_type, aggregate_id, event_type, payload, status, attempts, last_attempt_at, published_at, error_message, created_at
		FROM outbox_events
		WHERE status = 'PENDING'
		ORDER BY created_at ASC
		LIMIT $1
	`
	rows, err := r.pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query pending records: %w", err)
	}
	defer rows.Close()

	var records []*events.OutboxRecord
	for rows.Next() {
		var rec events.OutboxRecord
		err := rows.Scan(
			&rec.ID,
			&rec.TenantID,
			&rec.AggregateType,
			&rec.AggregateID,
			&rec.EventType,
			&rec.Payload,
			&rec.Status,
			&rec.Attempts,
			&rec.LastAttemptAt,
			&rec.PublishedAt,
			&rec.ErrorMessage,
			&rec.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		records = append(records, &rec)
	}

	return records, rows.Err()
}

// MarkAsPublished marks an event as successfully published.
func (r *PgxRepository) MarkAsPublished(ctx context.Context, id uuid.UUID, publishedAt time.Time) error {
	query := `UPDATE outbox_events SET status = 'PUBLISHED', published_at = $2 WHERE id = $1`
	_, err := r.pool.Exec(ctx, query, id, publishedAt)
	return err
}

// MarkAsFailed marks an event as failed and increments attempts.
func (r *PgxRepository) MarkAsFailed(ctx context.Context, id uuid.UUID, errorMessage string, lastAttemptAt time.Time) error {
	// Increment attempts and check if max reached
	query := `
		UPDATE outbox_events
		SET
			attempts = attempts + 1,
			last_attempt_at = $2,
			error_message = $3,
			status = CASE WHEN attempts + 1 >= 10 THEN 'FAILED' ELSE 'PENDING' END
		WHERE id = $1
	`
	_, err := r.pool.Exec(ctx, query, id, lastAttemptAt, errorMessage)
	return err
}

// BeginTx starts a database transaction (database/sql interface).
func (r *PgxRepository) BeginTx(ctx context.Context) (*sql.Tx, error) {
	// This is not directly supported with pgxpool.
	// Services using pgx should use BeginPgxTx instead.
	return nil, fmt.Errorf("use BeginPgxTx for pgx-based services")
}

// BeginPgxTx starts a pgx transaction.
func (r *PgxRepository) BeginPgxTx(ctx context.Context) (pgx.Tx, error) {
	return r.pool.Begin(ctx)
}

// CreateEventInPgxTx is a convenience method for creating an outbox event within a pgx transaction.
func CreateEventInPgxTx(ctx context.Context, tx pgx.Tx, event *events.Event) error {
	payload, err := event.ToJSON()
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	query := `
		INSERT INTO outbox_events (id, tenant_id, aggregate_type, aggregate_id, event_type, payload, status, attempts, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'PENDING', 0, $7)
	`
	_, err = tx.Exec(ctx, query,
		event.ID,
		event.TenantID,
		event.AggregateType,
		event.AggregateID,
		event.EventType,
		payload,
		time.Now().UTC(),
	)
	return err
}

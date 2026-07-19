// Package documents provides per-tenant atomic document numbering for pos-api. It mirrors
// inventory/treasury-api's DocumentSequence numbering: an optimistic compare-and-set on
// DocumentSequence.current_val yields race-safe, tenant-configurable document numbers
// (order, receipt, return, reversal, repair-job). The platform default is PURE NUMERIC.
package documents

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entdocseq "github.com/bengobox/pos-service/internal/ent/documentsequence"
)

// Doc type constants — must match DocumentSequence.doc_type values.
const (
	DocTypeOrder       = "order"
	DocTypePosReceipt  = "pos_receipt"
	DocTypePosReturn   = "pos_return"
	DocTypePosReversal = "pos_reversal"
	DocTypeRepairJob   = "repair_job"
)

type seqConfig struct {
	Prefix     string
	Separator  string
	DateFormat string
	Padding    int
	ResetFreq  string
}

// Platform default is PURE NUMERIC: empty prefix + empty date_format ⇒ formatNumber emits just
// the zero-padded counter (e.g. "000001"). Tenants who prefer the prefixed/dated style (POS-260707-
// 000013) opt in per doc type in Settings → Documents, which sets a prefix and/or date_format.
var seqDefaults = map[string]seqConfig{
	DocTypeOrder:       {Separator: "-", Padding: 6, ResetFreq: "never"},
	DocTypePosReceipt:  {Separator: "-", Padding: 6, ResetFreq: "never"},
	DocTypePosReturn:   {Separator: "-", Padding: 6, ResetFreq: "never"},
	DocTypePosReversal: {Separator: "-", Padding: 6, ResetFreq: "never"},
	DocTypeRepairJob:   {Separator: "-", Padding: 6, ResetFreq: "never"},
}

// SuggestedPrefixes are the pre-fill hints the Settings UI offers when a tenant switches a doc
// type to the prefixed format. NOT applied automatically — the platform default is numeric.
var SuggestedPrefixes = map[string]string{
	DocTypeOrder:       "POS",
	DocTypePosReceipt:  "RCT",
	DocTypePosReturn:   "RET",
	DocTypePosReversal: "REV",
	DocTypeRepairJob:   "JOB",
}

// SequenceService generates per-tenant atomic document numbers using optimistic
// compare-and-set on DocumentSequence.current_val (race-safe without raw SQL locks).
type SequenceService struct {
	ent *ent.Client
}

// NewSequenceService creates a SequenceService.
func NewSequenceService(client *ent.Client) *SequenceService {
	return &SequenceService{ent: client}
}

// GenerateNumber atomically increments the (tenant, docType) counter and returns a
// formatted document number. Retries up to 5 times on CAS contention.
func (s *SequenceService) GenerateNumber(ctx context.Context, tenantID uuid.UUID, docType string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(10*(1<<attempt))*time.Millisecond + time.Duration(rand.Intn(20))*time.Millisecond)
		}
		row, err := s.getOrCreate(ctx, tenantID, docType)
		if err != nil {
			lastErr = err
			continue
		}
		old := row.CurrentVal
		affected, err := s.ent.DocumentSequence.Update().
			Where(entdocseq.IDEQ(row.ID), entdocseq.CurrentValEQ(old)).
			SetCurrentVal(old + 1).
			Save(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		if affected == 0 {
			lastErr = fmt.Errorf("sequence CAS contention")
			continue
		}
		return formatNumber(row, old+1, time.Now()), nil
	}
	return "", fmt.Errorf("generate document number after retries: %w", lastErr)
}

// SeqConfigDTO is the editable + derived view of a document-sequence configuration.
type SeqConfigDTO struct {
	DocType    string `json:"doc_type"`
	Prefix     string `json:"prefix"`
	Separator  string `json:"separator"`
	DateFormat string `json:"date_format"`
	Padding    int    `json:"padding"`
	ResetFreq  string `json:"reset_freq"`
	CurrentVal int64  `json:"current_val"`
	NextNumber string `json:"next_number"`
}

// configuredDocTypes is the set of POS document types that carry a configurable sequence,
// surfaced in the Settings → Documents tab.
var configuredDocTypes = []string{
	DocTypeOrder, DocTypePosReceipt, DocTypePosReturn, DocTypePosReversal, DocTypeRepairJob,
}

func toSeqDTO(row *ent.DocumentSequence) SeqConfigDTO {
	return SeqConfigDTO{
		DocType: row.DocType, Prefix: row.Prefix, Separator: row.Separator,
		DateFormat: row.DateFormat, Padding: row.Padding, ResetFreq: row.ResetFreq,
		CurrentVal: row.CurrentVal, NextNumber: formatNumber(row, row.CurrentVal+1, time.Now()),
	}
}

// ListConfigs returns the configuration (auto-seeding defaults) for every POS doc type.
func (s *SequenceService) ListConfigs(ctx context.Context, tenantID uuid.UUID) ([]SeqConfigDTO, error) {
	out := make([]SeqConfigDTO, 0, len(configuredDocTypes))
	for _, dt := range configuredDocTypes {
		row, err := s.getOrCreate(ctx, tenantID, dt)
		if err != nil {
			return nil, err
		}
		out = append(out, toSeqDTO(row))
	}
	return out, nil
}

// PreviewNext returns the next document number without consuming the counter.
func (s *SequenceService) PreviewNext(ctx context.Context, tenantID uuid.UUID, docType string) (string, error) {
	row, err := s.getOrCreate(ctx, tenantID, docType)
	if err != nil {
		return "", err
	}
	return formatNumber(row, row.CurrentVal+1, time.Now()), nil
}

// UpdateConfig updates the format fields (never the counter) for a doc type's sequence.
func (s *SequenceService) UpdateConfig(ctx context.Context, tenantID uuid.UUID, docType string, cfg SeqConfigDTO) (SeqConfigDTO, error) {
	row, err := s.getOrCreate(ctx, tenantID, docType)
	if err != nil {
		return SeqConfigDTO{}, err
	}
	padding := cfg.Padding
	if padding <= 0 || padding > 12 {
		padding = row.Padding
	}
	sep := cfg.Separator
	if sep == "" {
		sep = "-"
	}
	updated, err := s.ent.DocumentSequence.UpdateOneID(row.ID).
		SetPrefix(strings.TrimSpace(cfg.Prefix)).
		SetSeparator(sep).
		SetDateFormat(strings.ToUpper(strings.TrimSpace(cfg.DateFormat))).
		SetPadding(padding).
		SetResetFreq(cfg.ResetFreq).
		Save(ctx)
	if err != nil {
		return SeqConfigDTO{}, err
	}
	return toSeqDTO(updated), nil
}

func (s *SequenceService) getOrCreate(ctx context.Context, tenantID uuid.UUID, docType string) (*ent.DocumentSequence, error) {
	row, err := s.ent.DocumentSequence.Query().
		Where(entdocseq.TenantID(tenantID), entdocseq.DocType(docType)).
		Only(ctx)
	if err == nil {
		return row, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	cfg, ok := seqDefaults[docType]
	if !ok {
		cfg = seqConfig{Prefix: strings.ToUpper(docType), Separator: "-", DateFormat: "YYMMDD", Padding: 6, ResetFreq: "never"}
	}
	created, err := s.ent.DocumentSequence.Create().
		SetTenantID(tenantID).SetDocType(docType).
		SetPrefix(cfg.Prefix).SetSeparator(cfg.Separator).SetDateFormat(cfg.DateFormat).
		SetPadding(cfg.Padding).SetResetFreq(cfg.ResetFreq).SetCurrentVal(0).
		Save(ctx)
	if err != nil {
		// Likely a concurrent create — re-read.
		if row2, e2 := s.ent.DocumentSequence.Query().Where(entdocseq.TenantID(tenantID), entdocseq.DocType(docType)).Only(ctx); e2 == nil {
			return row2, nil
		}
		return nil, err
	}
	return created, nil
}

func formatNumber(row *ent.DocumentSequence, val int64, now time.Time) string {
	parts := make([]string, 0, 3)
	if row.Prefix != "" {
		parts = append(parts, row.Prefix)
	}
	if d := formatDate(row.DateFormat, now); d != "" {
		parts = append(parts, d)
	}
	parts = append(parts, fmt.Sprintf("%0*d", row.Padding, val))
	sep := row.Separator
	if sep == "" {
		sep = "-"
	}
	return strings.Join(parts, sep)
}

func formatDate(format string, now time.Time) string {
	switch strings.ToUpper(format) {
	case "YYYYMMDD":
		return now.Format("20060102")
	case "YYMMDD":
		return now.Format("060102")
	case "MMYY":
		return now.Format("0106")
	case "YYYY":
		return now.Format("2006")
	default:
		return ""
	}
}

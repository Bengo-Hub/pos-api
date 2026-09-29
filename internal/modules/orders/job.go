package orders

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

// A services job order (order_subtype service_job) keeps its job header in
// pos_orders.metadata["job"]:
//
//	due_at               RFC 3339 time the customer expects to collect
//	brief                free-text instructions for the whole job
//	design_from_scratch  the customer wants the business to design the artwork
//	attachments          [{kind: link|file, url, label}] reference media
//	stage                current production stage (a key from the outlet's service profile)
//	proof_status         none | sent | approved | changes_requested
//	stage_history        [{stage, at, by}] audit of stage moves
//	collected_at         when the customer collected the finished job
//
// Per-line spec sheets live in pos_order_lines.metadata["job_specs"].

// Job limits. Attachments are links or small uploads (the upload endpoint caps each file at
// 512 KB); these caps keep one order's metadata bounded.
const (
	SubtypeServiceJob     = "service_job"
	MaxJobFileAttachments = 5
	MaxJobLinkAttachments = 10
	maxJobBriefLen        = 4000
	maxAttachmentLabelLen = 120
	maxAttachmentURLLen   = 2048
)

// Proof statuses.
const (
	ProofNone             = "none"
	ProofSent             = "sent"
	ProofApproved         = "approved"
	ProofChangesRequested = "changes_requested"
)

var validProofStatuses = map[string]bool{
	ProofNone: true, ProofSent: true, ProofApproved: true, ProofChangesRequested: true,
}

// ErrInvalidJob is returned for a job update the order or profile does not allow.
var ErrInvalidJob = errors.New("invalid job update")

// JobAttachment is one piece of reference media on a job.
type JobAttachment struct {
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

// JobUpdate is a partial update of a job header. Nil fields are left unchanged.
type JobUpdate struct {
	DueAt             *time.Time
	Brief             *string
	DesignFromScratch *bool
	Attachments       *[]JobAttachment
	Stage             *string
	ProofStatus       *string
	Collected         *bool
}

// sanitizeAttachments validates and trims attachments. A link must be an absolute http(s) URL;
// an uploaded file must be a same-platform media path or an http(s) URL returned by the upload
// endpoint. Invalid entries are rejected rather than silently dropped so the caller sees why.
func sanitizeAttachments(in []JobAttachment) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(in))
	files, links := 0, 0
	for _, a := range in {
		kind := strings.ToLower(strings.TrimSpace(a.Kind))
		raw := strings.TrimSpace(a.URL)
		if raw == "" {
			continue
		}
		if len(raw) > maxAttachmentURLLen {
			return nil, fmt.Errorf("%w: attachment url is too long", ErrInvalidJob)
		}
		switch kind {
		case "link":
			links++
			if !isHTTPURL(raw) {
				return nil, fmt.Errorf("%w: attachment link must start with http:// or https://", ErrInvalidJob)
			}
		case "file":
			files++
			if !isHTTPURL(raw) && !strings.HasPrefix(raw, "/media/") {
				return nil, fmt.Errorf("%w: uploaded file url is not a media url", ErrInvalidJob)
			}
		default:
			return nil, fmt.Errorf("%w: attachment kind must be link or file", ErrInvalidJob)
		}
		label := strings.TrimSpace(a.Label)
		if len(label) > maxAttachmentLabelLen {
			label = label[:maxAttachmentLabelLen]
		}
		entry := map[string]any{"kind": kind, "url": raw}
		if label != "" {
			entry["label"] = label
		}
		out = append(out, entry)
	}
	if files > MaxJobFileAttachments {
		return nil, fmt.Errorf("%w: at most %d uploaded files per job", ErrInvalidJob, MaxJobFileAttachments)
	}
	if links > MaxJobLinkAttachments {
		return nil, fmt.Errorf("%w: at most %d links per job", ErrInvalidJob, MaxJobLinkAttachments)
	}
	return out, nil
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// attachmentsFromAny converts the loosely-typed attachments a client sends in order metadata.
func attachmentsFromAny(v any) []JobAttachment {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]JobAttachment, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		a := JobAttachment{}
		a.Kind, _ = m["kind"].(string)
		a.URL, _ = m["url"].(string)
		a.Label, _ = m["label"].(string)
		out = append(out, a)
	}
	return out
}

// NormalizeNewJob validates the job header a client sent when creating a service_job order and
// fills in the defaults: the first production stage of the outlet's profile, proof status
// "none", and the first stage_history entry. Unknown keys are dropped.
func NormalizeNewJob(raw map[string]any, profile *outletpolicy.ServiceProfile, actorID uuid.UUID, now time.Time) (map[string]any, error) {
	job := map[string]any{}
	if raw == nil {
		raw = map[string]any{}
	}
	if s, _ := raw["due_at"].(string); strings.TrimSpace(s) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%w: due_at must be an RFC 3339 time", ErrInvalidJob)
		}
		job["due_at"] = t.UTC().Format(time.RFC3339)
	}
	if s, _ := raw["brief"].(string); strings.TrimSpace(s) != "" {
		brief := strings.TrimSpace(s)
		if len(brief) > maxJobBriefLen {
			return nil, fmt.Errorf("%w: brief is longer than %d characters", ErrInvalidJob, maxJobBriefLen)
		}
		job["brief"] = brief
	}
	if b, _ := raw["design_from_scratch"].(bool); b {
		job["design_from_scratch"] = true
	}
	atts, err := sanitizeAttachments(attachmentsFromAny(raw["attachments"]))
	if err != nil {
		return nil, err
	}
	if len(atts) > 0 {
		job["attachments"] = atts
	}
	stage := ""
	if profile != nil && len(profile.Stages) > 0 {
		stage = profile.Stages[0].Key
		job["profile"] = profile.Key
	}
	if stage != "" {
		job["stage"] = stage
	}
	job["proof_status"] = ProofNone
	entry := map[string]any{"stage": stage, "at": now.UTC().Format(time.RFC3339)}
	if actorID != uuid.Nil {
		entry["by"] = actorID.String()
	}
	job["stage_history"] = []any{entry}
	return job, nil
}

// ApplyJobUpdate merges upd into the existing job header and returns the new header plus a
// compact change summary for the audit event. It never mutates existing.
func ApplyJobUpdate(existing map[string]any, upd JobUpdate, profile *outletpolicy.ServiceProfile, actorID uuid.UUID, now time.Time) (map[string]any, map[string]any, error) {
	job := make(map[string]any, len(existing)+4)
	for k, v := range existing {
		job[k] = v
	}
	changes := map[string]any{}
	if upd.DueAt != nil {
		v := upd.DueAt.UTC().Format(time.RFC3339)
		job["due_at"] = v
		changes["due_at"] = v
	}
	if upd.Brief != nil {
		brief := strings.TrimSpace(*upd.Brief)
		if len(brief) > maxJobBriefLen {
			return nil, nil, fmt.Errorf("%w: brief is longer than %d characters", ErrInvalidJob, maxJobBriefLen)
		}
		job["brief"] = brief
		changes["brief"] = true
	}
	if upd.DesignFromScratch != nil {
		job["design_from_scratch"] = *upd.DesignFromScratch
		changes["design_from_scratch"] = *upd.DesignFromScratch
	}
	if upd.Attachments != nil {
		atts, err := sanitizeAttachments(*upd.Attachments)
		if err != nil {
			return nil, nil, err
		}
		job["attachments"] = atts
		changes["attachments"] = len(atts)
	}
	if upd.ProofStatus != nil {
		ps := strings.ToLower(strings.TrimSpace(*upd.ProofStatus))
		if !validProofStatuses[ps] {
			return nil, nil, fmt.Errorf("%w: proof_status must be none, sent, approved or changes_requested", ErrInvalidJob)
		}
		job["proof_status"] = ps
		changes["proof_status"] = ps
	}
	if upd.Stage != nil {
		stage := strings.ToLower(strings.TrimSpace(*upd.Stage))
		if stage == "" {
			return nil, nil, fmt.Errorf("%w: stage is empty", ErrInvalidJob)
		}
		if profile != nil && len(profile.Stages) > 0 && !profile.HasStage(stage) {
			return nil, nil, fmt.Errorf("%w: %q is not a stage of %s", ErrInvalidJob, stage, profile.Label)
		}
		if cur, _ := job["stage"].(string); cur != stage {
			job["stage"] = stage
			changes["stage"] = stage
			entry := map[string]any{"stage": stage, "at": now.UTC().Format(time.RFC3339)}
			if actorID != uuid.Nil {
				entry["by"] = actorID.String()
			}
			history, _ := job["stage_history"].([]any)
			next := make([]any, 0, len(history)+1)
			next = append(next, history...)
			job["stage_history"] = append(next, entry)
		}
	}
	if upd.Collected != nil && *upd.Collected {
		if _, done := job["collected_at"]; !done {
			v := now.UTC().Format(time.RFC3339)
			job["collected_at"] = v
			changes["collected_at"] = v
		}
	}
	return job, changes, nil
}

// OutletServiceProfile loads the service profile configured on an outlet, if any.
func (s *Service) OutletServiceProfile(ctx context.Context, outletID uuid.UUID) *outletpolicy.ServiceProfile {
	setting, err := s.client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Only(ctx)
	if err != nil {
		return nil
	}
	if p, ok := outletpolicy.ServiceProfileFromMetadata(setting.Metadata); ok {
		return &p
	}
	return nil
}

// UpdateJob applies a partial job update to a service_job order and records an audit event.
// Only open or ready-for-payment jobs (and completed ones, for collection) can change.
func (s *Service) UpdateJob(ctx context.Context, tenantID, orderID, actorID uuid.UUID, upd JobUpdate) (map[string]any, error) {
	order, err := s.client.POSOrder.Query().
		Where(posorder.ID(orderID), posorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("orders: order not found: %w", err)
	}
	if string(order.OrderSubtype) != SubtypeServiceJob {
		return nil, fmt.Errorf("%w: order %s is not a job order", ErrInvalidJob, order.OrderNumber)
	}
	existing, _ := order.Metadata["job"].(map[string]any)
	switch order.Status {
	case StatusOpen, StatusPendingPayment, StatusAwaitingAcceptance:
	case StatusCompleted:
		// A job paid in full up front is still in production; it stays editable until the
		// customer collects it.
		if _, collected := existing["collected_at"]; collected {
			return nil, fmt.Errorf("%w: job was already collected", ErrInvalidJob)
		}
	default:
		return nil, fmt.Errorf("%w: job is %s", ErrInvalidJob, order.Status)
	}
	profile := s.OutletServiceProfile(ctx, order.OutletID)
	job, changes, err := ApplyJobUpdate(existing, upd, profile, actorID, time.Now())
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return job, nil
	}
	meta := make(map[string]any, len(order.Metadata)+1)
	for k, v := range order.Metadata {
		meta[k] = v
	}
	meta["job"] = job
	if err := s.client.POSOrder.UpdateOneID(order.ID).SetMetadata(meta).Exec(ctx); err != nil {
		return nil, fmt.Errorf("orders: save job: %w", err)
	}
	create := s.client.POSOrderEvent.Create().
		SetOrderID(order.ID).
		SetEventType("job_updated").
		SetPayload(changes)
	if actorID != uuid.Nil {
		create.SetActorID(actorID)
	}
	if _, err := create.Save(ctx); err != nil {
		s.log.Warn("record job event failed", zap.String("order_id", order.ID.String()), zap.Error(err))
	}
	// Reaching the final stage (ready for collection) finishes production: the job's tickets
	// leave the production board and an unpaid or part-paid job moves to the cashier's
	// ready-for-payment queue, the same hand-off a served kitchen order makes.
	if stage, moved := changes["stage"].(string); moved && profile != nil && isFinalStage(profile, stage) {
		s.AutoClearKDSTicketsForOrder(ctx, tenantID, order.ID)
		if _, err := s.client.POSOrder.Update().
			Where(posorder.ID(order.ID), posorder.Status(StatusOpen)).
			SetStatus(StatusPendingPayment).
			Save(ctx); err != nil {
			s.log.Warn("move finished job to pending_payment failed", zap.String("order_id", order.ID.String()), zap.Error(err))
		}
	}
	return job, nil
}

// isFinalStage reports whether stage is the last stage of the profile's pipeline.
func isFinalStage(profile *outletpolicy.ServiceProfile, stage string) bool {
	return len(profile.Stages) > 0 && profile.Stages[len(profile.Stages)-1].Key == stage
}

// IsServiceJob reports whether an order is a services job order.
func IsServiceJob(subtype string) bool {
	return subtype == SubtypeServiceJob
}

// JobSummary is the services jobs dashboard: what is in production, what is waiting for the
// customer, what is late, and the money still to collect on unfinished jobs.
type JobSummary struct {
	InProduction       int     `json:"in_production"`
	ReadyForCollection int     `json:"ready_for_collection"`
	DueToday           int     `json:"due_today"`
	Overdue            int     `json:"overdue"`
	AwaitingProof      int     `json:"awaiting_proof"`
	BalanceOutstanding float64 `json:"balance_outstanding"`
	DepositsHeld       float64 `json:"deposits_held"`
}

// uncollectedJobWindow bounds how far back a paid-but-never-marked-collected job still counts as
// waiting for collection, so an outlet that never taps "Collected" cannot grow the scan forever.
const uncollectedJobWindow = 90 * 24 * time.Hour

// JobSummary aggregates the active service_job orders of an outlet. It reads only job rows in
// open / pending_payment status, plus paid jobs from the last 90 days that were never collected,
// through the posorder_service_jobs partial index, and only the columns it needs.
func (s *Service) JobSummary(ctx context.Context, tenantID, outletID uuid.UUID, loc *time.Location, now time.Time) (JobSummary, error) {
	var out JobSummary
	rows, err := s.client.POSOrder.Query().
		Where(
			posorder.TenantID(tenantID),
			posorder.OutletID(outletID),
			posorder.OrderSubtypeEQ(posorder.OrderSubtypeServiceJob),
			posorder.Or(
				posorder.StatusIn(StatusOpen, StatusPendingPayment),
				posorder.And(posorder.Status(StatusCompleted), posorder.CreatedAtGTE(now.Add(-uncollectedJobWindow))),
			),
		).
		Select(posorder.FieldStatus, posorder.FieldTotalAmount, posorder.FieldPaidTotal, posorder.FieldMetadata).
		All(ctx)
	if err != nil {
		return out, fmt.Errorf("orders: job summary: %w", err)
	}
	if loc == nil {
		loc = time.UTC
	}
	today := now.In(loc)
	dayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.Add(24 * time.Hour)
	for _, o := range rows {
		job, _ := o.Metadata["job"].(map[string]any)
		if o.Status == StatusCompleted {
			if _, collected := job["collected_at"]; collected {
				continue
			}
			out.ReadyForCollection++
			continue
		}
		balance := o.TotalAmount - o.PaidTotal
		if balance > 0 {
			out.BalanceOutstanding += balance
		}
		out.DepositsHeld += o.PaidTotal
		if o.Status == StatusPendingPayment {
			out.ReadyForCollection++
			continue
		}
		out.InProduction++
		if ps, _ := job["proof_status"].(string); ps == ProofSent {
			out.AwaitingProof++
		}
		if raw, _ := job["due_at"].(string); raw != "" {
			if due, perr := time.Parse(time.RFC3339, raw); perr == nil {
				switch {
				case due.Before(now):
					out.Overdue++
				case !due.Before(dayStart) && due.Before(dayEnd):
					out.DueToday++
				}
			}
		}
	}
	return out, nil
}

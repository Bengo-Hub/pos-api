package orders

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

func printingProfile(t *testing.T) *outletpolicy.ServiceProfile {
	t.Helper()
	p, ok := outletpolicy.LookupServiceProfile(outletpolicy.ProfilePrintingBranding)
	if !ok {
		t.Fatal("printing profile missing")
	}
	return &p
}

func TestServiceJobIsTicketedAndValid(t *testing.T) {
	if !isTicketedSubtype(SubtypeServiceJob) {
		t.Fatal("service_job must open with production tickets")
	}
	if _, ok := validOrderSubtypes[SubtypeServiceJob]; !ok {
		t.Fatal("service_job must be an accepted order subtype")
	}
	if isTicketedSubtype("retail") {
		t.Fatal("retail must stay a draft until paid")
	}
}

func TestNormalizeNewJobDefaults(t *testing.T) {
	now := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	actor := uuid.New()
	job, err := NormalizeNewJob(map[string]any{
		"due_at":              "2026-09-30T15:00:00+03:00",
		"brief":               "  500 business cards, logo attached  ",
		"design_from_scratch": false,
		"attachments": []any{
			map[string]any{"kind": "link", "url": "https://drive.google.com/file/d/abc", "label": "Logo"},
			map[string]any{"kind": "file", "url": "/media/uploads/attachments/t/x.pdf"},
		},
		"unknown_key": "dropped",
	}, printingProfile(t), actor, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job["due_at"] != "2026-09-30T12:00:00Z" {
		t.Errorf("due_at not normalized to UTC: %v", job["due_at"])
	}
	if job["brief"] != "500 business cards, logo attached" {
		t.Errorf("brief not trimmed: %q", job["brief"])
	}
	if _, ok := job["design_from_scratch"]; ok {
		t.Error("false design_from_scratch should not be stored")
	}
	if job["stage"] != "design" || job["profile"] != outletpolicy.ProfilePrintingBranding {
		t.Errorf("first stage/profile not stamped: %v %v", job["stage"], job["profile"])
	}
	if job["proof_status"] != ProofNone {
		t.Errorf("proof_status = %v", job["proof_status"])
	}
	if _, ok := job["unknown_key"]; ok {
		t.Error("unknown keys must be dropped")
	}
	atts := job["attachments"].([]map[string]any)
	if len(atts) != 2 || atts[0]["label"] != "Logo" {
		t.Errorf("attachments wrong: %+v", atts)
	}
	hist := job["stage_history"].([]any)
	if len(hist) != 1 || hist[0].(map[string]any)["by"] != actor.String() {
		t.Errorf("stage_history wrong: %+v", hist)
	}
}

func TestNormalizeNewJobWithoutProfile(t *testing.T) {
	job, err := NormalizeNewJob(nil, nil, uuid.Nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := job["stage"]; ok {
		t.Error("no profile means no stage")
	}
	if job["proof_status"] != ProofNone {
		t.Error("proof_status must default to none")
	}
}

func TestNormalizeNewJobRejectsBadInput(t *testing.T) {
	bad := []map[string]any{
		{"due_at": "tomorrow"},
		{"attachments": []any{map[string]any{"kind": "link", "url": "javascript:alert(1)"}}},
		{"attachments": []any{map[string]any{"kind": "link", "url": "ftp://files.example.com/a"}}},
		{"attachments": []any{map[string]any{"kind": "file", "url": "C:/Users/logo.png"}}},
		{"attachments": []any{map[string]any{"kind": "video", "url": "https://youtube.com/x"}}},
	}
	for i, raw := range bad {
		if _, err := NormalizeNewJob(raw, nil, uuid.Nil, time.Now()); !errors.Is(err, ErrInvalidJob) {
			t.Errorf("case %d: want ErrInvalidJob, got %v", i, err)
		}
	}
	files := make([]any, 0, MaxJobFileAttachments+1)
	for i := 0; i <= MaxJobFileAttachments; i++ {
		files = append(files, map[string]any{"kind": "file", "url": "/media/uploads/attachments/a.png"})
	}
	if _, err := NormalizeNewJob(map[string]any{"attachments": files}, nil, uuid.Nil, time.Now()); !errors.Is(err, ErrInvalidJob) {
		t.Errorf("more than %d files must be rejected, got %v", MaxJobFileAttachments, err)
	}
	long := make([]byte, maxJobBriefLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := NormalizeNewJob(map[string]any{"brief": string(long)}, nil, uuid.Nil, time.Now()); !errors.Is(err, ErrInvalidJob) {
		t.Errorf("an over-long brief must be rejected, got %v", err)
	}
}

func TestApplyJobUpdateStagesAndProof(t *testing.T) {
	p := printingProfile(t)
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	existing, _ := NormalizeNewJob(nil, p, uuid.Nil, now)

	stage := "print"
	proof := "approved"
	job, changes, err := ApplyJobUpdate(existing, JobUpdate{Stage: &stage, ProofStatus: &proof}, p, uuid.Nil, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if job["stage"] != "print" || changes["stage"] != "print" || changes["proof_status"] != "approved" {
		t.Errorf("stage/proof not applied: job=%v changes=%v", job, changes)
	}
	if n := len(job["stage_history"].([]any)); n != 2 {
		t.Errorf("stage history length = %d, want 2", n)
	}
	if existing["stage"] != "design" {
		t.Error("ApplyJobUpdate must not mutate the existing header")
	}

	// Same stage again: no new history entry, no change reported.
	job2, changes2, err := ApplyJobUpdate(job, JobUpdate{Stage: &stage}, p, uuid.Nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes2) != 0 || len(job2["stage_history"].([]any)) != 2 {
		t.Errorf("repeating a stage must be a no-op: %v", changes2)
	}

	bogus := "washing"
	if _, _, err := ApplyJobUpdate(job, JobUpdate{Stage: &bogus}, p, uuid.Nil, now); !errors.Is(err, ErrInvalidJob) {
		t.Errorf("a stage from another trade must be rejected, got %v", err)
	}
	badProof := "maybe"
	if _, _, err := ApplyJobUpdate(job, JobUpdate{ProofStatus: &badProof}, p, uuid.Nil, now); !errors.Is(err, ErrInvalidJob) {
		t.Errorf("an unknown proof status must be rejected, got %v", err)
	}
}

func TestApplyJobUpdateCollectedOnce(t *testing.T) {
	yes := true
	first := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	job, changes, err := ApplyJobUpdate(map[string]any{}, JobUpdate{Collected: &yes}, nil, uuid.Nil, first)
	if err != nil || job["collected_at"] != "2026-09-29T10:00:00Z" || changes["collected_at"] == nil {
		t.Fatalf("collected not stamped: %v %v %v", job, changes, err)
	}
	job2, changes2, _ := ApplyJobUpdate(job, JobUpdate{Collected: &yes}, nil, uuid.Nil, first.Add(time.Hour))
	if job2["collected_at"] != "2026-09-29T10:00:00Z" || len(changes2) != 0 {
		t.Error("collected_at must keep the first collection time")
	}
}

func TestApplyJobUpdateAttachmentsReplace(t *testing.T) {
	atts := []JobAttachment{{Kind: "link", URL: "https://youtu.be/abc", Label: "Reference video"}}
	job, changes, err := ApplyJobUpdate(map[string]any{"attachments": []any{"old"}}, JobUpdate{Attachments: &atts}, nil, uuid.Nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := job["attachments"].([]map[string]any)
	if len(got) != 1 || got[0]["url"] != "https://youtu.be/abc" || changes["attachments"] != 1 {
		t.Errorf("attachments not replaced: %v", got)
	}
}

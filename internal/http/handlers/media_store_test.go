package handlers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func pngUpload(t *testing.T, tenantID string, payload []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "photo.png")
	_, _ = fw.Write(payload)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r.WithContext(httpware.WithTenantID(r.Context(), tenantID))
}

func testPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 6, 6))
	img.Set(2, 2, color.NRGBA{0, 0, 255, 255})
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// Damage evidence is private: stored under the tenant, handed out as a signed link, stored back as
// its key when the client sends that link, and signed again on read.
func TestEvidenceUploadPrivate(t *testing.T) {
	RegisterMediaTypes()
	root := t.TempDir()
	store, err := objectstore.New("pos", objectstore.Config{LocalRoot: root, LocalURLBase: "http://localhost:4106"},
		objectstore.WithLocalSigner(httpware.NewMediaSigner("test-secret", time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	h := &HotelHandler{log: zap.NewNop(), media: store}
	tenant := uuid.NewString()

	rec := httptest.NewRecorder()
	h.UploadDamageEvidence(rec, pngUpload(t, tenant, testPNG()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ URL, Key string }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !strings.HasPrefix(got.Key, "pos/"+tenant+"/evidence/damage/") {
		t.Fatalf("key %q", got.Key)
	}
	if !strings.Contains(got.URL, "sig=") {
		t.Errorf("private link must be signed: %q", got.URL)
	}
	keys := mediaKeys(store, []string{got.URL, "https://elsewhere.example/p.png"})
	if keys[0] != got.Key || keys[1] != "https://elsewhere.example/p.png" {
		t.Errorf("links sent back must store as keys: %v", keys)
	}
	if links := mediaLinks(store, keys); !strings.Contains(links[0], "sig=") {
		t.Errorf("read must sign again: %v", links)
	}

	// A file that is not an image is refused.
	rec = httptest.NewRecorder()
	h.UploadDamageEvidence(rec, pngUpload(t, tenant, []byte("%PDF-1.4 not a photo")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-image evidence: %d", rec.Code)
	}
}

// Job attachments are public with unguessable names and allow PDFs up to 512 KB.
func TestJobAttachmentLimits(t *testing.T) {
	RegisterMediaTypes()
	store, _ := objectstore.New("pos", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost:4106"})
	h := &POSOrderHandler{log: zap.NewNop(), media: store}
	tenant := uuid.NewString()

	rec := httptest.NewRecorder()
	h.UploadJobAttachment(rec, pngUpload(t, tenant, []byte("%PDF-1.4 brief")))
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "/media/pos/"+tenant+"/job-attachments/jobs/") {
		t.Fatalf("pdf attachment: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.UploadJobAttachment(rec, pngUpload(t, tenant, append([]byte("%PDF-1.4 "), make([]byte, 600<<10)...)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("over 512 KB must be refused: %d", rec.Code)
	}
}

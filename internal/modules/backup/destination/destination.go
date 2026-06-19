// Package destination implements pluggable, encrypted-at-rest backup destinations
// for the pos backup module. The local PVC copy produced by the backup service is
// ALWAYS the primary, durable store; a configured remote destination (OneDrive /
// Google Drive / S3 / WebDAV / SFTP / SMB) is an additional, mirrored copy uploaded
// best-effort via rclone (see uploader.go).
//
// Configuration is stored in the existing ServiceConfig key/value store under the
// key "backup_destination": a platform-level default (tenant_id IS NULL) plus
// optional per-tenant overrides. The serialized Destination JSON is encrypted at
// rest with AES-256-GCM using a SECRET_KEY-derived Cipher, so secret backend params
// (access keys, tokens, passwords) are never stored or logged in plaintext.
//
// This package is intentionally self-contained so it can be lifted into the other
// Go services that share the ServiceConfig pattern.
package destination

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/serviceconfig"
)

// Cipher supplies the AES-256 key material used to encrypt destination configs at
// rest. It is deliberately a tiny interface so this package stays self-contained
// and portable across services: treasury passes its gateways.KeyProvider; services
// without a KeyProvider (like pos) pass a SECRET_KEY-derived static key (see
// NewSecretKeyCipher).
type Cipher interface {
	// PrimaryKey returns the 32-byte key used to encrypt NEW data.
	PrimaryKey(ctx context.Context) []byte
	// CandidateKeys returns all decryption keys in priority order (PrimaryKey
	// first) so data written under an older key still decrypts after a rotation.
	CandidateKeys(ctx context.Context) [][]byte
}

// secretKeyCipher is a Cipher derived deterministically from the SECRET_KEY env
// var: sha256(SECRET_KEY) → a fixed 32-byte AES-256 key. pos-api has no
// KeyProvider, so this keeps the encrypt/decrypt scheme identical to treasury's
// while sourcing key material from the service's existing SECRET_KEY.
type secretKeyCipher struct {
	key [32]byte
}

// NewSecretKeyCipher builds a Cipher whose key is sha256(os.Getenv("SECRET_KEY")).
// SECRET_KEY is always present in the pos-api runtime (it backs JWT/HMAC signing),
// so the derived AES key is stable across restarts and pods.
func NewSecretKeyCipher() Cipher {
	return &secretKeyCipher{key: sha256.Sum256([]byte(os.Getenv("SECRET_KEY")))}
}

func (c *secretKeyCipher) PrimaryKey(context.Context) []byte {
	k := c.key
	return k[:]
}

func (c *secretKeyCipher) CandidateKeys(ctx context.Context) [][]byte {
	return [][]byte{c.PrimaryKey(ctx)}
}

// ConfigKey is the ServiceConfig.config_key under which the (encrypted) backup
// destination JSON is stored, at both platform (tenant_id NULL) and tenant scope.
const ConfigKey = "backup_destination"

// Type enumerates the supported backup destination backends. "pvc" is the
// always-present local copy and never has a remote; all other types are rclone
// backends mirrored to in addition to the PVC.
type Type string

const (
	// TypePVC is the local persistent-volume copy. It is the durable primary
	// store and the fallback when no remote is configured or a mirror fails.
	TypePVC Type = "pvc"
	// TypeS3 is any S3-compatible object store (AWS S3, MinIO, Wasabi, R2, ...).
	TypeS3 Type = "s3"
	// TypeOneDrive is Microsoft OneDrive (rclone "onedrive" backend).
	TypeOneDrive Type = "onedrive"
	// TypeGDrive is Google Drive (rclone "drive" backend).
	TypeGDrive Type = "gdrive"
	// TypeWebDAV is any WebDAV server (Nextcloud, ownCloud, generic).
	TypeWebDAV Type = "webdav"
	// TypeSFTP is an SFTP/SSH file server.
	TypeSFTP Type = "sftp"
	// TypeSMB is an SMB / Windows network folder share.
	TypeSMB Type = "smb"
)

// Destination is the resolved backup destination configuration. Params carries
// the rclone backend parameters keyed by this package's stable param-key names
// (see the per-type SECRET/required key sets below); the resolver/uploader map
// them onto rclone RCLONE_CONFIG_* env vars.
type Destination struct {
	// Type is the backend kind (pvc, s3, onedrive, gdrive, webdav, sftp, smb).
	Type Type `json:"type"`
	// Enabled gates remote mirroring. A disabled (or pvc) destination mirrors
	// nothing — the PVC copy still stands as the durable store.
	Enabled bool `json:"enabled"`
	// RemotePath is the destination-side directory/prefix backups are written
	// under (e.g. "pos/backups", an S3 key prefix, or a Drive folder path).
	RemotePath string `json:"remote_path,omitempty"`
	// Params are the rclone backend parameters. Secret keys (see SecretParamKeys)
	// are never logged or returned in API responses; non-secret keys may be shown.
	Params map[string]string `json:"params,omitempty"`
}

// PVCOnly returns the inert destination used when nothing is configured or the
// configured destination is disabled: PVC only, no remote mirroring.
func PVCOnly() Destination {
	return Destination{Type: TypePVC, Enabled: false}
}

// IsRemote reports whether this destination should be mirrored to (a non-pvc,
// enabled backend). PVC-only / disabled destinations return false.
func (d Destination) IsRemote() bool {
	return d.Enabled && d.Type != TypePVC && d.Type != ""
}

// secretParamKeysByType lists, per backend type, the Param keys that hold secret
// material. These are masked in API responses and NEVER logged. Kept conservative
// (a superset) so a key is masked even if only used by some providers.
var secretParamKeysByType = map[Type][]string{
	TypeS3:       {"secret_access_key", "access_key_id", "session_token"},
	TypeOneDrive: {"token", "client_secret"},
	TypeGDrive:   {"token", "client_secret", "service_account_credentials"},
	TypeWebDAV:   {"pass", "bearer_token"},
	TypeSFTP:     {"pass", "key_pem", "key_file_pass"},
	TypeSMB:      {"pass"},
}

// requiredParamKeysByType lists the Param keys that MUST be present (non-empty)
// for a given backend type to be storable. Validation is intentionally lenient
// beyond these so unusual rclone setups (e.g. S3 with an IAM role, env auth) are
// still expressible by the operator.
var requiredParamKeysByType = map[Type][]string{
	TypeS3:       {"bucket"},
	TypeOneDrive: {"token"},
	TypeGDrive:   {"token"},
	TypeWebDAV:   {"url", "user"},
	TypeSFTP:     {"host", "user"},
	TypeSMB:      {"host", "user"},
}

// SupportedTypes returns all valid destination types (excluding pvc, which is
// implicit and not operator-selectable as a remote). Order is stable for UIs.
func SupportedTypes() []Type {
	return []Type{TypeS3, TypeOneDrive, TypeGDrive, TypeWebDAV, TypeSFTP, TypeSMB}
}

// ValidType reports whether t is a known, operator-selectable remote type.
func ValidType(t Type) bool {
	for _, s := range SupportedTypes() {
		if s == t {
			return true
		}
	}
	return false
}

// SecretParamKeys returns the secret param keys for a type (nil for unknown/pvc).
func SecretParamKeys(t Type) []string { return secretParamKeysByType[t] }

// IsSecretParam reports whether key is secret for the given destination type.
func (d Destination) IsSecretParam(key string) bool {
	for _, k := range secretParamKeysByType[d.Type] {
		if k == key {
			return true
		}
	}
	return false
}

// Validate checks the type is supported and all required params are present.
// It never inspects/echoes secret VALUES — only presence. Returns a sanitized,
// user-safe error.
func (d Destination) Validate() error {
	if !ValidType(d.Type) {
		return fmt.Errorf("unsupported backup destination type: %q", d.Type)
	}
	for _, k := range requiredParamKeysByType[d.Type] {
		if d.Params[k] == "" {
			return fmt.Errorf("missing required param %q for type %q", k, d.Type)
		}
	}
	return nil
}

// MaskedParam is the API-safe view of a single param: secret params report only
// whether a value is set; non-secret params expose their plaintext value.
type MaskedParam struct {
	Key      string `json:"key"`
	IsSecret bool   `json:"is_secret"`
	Set      bool   `json:"set"`             // true if a (non-empty) value is present
	Value    string `json:"value,omitempty"` // populated for non-secret params only
}

// MaskedParams returns the params as a stable, masked list suitable for direct
// inclusion in an API response. Secret values are reduced to a Set boolean.
func (d Destination) MaskedParams() []MaskedParam {
	out := make([]MaskedParam, 0, len(d.Params))
	for k, v := range d.Params {
		secret := d.IsSecretParam(k)
		mp := MaskedParam{Key: k, IsSecret: secret, Set: v != ""}
		if !secret {
			mp.Value = v
		}
		out = append(out, mp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Store reads/writes encrypted Destination configs in ServiceConfig. It is the
// single owner of the encrypt/decrypt + upsert logic, mirroring the gateways
// resolver's AES-256-GCM scheme (encrypt with the PRIMARY key; decrypt by trying
// each candidate key in priority order).
type Store struct {
	client *ent.Client
	keys   Cipher
	logger *zap.Logger
}

// NewStore constructs a Store over the ent client and a Cipher (e.g. a
// SECRET_KEY-derived key). The Cipher provides the encrypt/decrypt key material.
func NewStore(client *ent.Client, keys Cipher, logger *zap.Logger) *Store {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Store{client: client, keys: keys, logger: logger.Named("backup.destination")}
}

// Resolve returns the effective destination for a tenant: the tenant override
// (tenant_id = tenantID) if present, else the platform default (tenant_id NULL),
// else PVCOnly. A disabled stored destination resolves to PVCOnly so callers
// uniformly treat "no remote" the same way. Credentials are never logged.
func (s *Store) Resolve(ctx context.Context, tenantID uuid.UUID) Destination {
	// 1. Tenant-specific override.
	if tenantID != uuid.Nil {
		if d, ok := s.load(ctx, &tenantID); ok {
			if d.Enabled {
				return d
			}
			// An explicit disabled tenant override means "this tenant opts out of
			// any remote mirror" — do NOT fall through to the platform default.
			return PVCOnly()
		}
	}
	// 2. Platform default (tenant_id IS NULL).
	if d, ok := s.load(ctx, nil); ok && d.Enabled {
		return d
	}
	// 3. Nothing configured / disabled.
	return PVCOnly()
}

// Get returns the stored destination at the exact requested scope (tenant
// override when tenantID != nil, else platform default) WITHOUT inheritance,
// for the management endpoints. ok=false when no row exists at that scope.
func (s *Store) Get(ctx context.Context, tenantID *uuid.UUID) (Destination, bool) {
	return s.load(ctx, tenantID)
}

// load reads and decrypts the destination row at the given scope. tenantID nil =
// platform default. Returns ok=false when absent or unreadable (never panics,
// never logs credential material).
func (s *Store) load(ctx context.Context, tenantID *uuid.UUID) (Destination, bool) {
	q := s.client.ServiceConfig.Query().Where(serviceconfig.ConfigKey(ConfigKey))
	if tenantID == nil {
		q = q.Where(serviceconfig.TenantIDIsNil())
	} else {
		q = q.Where(serviceconfig.TenantID(*tenantID))
	}
	row, err := q.First(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			s.logger.Warn("failed to load backup destination config", zap.Error(err))
		}
		return Destination{}, false
	}
	d, err := s.decrypt(ctx, row.ConfigValue)
	if err != nil {
		// Never include the ciphertext or any value in the log.
		s.logger.Warn("failed to decrypt backup destination config", zap.Error(err))
		return Destination{}, false
	}
	return d, true
}

// Store encrypts and upserts the destination at the given scope (tenantID nil =
// platform default). config_type is "json" and is_secret is true so the value is
// masked by the generic settings endpoints as well.
func (s *Store) Store(ctx context.Context, tenantID *uuid.UUID, d Destination) error {
	enc, err := s.encrypt(ctx, d)
	if err != nil {
		return fmt.Errorf("encrypt destination: %w", err)
	}

	q := s.client.ServiceConfig.Query().Where(serviceconfig.ConfigKey(ConfigKey))
	if tenantID == nil {
		q = q.Where(serviceconfig.TenantIDIsNil())
	} else {
		q = q.Where(serviceconfig.TenantID(*tenantID))
	}
	existing, err := q.First(ctx)
	if err == nil {
		_, uErr := existing.Update().
			SetConfigValue(enc).
			SetConfigType("json").
			SetIsSecret(true).
			SetDescription("Encrypted pluggable backup destination (rclone) config").
			Save(ctx)
		return uErr
	}

	create := s.client.ServiceConfig.Create().
		SetConfigKey(ConfigKey).
		SetConfigValue(enc).
		SetConfigType("json").
		SetIsSecret(true).
		SetDescription("Encrypted pluggable backup destination (rclone) config")
	if tenantID != nil {
		create = create.SetTenantID(*tenantID)
	}
	_, cErr := create.Save(ctx)
	return cErr
}

// encrypt marshals + AES-256-GCM-encrypts a Destination with the PRIMARY key and
// returns base64(nonce||ciphertext). Mirrors gateways.EncryptCredentials.
func (s *Store) encrypt(ctx context.Context, d Destination) (string, error) {
	plaintext, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshal destination: %w", err)
	}
	key := s.keys.PrimaryKey(ctx)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decrypt base64-decodes + AES-256-GCM-decrypts, trying each candidate key in
// priority order so configs written under an older key still decrypt after a
// rotation. Falls back to plain JSON for dev/migration data. Mirrors
// gateways.decryptCredentials.
func (s *Store) decrypt(ctx context.Context, encrypted string) (Destination, error) {
	var d Destination
	if encrypted == "" {
		return d, fmt.Errorf("empty destination value")
	}
	data, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		// Might be plain JSON (dev/migration data).
		if jErr := json.Unmarshal([]byte(encrypted), &d); jErr == nil {
			return d, nil
		}
		return d, fmt.Errorf("decode destination: %w", err)
	}

	for _, key := range s.keys.CandidateKeys(ctx) {
		block, err := aes.NewCipher(key)
		if err != nil {
			continue
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			continue
		}
		nonceSize := gcm.NonceSize()
		if len(data) < nonceSize {
			break // shorter than a nonce for every key — stop, try JSON fallback
		}
		nonce, ciphertext := data[:nonceSize], data[nonceSize:]
		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			continue // try the next candidate key
		}
		if err := json.Unmarshal(plaintext, &d); err != nil {
			return d, fmt.Errorf("unmarshal decrypted destination: %w", err)
		}
		return d, nil
	}

	// Fallback: plain JSON (development data) when every key failed.
	if jErr := json.Unmarshal(data, &d); jErr == nil {
		return d, nil
	}
	return d, fmt.Errorf("decrypt destination: all candidate keys failed")
}

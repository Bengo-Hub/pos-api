package destination

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// envRcloneBin overrides the rclone binary path/name. Default: "rclone".
	envRcloneBin = "RCLONE_BIN"
	// mirrorTimeout bounds a single best-effort upload so a stuck remote can
	// never block the backup pipeline.
	mirrorTimeout = 5 * time.Minute
	// testTimeout bounds the interactive connection test.
	testTimeout = 30 * time.Second
	// remoteName is the ephemeral, in-memory rclone remote name. Backend params
	// are passed via RCLONE_CONFIG_<REMOTE>_* env vars (see configEnv) so NOTHING
	// is ever written to a persistent rclone config file on disk.
	remoteName = "posbkp"
)

// Uploader mirrors a locally-written backup file to a configured remote
// destination, best-effort. The local PVC copy is always the durable primary;
// any mirror failure is logged at WARN and swallowed so the backup is never
// failed because of a remote problem.
type Uploader struct {
	store  *Store
	logger *zap.Logger
}

// NewUploader builds an Uploader over the destination Store.
func NewUploader(store *Store, logger *zap.Logger) *Uploader {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Uploader{store: store, logger: logger.Named("backup.uploader")}
}

// Mirror copies localFilePath to the tenant's resolved remote destination under
// RemotePath/objectName. It is BEST-EFFORT: on any error (no remote configured,
// rclone missing, transfer failure, timeout) it logs a WARN and returns nil so
// the caller does not fail the backup — the PVC copy is the fallback.
//
// Credentials/params are passed to rclone exclusively via environment variables
// and are NEVER logged.
func (u *Uploader) Mirror(ctx context.Context, tenantID uuid.UUID, localFilePath, objectName string) error {
	d := u.store.Resolve(ctx, tenantID)
	if !d.IsRemote() {
		// PVC-only / disabled: nothing to mirror, not an error.
		return nil
	}

	bin, err := rcloneBin()
	if err != nil {
		u.logger.Warn("rclone not found, skipping remote backup mirror (PVC copy retained)",
			zap.String("type", string(d.Type)), zap.Error(err))
		return nil
	}

	remoteTarget := remoteName + ":" + joinRemotePath(d.RemotePath, objectName)

	runCtx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()

	// copyto copies a single source file to an exact destination object path.
	cmd := exec.CommandContext(runCtx, bin, rcloneFlags("copyto", localFilePath, remoteTarget)...)
	cmd.Env = configEnv(d)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Log only the destination TYPE + sanitized rclone stderr — never params.
		u.logger.Warn("backup remote mirror failed (PVC copy retained as fallback)",
			zap.String("type", string(d.Type)),
			zap.String("object", objectName),
			zap.String("error", sanitize(err, &stderr, d)),
		)
		return nil
	}

	u.logger.Info("backup mirrored to remote destination",
		zap.String("type", string(d.Type)),
		zap.String("object", objectName))
	return nil
}

// TestResult is the sanitized outcome of a connection test.
type TestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// TestConnection verifies a destination is reachable by listing its RemotePath
// (rclone lsd). It returns a sanitized ok/message and never logs credentials.
// The supplied Destination is used directly (not resolved) so an operator can
// test a config before saving it.
func (u *Uploader) TestConnection(ctx context.Context, d Destination) TestResult {
	if !d.IsRemote() {
		return TestResult{OK: false, Message: "destination is pvc-only or disabled; nothing to test"}
	}
	if err := d.Validate(); err != nil {
		return TestResult{OK: false, Message: err.Error()}
	}

	bin, err := rcloneBin()
	if err != nil {
		return TestResult{OK: false, Message: "rclone binary not available on server"}
	}

	runCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	remoteTarget := remoteName + ":" + strings.Trim(d.RemotePath, "/")
	cmd := exec.CommandContext(runCtx, bin, rcloneFlags("lsd", remoteTarget)...)
	cmd.Env = configEnv(d)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return TestResult{OK: false, Message: sanitize(err, &stderr, d)}
	}
	return TestResult{OK: true, Message: "connection ok"}
}

// rcloneBin resolves the rclone binary, honoring RCLONE_BIN. Returns an error
// when it is not installed/locatable.
func rcloneBin() (string, error) {
	bin := getenv(envRcloneBin, "rclone")
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("rclone binary %q not found in PATH", bin)
	}
	return path, nil
}

// rcloneFlags builds the argv for an rclone subcommand with safe, quiet defaults.
// --config /dev/null guarantees no persistent config file is read or written;
// all backend settings come from the RCLONE_CONFIG_* env vars in configEnv.
func rcloneFlags(args ...string) []string {
	base := []string{
		"--config", configFileNul(),
		"--use-server-modtime",
		"--low-level-retries", "2",
		"--retries", "1",
	}
	return append(base, args...)
}

// configEnv builds the process environment carrying the ephemeral rclone remote
// definition via RCLONE_CONFIG_<REMOTE>_* variables. NOTHING is written to disk
// and these values are never logged. The current process env is inherited so
// rclone still finds CA certs, HOME, etc.
func configEnv(d Destination) []string {
	prefix := "RCLONE_CONFIG_" + strings.ToUpper(remoteName) + "_"
	set := func(env []string, key, val string) []string {
		if val == "" {
			return env
		}
		return append(env, prefix+strings.ToUpper(key)+"="+val)
	}

	env := inheritEnv()
	env = append(env, prefix+"TYPE="+rcloneBackend(d.Type))

	switch d.Type {
	case TypeS3:
		// provider defaults to AWS but is overridable for MinIO/R2/Wasabi/etc.
		env = set(env, "provider", firstNonEmpty(d.Params["provider"], "AWS"))
		env = set(env, "access_key_id", d.Params["access_key_id"])
		env = set(env, "secret_access_key", d.Params["secret_access_key"])
		env = set(env, "session_token", d.Params["session_token"])
		env = set(env, "region", d.Params["region"])
		env = set(env, "endpoint", d.Params["endpoint"])
		env = set(env, "acl", d.Params["acl"])
	case TypeOneDrive:
		env = set(env, "token", d.Params["token"])
		env = set(env, "drive_id", d.Params["drive_id"])
		env = set(env, "drive_type", firstNonEmpty(d.Params["drive_type"], "business"))
		env = set(env, "client_id", d.Params["client_id"])
		env = set(env, "client_secret", d.Params["client_secret"])
	case TypeGDrive:
		env = set(env, "token", d.Params["token"])
		env = set(env, "root_folder_id", d.Params["drive_id"])
		env = set(env, "client_id", d.Params["client_id"])
		env = set(env, "client_secret", d.Params["client_secret"])
		env = set(env, "service_account_credentials", d.Params["service_account_credentials"])
	case TypeWebDAV:
		env = set(env, "url", d.Params["url"])
		env = set(env, "vendor", firstNonEmpty(d.Params["vendor"], "other"))
		env = set(env, "user", d.Params["user"])
		env = set(env, "pass", obscure(d.Params["pass"]))
		env = set(env, "bearer_token", d.Params["bearer_token"])
	case TypeSFTP:
		env = set(env, "host", d.Params["host"])
		env = set(env, "port", firstNonEmpty(d.Params["port"], "22"))
		env = set(env, "user", d.Params["user"])
		env = set(env, "pass", obscure(d.Params["pass"]))
		env = set(env, "key_pem", d.Params["key_pem"])
		env = set(env, "key_file_pass", obscure(d.Params["key_file_pass"]))
	case TypeSMB:
		env = set(env, "host", d.Params["host"])
		env = set(env, "port", firstNonEmpty(d.Params["port"], "445"))
		env = set(env, "user", d.Params["user"])
		env = set(env, "pass", obscure(d.Params["pass"]))
		env = set(env, "domain", d.Params["domain"])
		env = set(env, "share", d.Params["share"])
	}
	return env
}

// rcloneBackend maps our destination Type to the rclone backend name. They match
// except for gdrive → "drive".
func rcloneBackend(t Type) string {
	if t == TypeGDrive {
		return "drive"
	}
	return string(t)
}

// obscure wraps a plaintext secret in rclone's reversible obscure() form when
// possible. rclone's webdav/sftp/smb "pass" expects an obscured value; if the
// rclone binary or call fails we fall back to the raw value (rclone also accepts
// already-obscured strings idempotently). Never logged.
func obscure(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	bin, err := rcloneBin()
	if err != nil {
		return plaintext
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "obscure", plaintext).Output()
	if err != nil {
		return plaintext
	}
	return strings.TrimSpace(string(out))
}

// joinRemotePath joins the configured RemotePath prefix with the object name,
// normalizing slashes. Result has no leading/trailing slashes.
func joinRemotePath(remotePath, objectName string) string {
	rp := strings.Trim(remotePath, "/")
	on := strings.TrimLeft(objectName, "/")
	if rp == "" {
		return on
	}
	return rp + "/" + on
}

// sanitize converts an rclone exec error + stderr into a short, user-safe message
// that never contains credential material. It strips any param VALUES that might
// have leaked into rclone's stderr (defense-in-depth; rclone does not normally
// print them) and caps the length.
func sanitize(runErr error, stderr *bytes.Buffer, d Destination) string {
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			msg = "rclone exited with " + exitErr.String()
		} else {
			msg = runErr.Error()
		}
	}
	// Redact any secret param value that somehow appears in the output.
	for _, k := range secretParamKeysByType[d.Type] {
		if v := d.Params[k]; v != "" {
			msg = strings.ReplaceAll(msg, v, "***")
		}
	}
	// Keep only the last non-empty line (rclone's final error) and cap length.
	if lines := splitNonEmptyLines(msg); len(lines) > 0 {
		msg = lines[len(lines)-1]
	}
	const max = 300
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	return msg
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

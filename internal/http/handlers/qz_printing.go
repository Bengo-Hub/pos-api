package handlers

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
)

// QZ Tray printing bridge.
//
// QZ Tray (the local print app the POS terminal runs) requires signed requests for SILENT printing:
// the browser asks pos-api for the platform digital certificate and, per print, for a signature of
// the request payload. The private key lives ONLY here (env QZ_PRIVATE_KEY) and never reaches the
// browser. When the key/cert env vars are not set these endpoints return empty values, so QZ falls
// back to an unsigned connection (a one-time operator allow-prompt) — printing still works.
//
// Setup (operator/platform, once): generate an RSA keypair + self-signed cert with QZ Tray's tools
// or OpenSSL, upload the certificate into QZ Tray (override.crt / allowed list), and set the env vars
// QZ_CERTIFICATE (PEM cert) and QZ_PRIVATE_KEY (PEM private key) on pos-api. These are platform-level
// secrets — provided via the deployment secret store, never committed.

var (
	qzKeyOnce sync.Once
	qzKey     *rsa.PrivateKey
	qzKeyErr  error
)

var errQZNotConfigured = errors.New("qz signing key not configured")

// envPEM reads a PEM value from env, tolerating values stored with literal "\n" escape sequences
// (common in some secret stores / CI env definitions).
func envPEM(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "\n") && strings.Contains(v, "\\n") {
		v = strings.ReplaceAll(v, "\\n", "\n")
	}
	return v
}

func loadQZKey() (*rsa.PrivateKey, error) {
	qzKeyOnce.Do(func() {
		raw := envPEM("QZ_PRIVATE_KEY")
		if raw == "" {
			qzKeyErr = errQZNotConfigured
			return
		}
		block, _ := pem.Decode([]byte(raw))
		if block == nil {
			qzKeyErr = errors.New("qz: invalid private key PEM")
			return
		}
		if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
			qzKey = k
			return
		}
		if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
			if rk, ok := k.(*rsa.PrivateKey); ok {
				qzKey = rk
				return
			}
			qzKeyErr = errors.New("qz: PKCS8 key is not RSA")
			return
		}
		qzKeyErr = errors.New("qz: unsupported private key format")
	})
	return qzKey, qzKeyErr
}

// QZCert handles GET /{tenantID}/pos/printing/qz/cert — returns the platform QZ digital certificate
// (PEM). Empty when unconfigured, which makes QZ Tray connect unsigned (operator allow-prompt).
func QZCert(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{"certificate": envPEM("QZ_CERTIFICATE")})
}

// QZSign handles POST /{tenantID}/pos/printing/qz/sign — signs the QZ request payload with the
// platform private key (SHA512withRSA, matching qz-tray's default algorithm) and returns the
// base64 signature. Returns an empty signature when no key is configured so the client falls back
// to an unsigned connection rather than failing.
func QZSign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Request string `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	key, err := loadQZKey()
	if err != nil {
		// Not configured (or bad key) → empty signature → unsigned QZ connection (allow-prompt).
		jsonOK(w, map[string]any{"signature": ""})
		return
	}
	digest := sha512.Sum512([]byte(in.Request))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA512, digest[:])
	if err != nil {
		jsonError(w, "failed to sign request", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"signature": base64.StdEncoding.EncodeToString(sig)})
}

package app

import (
	"errors"
	"strings"

	"go.uber.org/zap"
)

// localEnvs are the APP_ENV values where the terminal JWT secret may fall back to the
// internal service key. Every other value, including an unknown one, is treated as production.
var localEnvs = map[string]bool{
	"development": true, "dev": true, "local": true, "test": true,
}

// resolveTerminalJWTSecret returns the HMAC secret for terminal PIN tokens. Every service in the
// fleet holds INTERNAL_SERVICE_KEY, so a terminal token signed with it can be forged by any of
// them, platform owner and sub_exempt included. Outside local/dev the secret must be set and must
// not equal the internal key, or the service refuses to start. In local/dev it falls back to the
// internal key with a loud warning.
func resolveTerminalJWTSecret(log *zap.Logger, env, secret string, internalKeys ...string) ([]byte, error) {
	secret = strings.TrimSpace(secret)
	local := localEnvs[strings.ToLower(strings.TrimSpace(env))]
	sharesInternalKey := false
	for _, k := range internalKeys {
		if k != "" && secret == strings.TrimSpace(k) {
			sharesInternalKey = true
		}
	}
	if !local {
		if secret == "" {
			return nil, errors.New("TERMINAL_JWT_SECRET is required outside local/dev")
		}
		if sharesInternalKey {
			return nil, errors.New("TERMINAL_JWT_SECRET must not equal INTERNAL_SERVICE_KEY")
		}
		return []byte(secret), nil
	}
	if secret != "" {
		if sharesInternalKey && log != nil {
			log.Warn("TERMINAL_JWT_SECRET equals INTERNAL_SERVICE_KEY; any service can forge PIN tokens. Allowed only in local/dev.")
		}
		return []byte(secret), nil
	}
	for _, k := range internalKeys {
		if k = strings.TrimSpace(k); k != "" {
			if log != nil {
				log.Warn("TERMINAL_JWT_SECRET is not set; signing PIN tokens with INTERNAL_SERVICE_KEY. Allowed only in local/dev, never in production.")
			}
			return []byte(k), nil
		}
	}
	return nil, errors.New("TERMINAL_JWT_SECRET is not set and there is no INTERNAL_SERVICE_KEY to fall back to")
}

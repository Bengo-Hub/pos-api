package app

import (
	"testing"

	"go.uber.org/zap"
)

// TestTerminalSecretRequiredInProduction locks in that production never signs PIN tokens with
// the fleet-wide internal service key, and that local/dev keeps the fallback.
func TestTerminalSecretRequiredInProduction(t *testing.T) {
	log := zap.NewNop()
	const internal = "shared-internal-key"

	for _, env := range []string{"production", "staging", "", "PRODUCTION"} {
		if _, err := resolveTerminalJWTSecret(log, env, "", internal); err == nil {
			t.Fatalf("env %q: empty TERMINAL_JWT_SECRET must refuse to start", env)
		}
		if _, err := resolveTerminalJWTSecret(log, env, internal, internal); err == nil {
			t.Fatalf("env %q: TERMINAL_JWT_SECRET equal to INTERNAL_SERVICE_KEY must refuse to start", env)
		}
		if _, err := resolveTerminalJWTSecret(log, env, internal, "", internal); err == nil {
			t.Fatalf("env %q: a match on any internal key copy must refuse to start", env)
		}
		got, err := resolveTerminalJWTSecret(log, env, "dedicated-terminal-secret", internal)
		if err != nil || string(got) != "dedicated-terminal-secret" {
			t.Fatalf("env %q: dedicated secret = %q, %v", env, got, err)
		}
	}

	for _, env := range []string{"development", "local", "Dev"} {
		got, err := resolveTerminalJWTSecret(log, env, "", internal)
		if err != nil || string(got) != internal {
			t.Fatalf("env %q: dev fallback = %q, %v; want the internal key", env, got, err)
		}
	}
}

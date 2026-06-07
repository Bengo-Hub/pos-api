package events

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Defense-in-depth for the "consumer is already bound to a subscription" race.
//
// When a pod is replaced, the new pod can try to bind a JetStream push durable
// before the NATS server has released the previous pod's binding, get
// "already bound", and (in the old code) abandon the subscription — the consumer
// then silently stops until the next restart. We layer several independent
// mitigations so that no single failing layer takes us back to that state:
//
//	Layer 1 (deploy):   maxSurge=0 / maxUnavailable=1 — the old pod terminates
//	                    before the replacement starts (devops-k8s app values).
//	Layer 2 (settle):   rebindSettle() — wait before the FIRST attempt so the
//	                    server can release the prior binding (the proven ~25s
//	                    operational buffer, applied in code).
//	Layer 3 (retry):    retry on "already bound" with capped backoff over a long
//	                    window, so even if Layers 1–2 are insufficient the
//	                    subscription recovers on its own instead of dying.
//	Layer 4 (ops):      the manual scale-cycle-with-buffer remains as a human
//	                    backstop for pathological cases.

// rebindSettle is the Layer-2 proactive buffer before the first subscribe
// attempt. Defaults to 25s (matching the operational scale-cycle buffer) and is
// tunable via NATS_REBIND_SETTLE_SECONDS (set 0 to disable).
func rebindSettle() time.Duration {
	if v := os.Getenv("NATS_REBIND_SETTLE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 25 * time.Second
}

func isAlreadyBound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already bound")
}

// SubscribeWithRebind establishes a durable JetStream push subscription with the
// layered resilience described above. It runs in the background so it never
// blocks startup; the returned subscription is retained by the NATS client for
// the life of the connection.
func SubscribeWithRebind(log *zap.Logger, js nats.JetStreamContext, subject string, handler nats.MsgHandler, opts ...nats.SubOpt) {
	go func() {
		time.Sleep(rebindSettle()) // Layer 2
		backoff := 3 * time.Second
		const maxBackoff = 30 * time.Second
		for attempt := 1; attempt <= 40; attempt++ { // Layer 3 — long, self-healing window
			if _, err := js.Subscribe(subject, handler, opts...); err == nil {
				log.Info("jetstream subscription active", zap.String("subject", subject))
				return
			} else if !isAlreadyBound(err) {
				log.Error("jetstream subscribe failed (non-retryable)",
					zap.String("subject", subject), zap.Error(err))
				return
			}
			log.Warn("jetstream subscribe bind conflict; retrying",
				zap.String("subject", subject),
				zap.Int("attempt", attempt),
				zap.Duration("backoff", backoff))
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
		}
		log.Error("jetstream subscribe gave up after retries", zap.String("subject", subject))
	}()
}

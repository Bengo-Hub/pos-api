package middleware

import (
	"strings"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
)

// Per-pod read caches in this package (outlet settings, maintenance windows) are cleared on
// EVERY replica when a write invalidates them, through one Broadcaster topic. Before, an
// invalidation only cleared the replica that handled the write, so other replicas served the
// old module toggle or maintenance window until their TTL ran out.

const invalidationTopic = "cache-invalidate"

var invalidator *eventslib.Broadcaster

// SetCacheInvalidator wires cross-replica invalidation. Call once at startup; without it,
// invalidations stay local (single replica, tests).
func SetCacheInvalidator(b *eventslib.Broadcaster) {
	invalidator = b
	if b == nil {
		return
	}
	_ = b.Subscribe(invalidationTopic, func(m eventslib.BroadcastMessage) {
		kind, rawID, ok := strings.Cut(m.Scope, ":")
		if !ok {
			return
		}
		id, err := uuid.Parse(rawID)
		if err != nil {
			return
		}
		switch kind {
		case "outlet-setting":
			settingCache.Delete(id)
		case "maintenance":
			maintenanceCache.Delete(id)
		}
	})
}

// invalidate clears kind/id on this replica and, when wired, on every other replica.
func invalidate(kind string, id uuid.UUID, local func()) {
	if invalidator == nil {
		local()
		return
	}
	// Publish delivers to this replica's handler first, then relays to the others.
	_ = invalidator.Publish(invalidationTopic, "", kind+":"+id.String(), nil)
}

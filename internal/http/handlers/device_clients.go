package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevice"
	"github.com/bengobox/pos-service/internal/platform/clientinfo"
)

// Every browser at an outlet shares the outlet's one web_terminal POSDevice row (shifts and the
// plan's device cap are per row), so the browsers themselves are listed on that row as
// metadata.clients: one entry per browser keyed by its client id, refreshed on every PIN sign-in.
// The Devices settings tab shows them.
const (
	deviceClientsKey = "clients"
	// maxDeviceClients bounds the list; the least recently seen browsers drop off first.
	maxDeviceClients = 50
)

// deviceClientKey identifies a browser: its client id, or for an older pos-ui build that does
// not send one, a stable hash of its user agent.
func deviceClientKey(info clientinfo.Info) string {
	if info.ClientID != "" {
		return info.ClientID
	}
	if info.UserAgent == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(info.UserAgent))
	return "ua-" + hex.EncodeToString(sum[:6])
}

// touchOutletDeviceClient records the signing-in browser on the outlet's web_terminal device
// and bumps the device's last_seen_at. The row is locked for the read-modify-write so two
// sign-ins on different pods can't drop each other's entry. Best effort: a failure is logged
// and never blocks the sign-in.
func touchOutletDeviceClient(ctx context.Context, client *ent.Client, log *zap.Logger, tid, outletID, staffUserID uuid.UUID, staffName string) {
	info, ok := clientinfo.FromContext(ctx)
	if !ok || outletID == uuid.Nil {
		return
	}
	key := deviceClientKey(info)
	if key == "" {
		return
	}
	tx, err := client.Tx(ctx)
	if err != nil {
		log.Warn("device client: begin tx", zap.Error(err))
		return
	}
	defer func() { _ = tx.Rollback() }()

	dev, err := tx.POSDevice.Query().
		Where(posdevice.TenantID(tid), posdevice.OutletID(outletID), posdevice.DeviceType("web_terminal")).
		ForUpdate().
		First(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			log.Warn("device client: load device", zap.Error(err))
		}
		return
	}

	now := time.Now().UTC()
	meta := make(map[string]any, len(dev.Metadata)+1)
	for k, v := range dev.Metadata {
		meta[k] = v
	}
	clients := map[string]any{}
	if existing, ok := meta[deviceClientsKey].(map[string]any); ok {
		for k, v := range existing {
			clients[k] = v
		}
	}
	entry := info.Map()
	entry["last_seen_at"] = now.Format(time.RFC3339)
	entry["last_user_id"] = staffUserID.String()
	if staffName != "" {
		entry["last_user_name"] = staffName
	}
	entry["first_seen_at"] = entry["last_seen_at"]
	if prev, ok := clients[key].(map[string]any); ok {
		if first, _ := prev["first_seen_at"].(string); first != "" {
			entry["first_seen_at"] = first
		}
		// A sign-in without a device name keeps the one set earlier.
		if _, named := entry["device_name"]; !named {
			if name, _ := prev["device_name"].(string); name != "" {
				entry["device_name"] = name
			}
		}
	}
	clients[key] = entry
	pruneDeviceClients(clients, maxDeviceClients)
	meta[deviceClientsKey] = clients

	if err := tx.POSDevice.UpdateOneID(dev.ID).SetMetadata(meta).SetLastSeenAt(now).Exec(ctx); err != nil {
		log.Warn("device client: save", zap.Error(err))
		return
	}
	if err := tx.Commit(); err != nil {
		log.Warn("device client: commit", zap.Error(err))
	}
}

// pruneDeviceClients drops the least recently seen entries beyond max.
func pruneDeviceClients(clients map[string]any, max int) {
	if len(clients) <= max {
		return
	}
	for _, k := range sortedDeviceClientKeys(clients)[max:] {
		delete(clients, k)
	}
}

// sortedDeviceClientKeys orders entries most recently seen first (RFC3339 UTC sorts as text).
func sortedDeviceClientKeys(clients map[string]any) []string {
	keys := make([]string, 0, len(clients))
	for k := range clients {
		keys = append(keys, k)
	}
	seen := func(k string) string {
		m, _ := clients[k].(map[string]any)
		s, _ := m["last_seen_at"].(string)
		return s
	}
	sort.Slice(keys, func(i, j int) bool {
		if a, b := seen(keys[i]), seen(keys[j]); a != b {
			return a > b
		}
		return keys[i] < keys[j]
	})
	return keys
}

// deviceClientList renders a device's metadata.clients for the API, most recent first.
func deviceClientList(meta map[string]any) []map[string]any {
	clients, ok := meta[deviceClientsKey].(map[string]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(clients))
	for _, k := range sortedDeviceClientKeys(clients) {
		if m, ok := clients[k].(map[string]any); ok {
			row := make(map[string]any, len(m)+1)
			for kk, v := range m {
				row[kk] = v
			}
			row["key"] = k
			out = append(out, row)
		}
	}
	return out
}

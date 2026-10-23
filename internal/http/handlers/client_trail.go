package handlers

import (
	"github.com/bengobox/pos-service/internal/platform/clientinfo"
)

// clientInfoFromMap reads back a client trail object written by audit.TrailHook
// (clientinfo.Info.Map) from order, payment, session or audit metadata.
func clientInfoFromMap(m map[string]any) clientinfo.Info {
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	return clientinfo.Info{
		ClientID:   str("client_id"),
		DeviceName: str("device_name"),
		DeviceKind: str("device_kind"),
		OS:         str("os"),
		Browser:    str("browser"),
		UserAgent:  str("user_agent"),
		IP:         str("ip"),
	}
}

// clientLabel renders a client trail object as "Front PC (desktop, Windows Chrome 109) · 196.96.0.1".
func clientLabel(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	info := clientInfoFromMap(m)
	label := info.Label()
	if info.IP != "" {
		if label != "" {
			label += " · "
		}
		label += info.IP
	}
	return label
}

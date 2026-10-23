// Package clientinfo captures which browser or device sent a request: the stable per-browser
// client id and the staff-chosen device name that pos-ui sends as headers, the raw user agent,
// a parsed device kind (desktop, mobile, tablet), OS and browser, and the trusted client IP.
//
// Every outlet shares one web_terminal POSDevice row, so this is the only thing that tells two
// browsers at the same outlet apart. The middleware stores an Info on the request context and
// audit.TrailHook stamps it onto orders, payments, shift sessions and audit entries at create
// time, which is what lets an audit answer "which phone or PC did this come from".
package clientinfo

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
)

// Request headers pos-ui sends. They must also be listed in the service's CORS extra headers.
const (
	HeaderClientID   = "X-Client-Id"
	HeaderDeviceName = "X-Device-Name"
)

// Device kinds.
const (
	KindDesktop = "desktop"
	KindMobile  = "mobile"
	KindTablet  = "tablet"
	KindUnknown = "unknown"
)

const (
	maxUserAgentLen  = 400
	maxDeviceNameLen = 60
)

// Info describes the client behind one request. Every field is best effort and may be empty.
type Info struct {
	ClientID   string `json:"client_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	DeviceKind string `json:"device_kind,omitempty"`
	OS         string `json:"os,omitempty"`
	Browser    string `json:"browser,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
	IP         string `json:"ip,omitempty"`
}

// IsZero reports whether nothing at all is known about the client.
func (i Info) IsZero() bool { return i == Info{} }

// Map renders the info as a metadata object with empty fields left out.
func (i Info) Map() map[string]any {
	m := make(map[string]any, 7)
	put := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	put("client_id", i.ClientID)
	put("device_name", i.DeviceName)
	put("device_kind", i.DeviceKind)
	put("os", i.OS)
	put("browser", i.Browser)
	put("user_agent", i.UserAgent)
	put("ip", i.IP)
	return m
}

// Label is a short human description, e.g. "Titus's iPhone (mobile, iOS Safari)".
func (i Info) Label() string {
	detail := strings.TrimSpace(strings.Join(nonEmpty(i.DeviceKind, strings.TrimSpace(i.OS+" "+i.Browser)), ", "))
	switch {
	case i.DeviceName != "" && detail != "":
		return i.DeviceName + " (" + detail + ")"
	case i.DeviceName != "":
		return i.DeviceName
	default:
		return detail
	}
}

type ctxKey struct{}

// WithInfo returns a copy of ctx carrying info.
func WithInfo(ctx context.Context, info Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// FromContext returns the request's client info. ok is false for background work (workers,
// event consumers) that has no originating browser.
func FromContext(ctx context.Context) (Info, bool) {
	info, ok := ctx.Value(ctxKey{}).(Info)
	if !ok || info.IsZero() {
		return Info{}, false
	}
	return info, true
}

// Middleware stores the request's Info on its context. It must run after
// ratelimit.TrustedRealIP so the IP is the real client address, not the ingress hop.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithInfo(r.Context(), FromRequest(r))))
	})
}

// FromRequest builds the Info for r.
func FromRequest(r *http.Request) Info {
	ua := truncate(strings.TrimSpace(r.Header.Get("User-Agent")), maxUserAgentLen)
	kind, os, browser := Classify(ua, r.Header.Get("Sec-CH-UA-Mobile"), r.Header.Get("Sec-CH-UA-Platform"))
	return Info{
		ClientID:   ParseClientID(r.Header.Get(HeaderClientID)),
		DeviceName: CleanDeviceName(r.Header.Get(HeaderDeviceName)),
		DeviceKind: kind,
		OS:         os,
		Browser:    browser,
		UserAgent:  ua,
		IP:         ratelimit.ClientIP(r),
	}
}

// FromReported builds an Info from values a browser reported about itself in a request body,
// e.g. the device an offline sale was rung up on, captured at ring-up and replayed at sync. The
// values are sanitized exactly like the headers; there is no IP because none was observed.
func FromReported(clientID, deviceName, userAgent string) Info {
	ua := truncate(strings.TrimSpace(userAgent), maxUserAgentLen)
	kind, os, browser := Classify(ua, "", "")
	return Info{
		ClientID:   ParseClientID(clientID),
		DeviceName: CleanDeviceName(deviceName),
		DeviceKind: kind,
		OS:         os,
		Browser:    browser,
		UserAgent:  ua,
	}
}

// ParseClientID accepts only a UUID so the header can't be used to smuggle arbitrary text.
func ParseClientID(v string) string {
	id, err := uuid.Parse(strings.TrimSpace(v))
	if err != nil || id == uuid.Nil {
		return ""
	}
	return id.String()
}

// CleanDeviceName decodes the URI-encoded header value (HTTP headers are ASCII, so pos-ui
// encodes names like "Titus's iPhone" or ones with emoji), strips control characters and
// caps the length.
func CleanDeviceName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if dec, err := url.PathUnescape(v); err == nil {
		v = dec
	}
	if !utf8.ValidString(v) {
		return ""
	}
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
	v = strings.Join(strings.Fields(v), " ")
	return truncate(v, maxDeviceNameLen)
}

// Classify derives the device kind, OS and browser from a user agent plus the low-entropy
// client hints Chromium sends on HTTPS requests (Sec-CH-UA-Mobile "?1"/"?0",
// Sec-CH-UA-Platform "\"Android\""). Chrome on Android tablets omits "Mobile" from the UA,
// which is how a tablet is told apart from a phone.
func Classify(ua, chMobile, chPlatform string) (kind, os, browser string) {
	if ua == "" && chPlatform == "" {
		return "", "", ""
	}
	os = platformOS(strings.Trim(strings.TrimSpace(chPlatform), `"`))
	if os == "" {
		os = uaOS(ua)
	}
	browser = uaBrowser(ua)

	switch {
	case strings.Contains(ua, "iPad") || strings.Contains(ua, "Tablet"):
		kind = KindTablet
	case strings.Contains(ua, "Android") && !strings.Contains(ua, "Mobile"):
		kind = KindTablet
	case strings.TrimSpace(chMobile) == "?1",
		strings.Contains(ua, "Mobi"), strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPod"):
		kind = KindMobile
	case os == "Windows" || os == "macOS" || os == "Linux" || os == "ChromeOS":
		kind = KindDesktop
	default:
		kind = KindUnknown
	}
	return kind, os, browser
}

func platformOS(p string) string {
	switch strings.ToLower(p) {
	case "windows":
		return "Windows"
	case "android":
		return "Android"
	case "ios":
		return "iOS"
	case "macos":
		return "macOS"
	case "chrome os", "chromeos":
		return "ChromeOS"
	case "linux":
		return "Linux"
	}
	return ""
}

func uaOS(ua string) string {
	switch {
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"), strings.Contains(ua, "iPod"):
		return "iOS"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "CrOS"):
		return "ChromeOS"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		return "macOS"
	case strings.Contains(ua, "Linux"), strings.Contains(ua, "X11"):
		return "Linux"
	}
	return ""
}

// uaBrowser checks the more specific tokens first: Edge, Opera and Samsung Internet all
// also claim "Chrome", and every Chromium browser claims "Safari".
func uaBrowser(ua string) string {
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"EdgA/", "Edge"}, {"EdgiOS/", "Edge"},
		{"OPR/", "Opera"}, {"SamsungBrowser/", "Samsung Internet"},
		{"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"},
		{"CriOS/", "Chrome"}, {"Chrome/", "Chrome"},
	} {
		if v := versionAfter(ua, b.token); v != "" {
			return b.name + " " + v
		}
		if strings.Contains(ua, b.token) {
			return b.name
		}
	}
	if strings.Contains(ua, "Safari/") {
		if v := versionAfter(ua, "Version/"); v != "" {
			return "Safari " + v
		}
		return "Safari"
	}
	return ""
}

// versionAfter returns the major version that follows token, e.g. "138" for "Chrome/138.0.0.0".
func versionAfter(ua, token string) string {
	i := strings.Index(ua, token)
	if i < 0 {
		return ""
	}
	rest := ua[i+len(token):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	return rest[:end]
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func nonEmpty(vs ...string) []string {
	out := vs[:0:0]
	for _, v := range vs {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

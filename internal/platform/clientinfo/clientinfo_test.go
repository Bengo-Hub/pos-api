package clientinfo

import (
	"net/http/httptest"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name, ua, chMobile, chPlatform string
		kind, os, browser              string
	}{
		{
			name: "android tablet chrome omits Mobile",
			ua:   "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
			kind: KindTablet, os: "Android", browser: "Chrome 138",
		},
		{
			name: "android phone chrome",
			ua:   "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Mobile Safari/537.36",
			kind: KindMobile, os: "Android", browser: "Chrome 155",
		},
		{
			name: "windows edge",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36 Edg/155.0.0.0",
			kind: KindDesktop, os: "Windows", browser: "Edge 155",
		},
		{
			name: "iphone safari",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
			kind: KindMobile, os: "iOS", browser: "Safari 18",
		},
		{
			name: "ipad safari is a tablet even though it says Mobile",
			ua:   "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			kind: KindTablet, os: "iOS", browser: "Safari 17",
		},
		{
			name: "client hint marks a phone using desktop mode",
			ua:   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
			chMobile: "?1", chPlatform: `"Android"`,
			kind: KindMobile, os: "Android", browser: "Chrome 138",
		},
		{name: "empty", kind: "", os: "", browser: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, os, browser := Classify(c.ua, c.chMobile, c.chPlatform)
			if kind != c.kind || os != c.os || browser != c.browser {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", kind, os, browser, c.kind, c.os, c.browser)
			}
		})
	}
}

func TestCleanDeviceName(t *testing.T) {
	cases := map[string]string{
		"Titus%27s%20iPhone":    "Titus's iPhone",
		"  Front   counter  PC ": "Front counter PC",
		"Bar%0Atablet":          "Bartablet",
		"":                      "",
		"Xiaomi%20%F0%9F%93%B1": "Xiaomi 📱",
	}
	for in, want := range cases {
		if got := CleanDeviceName(in); got != want {
			t.Errorf("CleanDeviceName(%q) = %q, want %q", in, got, want)
		}
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	if got := CleanDeviceName(long); len(got) != maxDeviceNameLen {
		t.Errorf("long name not capped: %d", len(got))
	}
}

func TestFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/109.0.0.0 Safari/537.36")
	r.Header.Set("X-Real-IP", "196.96.125.206")
	r.Header.Set(HeaderClientID, "not-a-uuid")
	r.Header.Set(HeaderDeviceName, "Front%20PC")
	info := FromRequest(r)
	if info.IP != "196.96.125.206" || info.ClientID != "" || info.DeviceName != "Front PC" || info.DeviceKind != KindDesktop {
		t.Fatalf("unexpected info: %+v", info)
	}
	if got := info.Label(); got != "Front PC (desktop, Windows Chrome 109)" {
		t.Fatalf("label = %q", got)
	}
	if _, ok := FromContext(WithInfo(r.Context(), Info{})); ok {
		t.Fatal("zero info must not be reported as present")
	}
}

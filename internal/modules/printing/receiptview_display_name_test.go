package printing

import "testing"

// TestResolveDisplayName covers every branch of the BOI Enterprises multi-outlet tenant-vs-outlet
// name rule: default (tenant name everywhere), HQ always wins regardless of the toggle, a non-HQ
// outlet explicitly opting out, and the never-blank-header fallback.
func TestResolveDisplayName(t *testing.T) {
	cases := []struct {
		name       string
		tenantName string
		outletName string
		isHQ       bool
		metadata   map[string]any
		want       string
	}{
		{
			name:       "default metadata absent shows tenant name on a non-HQ outlet",
			tenantName: "BOI Enterprises",
			outletName: "Westlands Branch",
			isHQ:       false,
			metadata:   nil,
			want:       "BOI Enterprises",
		},
		{
			name:       "explicit true behaves same as absent",
			tenantName: "BOI Enterprises",
			outletName: "Westlands Branch",
			isHQ:       false,
			metadata:   map[string]any{"receipt_show_tenant_name": true},
			want:       "BOI Enterprises",
		},
		{
			name:       "HQ outlet always shows tenant name even when toggle is off",
			tenantName: "BOI Enterprises",
			outletName: "Head Office",
			isHQ:       true,
			metadata:   map[string]any{"receipt_show_tenant_name": false},
			want:       "BOI Enterprises",
		},
		{
			name:       "non-HQ outlet with toggle off shows its OWN name",
			tenantName: "BOI Enterprises",
			outletName: "Westlands Branch",
			isHQ:       false,
			metadata:   map[string]any{"receipt_show_tenant_name": false},
			want:       "Westlands Branch",
		},
		{
			name:       "empty tenant name never blanks the header",
			tenantName: "",
			outletName: "Westlands Branch",
			isHQ:       true,
			metadata:   nil,
			want:       "Westlands Branch",
		},
		{
			name:       "non-bool metadata value treated as absent (default tenant name)",
			tenantName: "BOI Enterprises",
			outletName: "Westlands Branch",
			isHQ:       false,
			metadata:   map[string]any{"receipt_show_tenant_name": "false"}, // wrong type — from a stale/manual DB edit
			want:       "BOI Enterprises",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDisplayName(tc.tenantName, tc.outletName, tc.isHQ, tc.metadata)
			if got != tc.want {
				t.Errorf("resolveDisplayName(%q, %q, %v, %v) = %q, want %q",
					tc.tenantName, tc.outletName, tc.isHQ, tc.metadata, got, tc.want)
			}
		})
	}
}

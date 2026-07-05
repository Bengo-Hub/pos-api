package handlers

import (
	"net/http"
	"os"
	"strings"
)

// defaultPrintAgentWindowsURL is the fallback download for the Windows Print Agent installer when
// PRINT_AGENT_DOWNLOAD_URL is not configured — the "latest" GitHub release asset (stable URL).
const defaultPrintAgentWindowsURL = "https://github.com/Bengo-Hub/pos-service/releases/latest/download/CodevertexPrintAgentSetup.exe"

// PrintAgentDownload handles GET /api/v1/pos/print-agent/download?os=windows
// Public (no auth) — it serves a generic, credential-free installer. It 302-redirects to the release
// asset so the binary is hosted/served by GitHub Releases (or an override), not baked into this image.
func PrintAgentDownload(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(os.Getenv("PRINT_AGENT_DOWNLOAD_URL"))
	if target == "" {
		// Only Windows is packaged today; other OSes fall back to the same (or later add mac/linux).
		target = defaultPrintAgentWindowsURL
	}
	http.Redirect(w, r, target, http.StatusFound)
}

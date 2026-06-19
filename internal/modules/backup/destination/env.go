package destination

import (
	"os"
	"runtime"
)

// getenv returns the env var value or fallback when unset/empty.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// inheritEnv returns a copy of the current process environment, used as the base
// for rclone invocations so it still finds HOME, CA certificates, proxy settings,
// etc. The ephemeral RCLONE_CONFIG_* backend vars are appended on top.
func inheritEnv() []string {
	src := os.Environ()
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// configFileNul returns the platform null device path passed to `rclone --config`
// so rclone neither reads nor writes any persistent config file — all backend
// settings come from RCLONE_CONFIG_* env vars instead. The production runtime is
// Linux (alpine); the Windows branch keeps local `go build`/dev runs honest.
func configFileNul() string {
	if runtime.GOOS == "windows" {
		return "NUL"
	}
	return "/dev/null"
}

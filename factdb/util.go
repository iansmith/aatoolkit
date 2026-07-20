package factdb

import "os"

// envOr returns the environment variable v, or def when it is unset or empty.
func envOr(v, def string) string {
	if s := os.Getenv(v); s != "" {
		return s
	}
	return def
}

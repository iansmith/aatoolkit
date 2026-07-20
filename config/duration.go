package config

import (
	"fmt"
	"time"
)

// Duration wraps time.Duration so it can be decoded from a TOML string like
// "5s" or "500ms" (time.ParseDuration syntax) via the toml.Unmarshaler
// interface (BurntSushi/toml's UnmarshalText hook).
type Duration struct {
	time.Duration
}

// UnmarshalText implements encoding.TextUnmarshaler, which BurntSushi/toml
// uses to decode TOML strings into non-string Go types.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	d.Duration = parsed
	return nil
}

// MarshalText implements encoding.TextMarshaler for round-tripping.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

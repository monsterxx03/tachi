package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that marshals/unmarshals as a human-readable
// string (e.g. "10s", "1m", "500ms") in JSON. This allows JSON config files
// to use natural duration strings instead of raw nanosecond integers.
type Duration time.Duration

// MarshalJSON implements json.Marshaler, outputting a string like "10s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler, accepting both string ("10s")
// and numeric (nanoseconds) representations for compatibility.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case string:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", value, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(time.Duration(value))
	default:
		return fmt.Errorf("invalid duration: expected string or number, got %T", v)
	}
	return nil
}

// String returns the duration as a string (e.g. "10s").
func (d Duration) String() string {
	return time.Duration(d).String()
}

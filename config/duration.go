package config

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that marshals/unmarshals as a human-readable
// string (e.g. "10s", "1m", "500ms") in JSON and YAML. This allows config
// files to use natural duration strings instead of raw nanosecond integers.
type Duration time.Duration

// MarshalJSON implements json.Marshaler, outputting a string like "10s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler, accepting both string ("10s")
// and numeric (nanoseconds) representations for compatibility.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	return d.parse(v)
}

// MarshalYAML implements yaml.Marshaler, outputting a string like "10s".
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// UnmarshalYAML implements yaml.Unmarshaler, accepting both string ("10s")
// and numeric (nanoseconds) representations — matching UnmarshalJSON.
// Without this, Duration fields tagged with yaml (e.g. lsp.request_timeout)
// fail to load whenever a user writes a natural string like "15s".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var v any
	if err := value.Decode(&v); err != nil {
		return err
	}
	return d.parse(v)
}

// parse accepts a decoded scalar as a duration string ("10s") or a number
// (nanoseconds). Shared by the JSON and YAML unmarshalers.
func (d *Duration) parse(v any) error {
	switch value := v.(type) {
	case string:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", value, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(time.Duration(value))
	case int:
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

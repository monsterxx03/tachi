package llm

import (
	"testing"
)

func TestEffortFromString_Default(t *testing.T) {
	result := effortFromString("")
	if result != "high" {
		t.Errorf("effortFromString('') = %q, want 'high'", result)
	}
}

func TestEffortFromString_Valid(t *testing.T) {
	valid := []string{"low", "medium", "high", "xhigh", "max"}
	for _, e := range valid {
		result := effortFromString(e)
		if string(result) != e {
			t.Errorf("effortFromString(%q) = %q, want %q", e, result, e)
		}
	}
}

func TestEffortFromString_Invalid(t *testing.T) {
	// Invalid values should fall back to "high"
	result := effortFromString("extreme")
	if result != "high" {
		t.Errorf("effortFromString('extreme') = %q, want 'high'", result)
	}
}

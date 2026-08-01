package httpx

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	c := NewClient(10*time.Second, "")
	if c == nil {
		t.Fatal("NewClient with empty proxy returned nil")
	}
	if c.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", c.Timeout)
	}

	// Invalid proxy URL must fall back to a plain client, not fail.
	c = NewClient(5*time.Second, "://bad-url")
	if c == nil || c.Timeout != 5*time.Second {
		t.Errorf("fallback client wrong: %+v", c)
	}
}

func TestReadAllLimited(t *testing.T) {
	body := []byte("0123456789")

	got, err := ReadAllLimited(strings.NewReader(string(body)), 10)
	if err != nil {
		t.Fatalf("exact-size read: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("got %q, want %q", got, body)
	}

	_, err = ReadAllLimited(strings.NewReader(string(body)), 9)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("over-limit read: got %v, want ErrTooLarge", err)
	}
}

func TestReadAllLimitedLenient(t *testing.T) {
	body := "0123456789"

	got, truncated, err := ReadAllLimitedLenient(strings.NewReader(body), 5)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || string(got) != "01234" {
		t.Errorf("got %q truncated=%v, want 01234 true", got, truncated)
	}

	got, truncated, err = ReadAllLimitedLenient(strings.NewReader(body), 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || string(got) != body {
		t.Errorf("got %q truncated=%v, want %q false", got, truncated, body)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrorKind
	}{
		{402, "no more credits", KindQuota},
		{402, "anything", KindQuota},
		{429, "slow down", KindRateLimit},
		{429, "NO MORE CREDITS", KindQuota},
		{429, "insufficient credits", KindQuota},
		{200, "budget exceeded", KindQuota},
		{500, "server error", KindOther},
		{404, "not found", KindOther},
	}
	for _, tc := range cases {
		if got := ClassifyError(tc.status, tc.body); got != tc.want {
			t.Errorf("ClassifyError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestIsQuotaExhaustedBody(t *testing.T) {
	if !IsQuotaExhaustedBody("credits exhausted") {
		t.Error("credits exhausted should be detected")
	}
	if IsQuotaExhaustedBody("rate limit exceeded") {
		t.Error("bare rate limit wording should NOT be detected")
	}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, code := range []int{408, 409, 429, 500, 502, 503} {
		if !IsRetryableStatus(code) {
			t.Errorf("IsRetryableStatus(%d) = false, want true", code)
		}
	}
	for _, code := range []int{200, 400, 401, 404, http.StatusOK} {
		if IsRetryableStatus(code) {
			t.Errorf("IsRetryableStatus(%d) = true, want false", code)
		}
	}
}

func TestJoinURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://api.example.com", "v1/search", "https://api.example.com/v1/search"},
		{"https://api.example.com/", "/v1/search", "https://api.example.com/v1/search"},
		{"https://api.example.com", "/search", "https://api.example.com/search"},
	}
	for _, tc := range cases {
		if got := JoinURL(tc.base, tc.path); got != tc.want {
			t.Errorf("JoinURL(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

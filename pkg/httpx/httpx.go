// Package httpx provides shared HTTP helpers used across tachi: client
// construction (with optional proxy), size-limited body reads, error
// classification, and URL joining.
package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// NewClient returns an *http.Client with the given timeout. When proxyURL is
// non-empty it is used (socks5/http/https); a proxy misconfiguration is
// logged and falls back to a plain client rather than failing the request.
func NewClient(timeout time.Duration, proxyURL string) *http.Client {
	if proxyURL != "" {
		if c, err := newHTTPClient(proxyURL, timeout); err == nil {
			return c
		} else {
			logger.Default().Warn(context.Background(),
				"httpx: invalid proxy config, falling back to plain client",
				"error", err, "proxy", proxyURL)
		}
	}
	return &http.Client{Timeout: timeout}
}

// ErrTooLarge reports that a body exceeded the size limit passed to
// ReadAllLimited.
var ErrTooLarge = errors.New("body exceeds size limit")

// ReadAllLimited reads at most max bytes from r. If the body exceeds max it
// returns ErrTooLarge. max+1 bytes are read internally so the caller can
// distinguish "exactly max" from "over limit".
func ReadAllLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, ErrTooLarge
	}
	return body, nil
}

// ReadAllLimitedLenient reads up to max bytes, silently truncating longer
// bodies. It reports whether truncation occurred.
func ReadAllLimitedLenient(r io.Reader, max int64) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	truncated := int64(len(body)) > max
	if truncated {
		body = body[:max]
	}
	return body, truncated, nil
}

// ErrorKind classifies an HTTP error response.
type ErrorKind int

const (
	// KindOther is an unclassified error.
	KindOther ErrorKind = iota
	// KindQuota is quota exhaustion (402, or explicit credit-exhausted text).
	KindQuota
	// KindRateLimit is a transient rate limit (429).
	KindRateLimit
)

// ClassifyError classifies an HTTP status/body into quota exhaustion, a
// transient rate limit, or other. 429s are reported as KindRateLimit unless
// the body shows explicit quota exhaustion (see IsQuotaExhaustedBody).
func ClassifyError(status int, body string) ErrorKind {
	switch {
	case status == http.StatusPaymentRequired:
		return KindQuota
	case status == http.StatusTooManyRequests:
		if IsQuotaExhaustedBody(body) {
			return KindQuota
		}
		return KindRateLimit
	case IsQuotaExhaustedBody(body):
		return KindQuota
	default:
		return KindOther
	}
}

// IsQuotaExhaustedBody detects explicit credit/budget exhaustion wording in
// an API error body. Deliberately narrow — a bare "rate limit" or "quota"
// word is NOT enough (e.g. Brave's 429 body embeds quota_limit metadata even
// when only the per-second limit was hit).
func IsQuotaExhaustedBody(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "no more credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "insufficient credits") ||
		strings.Contains(lower, "budget exceeded")
}

// IsRetryableStatus mirrors the anthropic-sdk-go retry policy: 408 (request
// timeout), 409 (conflict), 429 (rate limit) and all 5xx are retryable.
func IsRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusConflict ||
		code == http.StatusTooManyRequests ||
		code >= http.StatusInternalServerError
}

// JoinURL joins a base URL and a path, avoiding double slashes. The path may
// begin with "/" or not.
func JoinURL(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/sashabaranov/go-openai"
)

// RetryConfig controls the retry behavior of RetryProvider.
type RetryConfig struct {
	// MaxRetries is the number of retries after the initial attempt
	// (0 disables retrying).
	MaxRetries int
	// BaseDelay is the backoff delay after the first failed attempt; it
	// doubles on each subsequent retry, capped at MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff delay.
	MaxDelay time.Duration
}

// RetryProvider wraps a Provider and retries transient failures with
// exponential backoff. Only errors that happen before a stream is
// established can be retried; mid-stream failures are passed through
// untouched (retrying those would require replaying the conversation).
type RetryProvider struct {
	inner Provider
	cfg   RetryConfig
}

// NewRetryProvider returns a Provider that wraps inner with retry behavior.
// Non-positive durations in cfg are replaced with sane defaults.
func NewRetryProvider(inner Provider, cfg RetryConfig) *RetryProvider {
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 500 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 8 * time.Second
	}
	return &RetryProvider{inner: inner, cfg: cfg}
}

func (r *RetryProvider) Name() string  { return r.inner.Name() }
func (r *RetryProvider) Model() string { return r.inner.Model() }

func (r *RetryProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	var lastErr error
	for attempt := 0; attempt <= r.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			logger.FromContext(ctx).Info(ctx, "llm: retrying CreateChat after transient error",
				"attempt", attempt, "maxRetries", r.cfg.MaxRetries, "error", lastErr)
			if err := sleepCtx(ctx, r.backoff(attempt)); err != nil {
				return nil, err
			}
		}
		resp, err := r.inner.CreateChat(ctx, messages, tools, opts)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (r *RetryProvider) CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error) {
	var lastErr error
	for attempt := 0; attempt <= r.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			logger.FromContext(ctx).Info(ctx, "llm: retrying CreateChatStream after transient error",
				"attempt", attempt, "maxRetries", r.cfg.MaxRetries, "error", lastErr)
			if err := sleepCtx(ctx, r.backoff(attempt)); err != nil {
				return nil, err
			}
		}
		ch, err := r.inner.CreateChatStream(ctx, messages, tools, opts)
		if err == nil {
			return ch, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// backoff returns the delay before the given retry attempt (1-based),
// doubling BaseDelay each time and capping at MaxDelay.
func (r *RetryProvider) backoff(attempt int) time.Duration {
	d := r.cfg.BaseDelay << (attempt - 1)
	if d < 0 || d > r.cfg.MaxDelay {
		d = r.cfg.MaxDelay
	}
	return d
}

// sleepCtx sleeps for d, returning early with ctx.Err() if ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isRetryable reports whether err is a transient failure worth retrying:
// network-level errors (connection reset, timeout, unexpected EOF) and
// HTTP 408/409/429/5xx responses. Context cancellation and other 4xx
// client errors are never retried.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Network errors: connection reset/refused, timeouts, DNS failures.
	// *net.OpError (e.g. "read tcp ...: read: connection reset by peer")
	// implements net.Error, so this covers the common mid-flight failures.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Server closed the connection before/while responding.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// OpenAI SDK errors: classify by HTTP status code.
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return retryableStatus(apiErr.HTTPStatusCode)
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return retryableStatus(reqErr.HTTPStatusCode)
	}
	// Anthropic SDK errors. The SDK retries internally by default, but keep
	// the classification correct in case a RetryProvider ever wraps it.
	var anthErr *anthropic.Error
	if errors.As(err, &anthErr) {
		return retryableStatus(anthErr.StatusCode)
	}
	return false
}

// retryableStatus mirrors the anthropic-sdk-go retry policy: 408 (request
// timeout), 409 (conflict), 429 (rate limit) and all 5xx are retryable.
func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusConflict ||
		code == http.StatusTooManyRequests ||
		code >= http.StatusInternalServerError
}

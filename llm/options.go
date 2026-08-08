package llm

import "time"

// ProviderOptions collects optional behavioral parameters for provider
// construction. Zero values mean "use the path's default".
//
// Where each option lands depends on the provider path:
//   - openai (go-openai, no built-in retry): MaxRetries goes to the outer
//     RetryProvider, Timeout sets the HTTP client timeout;
//   - anthropic / openai-res (SDK retries internally): MaxRetries / Timeout
//     map to the SDK's option.WithMaxRetries / option.WithRequestTimeout.
type ProviderOptions struct {
	// MaxRetries overrides the default number of retries after a failed
	// attempt. nil = use the path default (currently 2 everywhere);
	// 0 = disable retrying; n = retry n times.
	MaxRetries *int
	// Timeout overrides the default per-request API timeout;
	// 0 = use the path default (no timeout).
	Timeout time.Duration
}

// ProviderOption mutates ProviderOptions in a functional-options style.
// Constructor variadics accept any number of options; unset fields keep
// their defaults.
type ProviderOption func(*ProviderOptions)

// WithMaxRetries sets the number of retries after a failed attempt
// (0 disables retrying; unset uses the default).
func WithMaxRetries(n int) ProviderOption {
	return func(o *ProviderOptions) { o.MaxRetries = &n }
}

// WithTimeout sets the per-request API timeout (0 uses the default, i.e. no
// timeout).
func WithTimeout(d time.Duration) ProviderOption {
	return func(o *ProviderOptions) { o.Timeout = d }
}

// applyOptions applies a set of options to a ProviderOptions and returns it.
func applyOptions(opts []ProviderOption) ProviderOptions {
	var o ProviderOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

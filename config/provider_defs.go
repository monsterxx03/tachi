package config

import "time"

// Provider type constants — the canonical provider-type identifiers used
// across config (provider type field, default templates) and llm (provider
// construction). Defined here so config stays free of any llm dependency.
const (
	ProviderTypeOpenAI          = "openai"
	ProviderTypeOpenAIResponses = "openai-res"
	ProviderTypeAnthropic       = "anthropic"
)

// ProviderTypes lists every provider type this code can construct, in the order a form should
// offer them.
//
// It exists because a settings page can present the choice instead of accepting a typo:
// nothing validates a provider's `type` at LOAD time, so a misspelling shows up later — when a
// session resolves its provider and llm.NewNamedProvider reaches its default branch with
// "unsupported provider type: …". A list here is not a second source of truth, it is the same
// three constants the construction switch dispatches on: adding a type means adding it in both
// places, which is one decision.
//
// A fresh slice each call: the caller is a bound method whose payload is serialized, and a
// package-level slice would be shared with it.
func ProviderTypes() []string {
	return []string{ProviderTypeOpenAI, ProviderTypeOpenAIResponses, ProviderTypeAnthropic}
}

// ProviderOptions collects optional behavioral parameters for provider
// construction. Zero values mean "use the path's default".
//
// Where each option lands depends on the provider path:
//   - openai (go-openai, no built-in retry): MaxRetries goes to the outer
//     RetryProvider, Timeout sets the HTTP client timeout;
//   - anthropic / openai-res (SDK retries internally): MaxRetries / Timeout
//     map to the SDK's option.WithMaxRetries / option.WithRequestTimeout;
//   - Proxy is the HTTP client's transport on every path (see httpClient in
//     llm/options.go).
type ProviderOptions struct {
	// MaxRetries overrides the default number of retries after a failed
	// attempt. nil = use the path default (currently 2 everywhere);
	// 0 = disable retrying; n = retry n times.
	MaxRetries *int
	// Timeout overrides the default per-request API timeout;
	// 0 = use the path default (no timeout).
	Timeout time.Duration
	// Proxy routes this provider's API requests through the given proxy
	// (http:// / https:// / socks5://). Empty = no explicit proxy, leaving the
	// default transport's environment lookup (HTTP_PROXY / HTTPS_PROXY /
	// NO_PROXY) in effect.
	Proxy string
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

// WithProxy routes requests through a proxy URL (http, https or socks5).
// Empty means "no explicit proxy": the default transport then still honors
// HTTP_PROXY / HTTPS_PROXY from the environment.
func WithProxy(proxyURL string) ProviderOption {
	return func(o *ProviderOptions) { o.Proxy = proxyURL }
}

// WithTimeout sets the per-request API timeout (0 uses the default, i.e. no
// timeout).
func WithTimeout(d time.Duration) ProviderOption {
	return func(o *ProviderOptions) { o.Timeout = d }
}

// PriceOverrides is implemented by both ModelPrice and PriceBand — both
// carry the four unit-price fields. ApplyPriceOverrides copies non-nil
// overrides onto the receiver (nil = keep the field's current value, an
// explicit 0 = free). One method to touch when a price dimension is added.
// The interface lives here (pure data contract); llm's runtime price types
// implement it.
type PriceOverrides interface {
	ApplyPriceOverrides(input, output, cacheRead, cacheCreate *float64)
}

// PricingConfig is the user-facing per-provider pricing override block (the
// SAME type as the resolver — one source of truth for the pricing schema,
// no translation layer. The yaml tags are consumed by this package's YAML
// loading; the pricing semantics (built-in table, versioning, band
// inheritance, time pinning) live in llm.
//
// All fields are optional: nil = not set (flat prices fall back to the
// built-in table; band fields inherit the flat price), explicit 0 = free.
// Timezone anchors band selection (IANA name, e.g. "Asia/Shanghai");
// empty = the instant's own local timezone.
type PricingConfig struct {
	InputPrice              *float64        `yaml:"input_price,omitempty" json:"input_price,omitempty"`
	OutputPrice             *float64        `yaml:"output_price,omitempty" json:"output_price,omitempty"`
	CacheReadInputPrice     *float64        `yaml:"cache_read_input_price,omitempty" json:"cache_read_input_price,omitempty"`
	CacheCreationInputPrice *float64        `yaml:"cache_creation_input_price,omitempty" json:"cache_creation_input_price,omitempty"`
	Timezone                string          `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Bands                   []PriceBandSpec `yaml:"bands,omitempty" json:"bands,omitempty"`

	// USD says the prices in THIS block (the four flat ones and every band's) are quoted in
	// USD per 1M tokens, not CNY. The ledger's unit is CNY, so the resolver converts them at
	// the configured rate (usd_to_cny, else llm.DefaultUSDToCNY) when it reads them out.
	//
	// It exists because most vendors — and every page a price is copied from — quote in USD,
	// and copying a USD figure into a CNY field is a silent factor-of-seven error in the one
	// direction nobody notices (the bill looks cheap). Writing the number as it appears on the
	// page and saying which currency it is keeps the two in step, and makes a rate change
	// reprice every row without re-editing the files.
	//
	// Unlike the built-in table's usdPrices (a flag on the MODEL, in code), this is a field of
	// the user's override block — the two mean the same thing about the same numbers.
	USD bool `yaml:"usd,omitempty" json:"usd,omitempty"`
}

// HasAny reports whether at least one flat override is set — when none is,
// the flat price falls back to the built-in table (a bands-only override
// then inherits the built-in flat prices).
func (p *PricingConfig) HasAny() bool {
	return p.InputPrice != nil || p.OutputPrice != nil ||
		p.CacheReadInputPrice != nil || p.CacheCreationInputPrice != nil
}

// Apply copies non-nil price overrides onto dst (a ModelPrice or PriceBand)
// — the existing convention: unset fields keep their value, an explicit 0
// means free.
func (p *PricingConfig) Apply(dst PriceOverrides) {
	dst.ApplyPriceOverrides(p.InputPrice, p.OutputPrice, p.CacheReadInputPrice, p.CacheCreationInputPrice)
}

// PriceBandSpec is one raw time-of-use band in user-facing form: Start/End
// are "HH:MM" (24h, inclusive start, exclusive end; end <= start wraps past
// midnight, end == start = whole day; minutes must be 0). nil price fields
// inherit the flat price, explicit 0 = free.
// Days restricts the band to a subset of the week (1 = Monday … 7 = Sunday);
// empty = every day. Days with a band miss the weekday → the flat price
// applies (e.g. a workday-only peak band leaves weekends fully off-peak).
type PriceBandSpec struct {
	Name                    string   `yaml:"name,omitempty" json:"name,omitempty"` // band name written to the ledger row ("" = unnamed)
	Days                    []int    `yaml:"days,omitempty" json:"days,omitempty"` // 1=周一 … 7=周日；空 = 每天
	Start                   string   `yaml:"start" json:"start"`
	End                     string   `yaml:"end" json:"end"`
	InputPrice              *float64 `yaml:"input_price,omitempty" json:"input_price,omitempty"`
	OutputPrice             *float64 `yaml:"output_price,omitempty" json:"output_price,omitempty"`
	CacheReadInputPrice     *float64 `yaml:"cache_read_input_price,omitempty" json:"cache_read_input_price,omitempty"`
	CacheCreationInputPrice *float64 `yaml:"cache_creation_input_price,omitempty" json:"cache_creation_input_price,omitempty"`
}

package llm

import (
	"context"
	"net/http"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// applyOptions applies a set of options to a config.ProviderOptions and
// returns it. ProviderOptions / ProviderOption / WithMaxRetries / WithTimeout /
// WithProxy live in config (pure data, shared with the config package); the
// provider constructors consume them here.
func applyOptions(opts []config.ProviderOption) config.ProviderOptions {
	var o config.ProviderOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// httpClient builds the HTTP client every provider path sends through:
// tachiTransport (the session-id header stamp) on top of the proxy transport
// from the options. One builder for all three paths, because "which proxy a
// provider uses" must not be able to differ between them.
//
// The TIMEOUT is deliberately NOT set here — that is the caller's half:
//
//   - the go-openai path sets client.Timeout (it has no per-request timeout of
//     its own);
//   - anthropic / openai-res pass o.Timeout to option.WithRequestTimeout
//     instead, because a client-wide timeout also caps the READING of a
//     streaming response body — it would cut a long stream short, where the
//     SDK's own timeout is per request attempt.
//
// An empty Proxy keeps the default transport: that is http.ProxyFromEnvironment,
// so HTTP_PROXY / HTTPS_PROXY still apply on an unconfigured client. A proxy
// URL that cannot be built into a transport falls back to a direct client with
// a warning rather than failing every turn — resolveProviderConfig already
// refuses such a config at resolve time, so this branch is reached only by a
// direct WithProxy call.
func httpClient(o config.ProviderOptions) *http.Client {
	base := http.RoundTripper(http.DefaultTransport)
	if o.Proxy != "" {
		transport, err := httpx.NewTransport(o.Proxy)
		switch {
		case err != nil:
			logger.New("llm").Warn(context.Background(),
				"invalid proxy config, sending provider requests directly",
				"error", err, "proxy", o.Proxy)
		case transport != nil:
			base = transport
		}
	}
	return &http.Client{Transport: &tachiTransport{base: base}}
}

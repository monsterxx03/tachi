// Package proxy provides a helper to create HTTP clients with optional
// SOCKS5 / HTTP / HTTPS proxy support. Both the web_search tool and the
// MCP HTTP transport use this so that proxy configuration is handled in
// a single place.
package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// NewHTTPClient creates an *http.Client with the given timeout. When
// proxyURL is non-empty it is parsed and used to configure the transport
// accordingly:
//
//	http://host:port    – HTTP proxy
//	https://host:port   – HTTPS proxy (CONNECT tunnel)
//	socks5://host:port  – SOCKS5 proxy
//
// An empty proxyURL returns a plain client (no proxy).
func NewHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if proxyURL == "" {
		return &http.Client{Timeout: timeout}, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %w", proxyURL, err)
	}

	var transport *http.Transport

	switch u.Scheme {
	case "http", "https":
		transport = &http.Transport{
			Proxy: http.ProxyURL(u),
		}

	case "socks5":
		dialer, err := proxy.SOCKS5("tcp", u.Host, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %q: %w", proxyURL, err)
		}
		// Wrap the SOCKS5 dialer in a transport that uses it for both
		// plain TCP and TLS connections.
		transport = &http.Transport{
			Dial: dialer.Dial,
		}

	default:
		return nil, fmt.Errorf(
			"unsupported proxy scheme %q (supported: http, https, socks5)",
			u.Scheme,
		)
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

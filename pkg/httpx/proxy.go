package httpx

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	goproxy "golang.org/x/net/proxy"
)

// NewTransport returns the transport a client should use to reach through
// proxyURL:
//
//	http://host:port    – HTTP proxy
//	https://host:port   – HTTPS proxy (CONNECT tunnel)
//	socks5://host:port  – SOCKS5 proxy
//
// An empty proxyURL returns nil — nil MEANS "the caller's own default", and a
// caller installing it (or leaving it unset) keeps http.ProxyFromEnvironment
// working, so HTTP_PROXY / HTTPS_PROXY / NO_PROXY still apply to a client with
// nothing explicitly configured.
//
// An unusable URL or an unsupported scheme is an error rather than a silent
// fallback, because only the caller knows whether going direct is acceptable
// (NewClient warns and goes direct; a caller for which a proxy is mandatory
// should refuse to build). Every HTTP client built from a proxy URL goes
// through here — NewClient and the LLM providers' clients alike. NewDialer
// below parses its own, because a websocket dialer needs a dial function
// rather than a transport.
func NewTransport(proxyURL string) (*http.Transport, error) {
	if proxyURL == "" {
		return nil, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %w", proxyURL, err)
	}

	switch u.Scheme {
	case "http", "https":
		return &http.Transport{Proxy: http.ProxyURL(u)}, nil

	case "socks5":
		dialer, err := goproxy.SOCKS5("tcp", u.Host, nil, goproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %q: %w", proxyURL, err)
		}
		return &http.Transport{
			// DialContext (not the deprecated Dial) so the transport can
			// cancel in-flight dials; proxy.Dialer only exposes Dial, so
			// adapt it here (the ctx parameter is unused by the socks5
			// dialer).
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}, nil

	default:
		return nil, fmt.Errorf(
			"unsupported proxy scheme %q (supported: http, https, socks5)",
			u.Scheme,
		)
	}
}

// newHTTPClient creates an *http.Client with the given timeout. When
// proxyURL is non-empty it is parsed and used (see NewTransport). An empty
// proxyURL returns a plain client (no proxy). This is the internal
// implementation behind NewClient; callers that want graceful degradation on
// proxy errors should use NewClient instead.
func newHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport, err := NewTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout}
	if transport != nil {
		// Only when there IS one: a nil *http.Transport stored in the interface
		// field is not the same as an unset one — it would be a RoundTripper
		// whose methods panic.
		client.Transport = transport
	}
	return client, nil
}

// NewDialer returns a function suitable for use as a websocket.Dialer.NetDial
// or net.Dialer that routes TCP connections through the given proxy.
//
// Supported schemes:
//
//	http://host:port    – HTTP CONNECT proxy
//	https://host:port   – HTTPS CONNECT proxy (CONNECT over TLS to proxy)
//	socks5://host:port  – SOCKS5 proxy
//
// Returns a no-op forward dialer when proxyURL is empty.
func NewDialer(proxyURL string) (func(network, addr string) (net.Conn, error), error) {
	if proxyURL == "" {
		return net.Dial, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %w", proxyURL, err)
	}

	switch u.Scheme {
	case "http":
		return httpConnectDialer(u.Host, false), nil
	case "https":
		return httpConnectDialer(u.Host, true), nil
	case "socks5":
		d, err := goproxy.SOCKS5("tcp", u.Host, nil, goproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %q: %w", proxyURL, err)
		}
		return d.Dial, nil
	default:
		return nil, fmt.Errorf(
			"unsupported proxy scheme %q for dialer (supported: http, https, socks5)",
			u.Scheme,
		)
	}
}

// httpConnectDialer returns a dial function that establishes a TCP connection
// through an HTTP CONNECT proxy.
func httpConnectDialer(proxyHost string, useTLS bool) func(network, addr string) (net.Conn, error) {
	return func(network, addr string) (net.Conn, error) {
		var conn net.Conn
		var err error

		// Connect to the proxy server.
		if useTLS {
			// HTTPS CONNECT proxy: establish TLS session with the proxy first,
			// then send CONNECT request over the encrypted connection.
			d := &net.Dialer{Timeout: 30 * time.Second}
			conn, err = tls.DialWithDialer(d, "tcp", proxyHost, &tls.Config{})
		} else {
			conn, err = net.DialTimeout("tcp", proxyHost, 30*time.Second)
		}
		if err != nil {
			return nil, fmt.Errorf("connect to proxy %s: %w", proxyHost, err)
		}

		// Send CONNECT request.
		reqStr := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
		if _, err := conn.Write([]byte(reqStr)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT write: %w", err)
		}

		// Read HTTP response with a 30-second deadline.
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)

		// Clear the deadline after reading.
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT response: %w", err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT returned %s", resp.Status)
		}

		return conn, nil
	}
}

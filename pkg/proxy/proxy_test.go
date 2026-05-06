package proxy

import (
	"testing"
	"time"
)

func TestNewHTTPClient_EmptyURL(t *testing.T) {
	client, err := NewHTTPClient("", 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Timeout != 10*time.Second {
		t.Errorf("expected timeout 10s, got %v", client.Timeout)
	}
	if client.Transport != nil {
		t.Error("expected nil transport for empty proxy URL")
	}
}

func TestNewHTTPClient_HTTPScheme(t *testing.T) {
	client, err := NewHTTPClient("http://127.0.0.1:8080", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Transport == nil {
		t.Fatal("expected non-nil transport for HTTP proxy")
	}
	if client.Timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", client.Timeout)
	}
}

func TestNewHTTPClient_SOCKS5Scheme(t *testing.T) {
	client, err := NewHTTPClient("socks5://127.0.0.1:1080", 3*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Transport == nil {
		t.Fatal("expected non-nil transport for SOCKS5 proxy")
	}
	if client.Timeout != 3*time.Second {
		t.Errorf("expected timeout 3s, got %v", client.Timeout)
	}
}

func TestNewHTTPClient_InvalidURL(t *testing.T) {
	_, err := NewHTTPClient(" ://bad", 1*time.Second)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNewHTTPClient_UnsupportedScheme(t *testing.T) {
	_, err := NewHTTPClient("ftp://127.0.0.1:21", 1*time.Second)
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

package netutil

import (
	"errors"
	"net"
	"testing"
)

func TestIsReservedIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		// Public addresses.
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false},
		{"100.128.0.1", false},
		{"198.20.0.1", false},
		{"2001:4860:4860::8888", false},     // Google DNS
		{"2606:4700:4700::1111", false},     // Cloudflare DNS
		{"2a00:1450:4001:82f::200e", false}, // google.com

		// RFC 1918 private + IPv6 ULA.
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},

		// Loopback.
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"::1", true},

		// Link-local + cloud metadata.
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		{"fe80::1", true},

		// CGNAT (RFC 6598).
		{"100.64.0.1", true},
		{"100.127.255.254", true},

		// TEST-NET (RFC 5737).
		{"192.0.2.1", true},
		{"198.51.100.1", true},
		{"203.0.113.1", true},

		// Benchmarking (RFC 2544).
		{"198.18.0.1", true},
		{"198.19.255.255", true},

		// Reserved / future use, broadcast.
		{"240.0.0.1", true},
		{"255.255.255.255", true},

		// Multicast + unspecified.
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"0.0.0.0", true},

		// IPv6 documentation prefix.
		{"2001:db8::1", true},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP %q", tt.ip)
			}
			if got := IsReservedIP(ip); got != tt.want {
				t.Errorf("IsReservedIP(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestHostIsReserved_IPLiteral(t *testing.T) {
	// IP literals short-circuit DNS — the real resolver never matters here,
	// so no stubbing is needed.
	tests := []struct {
		host string
		want bool
	}{
		{"10.0.0.1", true},
		{"8.8.8.8", false},
		{"[::1]:8080", true},
		{"127.0.0.1:9000", true},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got, err := HostIsReserved(tt.host)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("HostIsReserved(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestHostIsReserved_Hostname(t *testing.T) {
	orig := LookupIP
	t.Cleanup(func() { LookupIP = orig })

	errStub := errors.New("stub lookup failure")
	pub := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")}, nil
	}
	mixed := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1")}, nil
	}
	reserved := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.168.1.1")}, nil
	}
	empty := func(host string) ([]net.IP, error) { return nil, nil }
	fail := func(host string) ([]net.IP, error) { return nil, errStub }

	tests := []struct {
		name    string
		lookup  func(string) ([]net.IP, error)
		want    bool
		wantErr bool
	}{
		{"public only", pub, false, false},
		{"any reserved wins", mixed, true, false},
		{"reserved only", reserved, true, false},
		{"empty result", empty, false, true},
		{"lookup failure", fail, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			LookupIP = tt.lookup
			got, err := HostIsReserved("internal.example")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("HostIsReserved(internal.example) = %v, want %v", got, tt.want)
			}
		})
	}
}

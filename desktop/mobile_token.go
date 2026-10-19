package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// The mobile token is a CREDENTIAL, not a preference, so it does not live in config.yaml
// and the settings page never edits it leaf by leaf: it has its own file, its own mode, and
// one button that replaces it (see RegenerateMobileToken).
//
// Deleting the file is also a way out: the next pairing regenerates one, which is what a
// user who has lost track of the old value wants.
const mobileTokenFileName = "mobile.json"

// mobileToken is what the file holds. created_at is for the settings card ("paired at …"),
// not for any expiry: V1 has no rotation schedule.
type mobileToken struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

// mobileTokenPath is the file's location — beside config.yaml, in whatever base dir this
// process was started with (config.BaseDir, so a smoke/test sandbox gets its own).
func mobileTokenPath() string {
	return filepath.Join(config.BaseDir(), mobileTokenFileName)
}

// readMobileTokenFile returns the stored token, or ok=false when there is no usable file.
// An unreadable file is reported, never silently replaced: a credential that vanishes on a
// parse error would invalidate every phone the user had paired, without telling them why.
func readMobileTokenFile() (mobileToken, bool, error) {
	path := mobileTokenPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return mobileToken{}, false, nil
	}
	if err != nil {
		return mobileToken{}, false, err
	}
	var t mobileToken
	if err := json.Unmarshal(b, &t); err != nil {
		return mobileToken{}, false, fmt.Errorf("mobile token file %s: %w", path, err)
	}
	if t.Token == "" {
		return mobileToken{}, false, nil
	}
	return t, true, nil
}

// loadOrCreateMobileToken is the one entry point that answers "which token is in force".
// It creates one on first use (the settings card has something to show before the phone has
// ever connected) and keeps the file at 0600.
func loadOrCreateMobileToken() (string, error) {
	t, ok, err := readMobileTokenFile()
	if err != nil {
		return "", err
	}
	if ok {
		return t.Token, nil
	}
	return writeNewMobileToken()
}

// writeNewMobileToken generates a token and replaces the file atomically.
//
// 0600 is the point of a separate file: the token lets its holder run commands on this
// machine, so it must not be readable by any other account on it. The temp file is created
// with that mode BEFORE anything is written, and the rename keeps it.
func writeNewMobileToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mobile token: %w", err)
	}
	t := mobileToken{Token: base64.RawURLEncoding.EncodeToString(raw), CreatedAt: time.Now()}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	dir := config.BaseDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := mobileTokenPath()
	tmp, err := os.CreateTemp(dir, ".mobile-*.json")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return t.Token, nil
}

// mobileTokenEqual compares a presented token with the live one in constant time. Length is
// compared too, but by the same primitive: a short-circuit on length is a timing signal, and
// the length of a fixed-size random token says nothing useful anyway.
func mobileTokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// MobilePairingVO is what the settings card needs to pair a phone: where to point it, the
// token to present, and the exact payload the QR code encodes (mobile/src/api/qr.ts parses
// this shape, so the card must not build a second one).
type MobilePairingVO struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
	// URL is the address a phone on the same network can reach, or "" when this machine has
	// no non-loopback IPv4 address to offer (then the card shows the listen address and says
	// so rather than printing 127.0.0.1, which no phone could use).
	URL   string `json:"url"`
	Token string `json:"token"`
	// Payload is {"v":1,"url":…,"token":…} — the QR's content, verbatim.
	Payload string `json:"payload"`
	// Error is non-empty when the listener could not start (port taken, bad address), so the
	// card can say why instead of showing a URL that answers nothing.
	Error string `json:"error,omitempty"`
}

// GetMobilePairing returns everything the settings card renders. It creates the token on
// first call: a card that says "no token yet" would leave the user nothing to pair with.
func (s *AgentService) GetMobilePairing() MobilePairingVO {
	d := s.desk
	cfg := d.config()
	out := MobilePairingVO{}
	if cfg != nil {
		out.Enabled = cfg.Mobile.Enabled
		out.Listen = cfg.Mobile.MobileAddress()
	}
	token, err := loadOrCreateMobileToken()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Token = token
	out.URL = mobilePairingURL(out.Listen)
	payload, _ := json.Marshal(map[string]any{"v": 1, "url": out.URL, "token": token})
	out.Payload = string(payload)
	if m := d.mobileServer(); m != nil {
		if err := m.listenError(); err != "" {
			out.Error = err
		}
	}
	return out
}

// RegenerateMobileToken replaces the token and DROPS every live connection.
//
// Dropping them is the other half of "invalidate": a paired phone holds its socket open, and
// a token replaced underneath it would otherwise keep working for as long as that socket
// lived — which is exactly the phone the user is trying to lock out.
// It answers "ok" or the reason it could not, like the other bindings: the value itself is
// the CARD's to re-read (GetMobilePairing), and a caller that had to tell a token apart from a
// refusal message would be guessing.
func (s *AgentService) RegenerateMobileToken() string {
	token, err := writeNewMobileToken()
	if err != nil {
		return err.Error()
	}
	if m := s.desk.mobileServer(); m != nil {
		m.setToken(token)
	}
	return "ok"
}

// mobilePairingURL turns a listen address into one a phone can dial: the port survives, the
// host becomes this machine's LAN address (0.0.0.0 and 127.0.0.1 are not reachable from
// another device), and the scheme is http because V1 is plain HTTP on the LAN.
func mobilePairingURL(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		port = "8787"
	}
	host := firstLANIPv4()
	if host == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, port)
}

// firstLANIPv4 returns the first non-loopback IPv4 address of an up interface. "" means this
// machine is on no network we can name a phone with.
func firstLANIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

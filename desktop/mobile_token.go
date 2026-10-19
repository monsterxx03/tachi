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
	"strings"
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
	// Interface is the network interface the pairing URL advertises ("" = automatic: the first
	// physical one with an IPv4). It is stored by NAME rather than by address so a DHCP lease
	// change does not silently point the QR at an address nobody holds any more.
	Interface string `json:"interface,omitempty"`
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
	// Regenerating the token must NOT forget which interface the reader picked: the two are
	// different decisions, and the card would silently jump back to "automatic".
	prev, ok, _ := readMobileTokenFile()
	if !ok {
		prev = mobileToken{}
	}
	t := mobileToken{
		Token:     base64.RawURLEncoding.EncodeToString(raw),
		CreatedAt: time.Now(),
		Interface: prev.Interface,
	}
	if err := writeMobileTokenFile(t); err != nil {
		return "", err
	}
	return t.Token, nil
}

// writeMobileTokenFile is the module's only writer: atomic, 0600, and it never drops a field it
// was not asked to change (the token and the interface are two separate decisions).
func writeMobileTokenFile(t mobileToken) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	dir := config.BaseDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mobile-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, mobileTokenPath())
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
	// Host is the IPv4 address the URL was built from ("" when there is nothing usable).
	Host string `json:"host"`
	// Interface is the one the reader picked, or "" for automatic.
	Interface string `json:"interface"`
	// Candidates are the addresses this machine can offer. More than one is the normal case on
	// a machine with Wi-Fi + Ethernet + a VPN, and only a person knows which network the phone
	// is on — so the card lists them instead of betting on the kernel's ordering.
	Candidates []MobilePairingCandidate `json:"candidates"`
	// Payload is {"v":1,"url":…,"token":…} — the QR's content, verbatim.
	Payload string `json:"payload"`
	// Error is non-empty when the listener could not start (port taken, bad address), so the
	// card can say why instead of showing a URL that answers nothing.
	Error string `json:"error,omitempty"`
}

// MobilePairingCandidate is one address the pairing URL could use.
type MobilePairingCandidate struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	// URL is what the phone would dial if this candidate were chosen — built here, once, so the
	// card never assembles a URL of its own.
	URL string `json:"url"`
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
	_, port, err := net.SplitHostPort(out.Listen)
	if err != nil || port == "" {
		port = "8787"
	}
	out.Candidates = mobileLANCandidates(port)
	want := ""
	if st, ok, _ := readMobileTokenFile(); ok {
		want = st.Interface
	}
	if c, ok := chooseMobileCandidate(out.Candidates, want); ok {
		out.Interface = c.Interface
		out.Host = c.Address
		out.URL = c.URL
	}
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

// mobilePairingURL is what a phone dials: the scheme is http because V1 is plain HTTP on the
// LAN, and the host is one of this machine's own addresses (0.0.0.0 and 127.0.0.1 are not
// reachable from another device).
func mobilePairingURL(host, port string) string {
	if host == "" || port == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, port)
}

// mobileLANCandidates lists the addresses the card can offer, PHYSICAL interfaces first (see
// pickLANIPv4 for why the order matters).
func mobileLANCandidates(port string) []MobilePairingCandidate {
	out := make([]MobilePairingCandidate, 0, 4)
	for _, c := range lanCandidates() {
		if !lanUsable(c) {
			continue
		}
		out = append(out, MobilePairingCandidate{
			Interface: c.name,
			Address:   c.ip.String(),
			URL:       mobilePairingURL(c.ip.String(), port),
		})
	}
	// Stable partition: physical interfaces first, then everything else.
	physical := make([]MobilePairingCandidate, 0, len(out))
	other := make([]MobilePairingCandidate, 0, len(out))
	for _, c := range out {
		if physicalName(c.Interface) {
			physical = append(physical, c)
		} else {
			other = append(other, c)
		}
	}
	return append(physical, other...)
}

// chooseMobileCandidate answers which address the URL uses: the one the reader picked, when that
// interface still has a usable address; otherwise the automatic one (the first, i.e. a physical
// interface). "" means the machine has no usable address at all.
func chooseMobileCandidate(cands []MobilePairingCandidate, wantInterface string) (MobilePairingCandidate, bool) {
	if wantInterface != "" {
		for _, c := range cands {
			if c.Interface == wantInterface {
				return c, true
			}
		}
	}
	if len(cands) > 0 {
		return cands[0], true
	}
	return MobilePairingCandidate{}, false
}

// SetMobilePairingHost records which interface the pairing URL should use. "" means automatic.
//
// It writes the interface's own file (mobile.json) rather than config.yaml: this is a property
// of the phone interface, not of the agent's preferences, and the card that shows it is not part
// of the settings form's save batch.
func (s *AgentService) SetMobilePairingHost(iface string) string {
	cands := mobileLANCandidates("1") // the ports do not matter for the check
	if iface != "" {
		found := false
		for _, c := range cands {
			if c.Interface == iface {
				found = true
				break
			}
		}
		if !found {
			return "这台机器上没有这个网络接口（或它当前没有可用的 IPv4 地址）"
		}
	}
	if err := setMobileInterface(iface); err != nil {
		return err.Error()
	}
	return "ok"
}

// setMobileInterface rewrites the interface choice, keeping the token.
func setMobileInterface(iface string) error {
	t, ok, err := readMobileTokenFile()
	if err != nil {
		return err
	}
	if !ok {
		// No token yet: create one, then record the choice on top of it.
		if _, err := writeNewMobileToken(); err != nil {
			return err
		}
		t, ok, err = readMobileTokenFile()
		if err != nil || !ok {
			return err
		}
	}
	t.Interface = iface
	return writeMobileTokenFile(t)
}

// lanCandidate is one interface address as the pairing URL weighs it. Point-to-point links
// (utun/tun/tap) carry VPN addresses that only exist through that tunnel; bridges and vm
// interfaces (bridge100, vmenet0, docker0) carry container/emulator networks. A phone on the
// same Wi-Fi can reach NONE of them.
type lanCandidate struct {
	name         string
	ip           net.IP
	up           bool
	pointToPoint bool
}

// lanCandidates is every IPv4 address this machine holds, with what decides whether a phone can
// reach it. The ORDER is the kernel's; the preference between them is pickLANIPv4's business.
func lanCandidates() []lanCandidate {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	cands := make([]lanCandidate, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil {
				continue
			}
			cands = append(cands, lanCandidate{
				name:         iface.Name,
				ip:           v4,
				up:           iface.Flags&net.FlagUp != 0,
				pointToPoint: iface.Flags&net.FlagPointToPoint != 0,
			})
		}
	}
	return cands
}

// lanUsable is the ONE rule for "a phone on the same network could dial this".
func lanUsable(c lanCandidate) bool {
	return c.up && !c.pointToPoint && c.ip.To4() != nil &&
		!c.ip.IsLoopback() && !c.ip.IsLinkLocalUnicast()
}

// firstLANIPv4 returns the automatic choice (the address a phone on the same network should
// dial when nobody has picked one). "" means this machine has no address we can name one with.
func firstLANIPv4() string { return pickLANIPv4(lanCandidates()) }

// pickLANIPv4 prefers a PHYSICAL-looking interface (en0, eth0, wlan0…) and only falls back to
// any other up, non-tunnel address when there is none — the order interfaces come back in is
// not a promise, and the address on a VPN is the one that never works from the phone.
func pickLANIPv4(cands []lanCandidate) string {
	for _, wantPhysical := range []bool{true, false} {
		for _, c := range cands {
			if !lanUsable(c) || physicalName(c.name) != wantPhysical {
				continue
			}
			return c.ip.String()
		}
	}
	return ""
}

// physicalName names the interface kinds a LAN peer actually shares: macOS en0/en1, Linux
// eth0/enp0s3/wlan0/wlp2s0. Everything else (bridge*, vmenet*, utun*, docker*, virbr*) is a
// virtual network that exists only inside this machine.
func physicalName(name string) bool {
	for _, prefix := range []string{"en", "eth", "wl"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

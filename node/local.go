package node

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const (
	// localFileName is what a hosting process publishes for the other Tachi
	// processes on this machine: where its local interface listens and the
	// credential to talk to it. Private (0600) — it carries that credential.
	localFileName = "local.json"

	// localRouteHandshake is the local interface's identity answer. One route in
	// this slice; the snapshot / delegation / task routes arrive with their slices.
	localRouteHandshake = "/local/v1/handshake"

	// maxLocalErrorBody caps how much of an unexpected response body the client
	// copies into its error. Loopback and a small JSON answer: anything longer is
	// not worth carrying around.
	maxLocalErrorBody = 512
)

// localInfo is the published connection info. It carries an instance id as well as
// an address because the address alone cannot tell a reader whether the process
// answering there is the one the file described — a port is reused, a process is
// replaced, and a stale file would otherwise look authoritative forever.
//
// The file is deliberately NOT removed on shutdown: the next host overwrites it,
// and every reader checks InstanceID against what actually answers before
// trusting a single field.
type localInfo struct {
	InstanceID string    `json:"instance_id"`
	NodeID     string    `json:"node_id"`
	Addr       string    `json:"addr"`
	PID        int       `json:"pid"`
	APIVersion int       `json:"api_version"`
	Token      string    `json:"token"`
	StartedAt  time.Time `json:"started_at"`
}

// Handshake is the local interface's answer to "who are you". It never carries the
// credential.
type Handshake struct {
	InstanceID string `json:"instance_id"`
	NodeID     string `json:"node_id"`
	APIVersion int    `json:"api_version"`
	PID        int    `json:"pid"`
}

func (info *localInfo) validate() error {
	switch {
	case info.InstanceID == "":
		return fmt.Errorf("node: local info has no instance_id")
	case info.NodeID == "":
		return fmt.Errorf("node: local info has no node_id")
	case info.Token == "":
		return fmt.Errorf("node: local info has no token")
	case info.APIVersion == 0:
		return fmt.Errorf("node: local info has no api_version")
	}
	if _, _, err := net.SplitHostPort(info.Addr); err != nil {
		return fmt.Errorf("node: local info addr %q: %w", info.Addr, err)
	}
	return nil
}

func readLocalInfo(dir string) (*localInfo, error) {
	var info localInfo
	if err := fileutil.ReadJSON(filepath.Join(dir, localFileName), &info); err != nil {
		return nil, err
	}
	if err := info.validate(); err != nil {
		return nil, err
	}
	return &info, nil
}

// publishLocalInfo is the ONE write path for local.json: atomic, private, and the
// only place that knows the file's name.
func publishLocalInfo(dir string, info *localInfo) error {
	if err := info.validate(); err != nil {
		return err
	}
	if err := fileutil.AtomicWriteJSONPrivate(filepath.Join(dir, localFileName), info); err != nil {
		return fmt.Errorf("node: publish local info: %w", err)
	}
	return nil
}

// localHandler serves the local control interface.
//
// The listener is loopback-only, and every request still has to carry the
// credential: being reachable only from this machine is not authentication.
// Another local user, or any process that guessed a port, can connect — what it
// cannot do is read the 0600 file the credential was written to.
func localHandler(info *localInfo) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+localRouteHandshake, func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r, info.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tachi-node"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, Handshake{
			InstanceID: info.InstanceID,
			NodeID:     info.NodeID,
			APIVersion: info.APIVersion,
			PID:        info.PID,
		})
	})
	return mux
}

func bearerMatches(r *http.Request, want string) bool {
	const scheme = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, scheme) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, scheme)), []byte(want)) == 1
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Encoding a small struct straight into a live response: a failure here means
	// the peer is gone, and there is nothing left to report it to but the status
	// line, which is already sent.
	_ = json.NewEncoder(w).Encode(v)
}

// newRandomHex returns n random bytes as hex. Instance ids and credentials both
// come from here: guessing either must not be cheaper than guessing the other.
func newRandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read cannot fail on the platforms we ship: if it does, the
		// process has no entropy and an id that is predictable is worse than not
		// starting.
		panic(fmt.Sprintf("node: crypto/rand: %v", err))
	}
	return hex.EncodeToString(buf)
}

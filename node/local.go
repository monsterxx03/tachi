package node

import (
	"context"
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

	// localRouteEndWork stops whatever is executing in one session — the machine owner's own
	// escape hatch, reached only from this machine (see runner.EndSessionWork for why this is
	// not the protocol's cancel). It takes the session id as a query parameter: a reader has a
	// session, never a task id.
	localRouteEndWork = "/local/v1/session:end"
	// localQuerySession names that parameter, in one place because the client and the handler
	// have to agree on it.
	localQuerySession = "session"

	// maxLocalErrorBody caps how much of an unexpected response body the client
	// copies into its error. Loopback and a small JSON answer: anything longer is
	// not worth carrying around.
	maxLocalErrorBody = 512
)

// endWorkResult is what the end-work route answers with: how many executions it stopped. Zero is
// a real answer — "nothing was running in that session" — and it is the one an entry point acts
// on when it is about to delete the session.
type endWorkResult struct {
	Ended int `json:"ended"`
}

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

// workEnder is the host's half that an ENTRY POINT on this machine can reach: stop whatever is
// executing in a session.
//
// It is a separate optional interface rather than a method on the executor, because only the real
// task runner has executions to stop — the placeholder echo executor an integration test uses has
// none, and a node built on it must answer "nothing to stop" rather than fail the route.
type workEnder interface {
	EndSessionWork(ctx context.Context, sessionID string) (int, error)
}

// localHandler serves the local control interface.
//
// The listener is loopback-only, and every request still has to carry the
// credential: being reachable only from this machine is not authentication.
// Another local user, or any process that guessed a port, can connect — what it
// cannot do is read the 0600 file the credential was written to.
//
// The surface is passed in for the ONE route that has to reach the executor (ending a session's
// work): that capability lives in the hosting process, and this interface is how the other
// entries on this machine ask for it. It may be nil — a node whose surface has no such half
// answers "nothing to end".
func localHandler(info *localInfo, surface *a2aSurface) http.Handler {
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
	mux.HandleFunc("POST "+localRouteEndWork, func(w http.ResponseWriter, r *http.Request) {
		if !bearerMatches(r, info.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tachi-node"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sessionID := r.URL.Query().Get(localQuerySession)
		if sessionID == "" {
			http.Error(w, "a session id is required", http.StatusBadRequest)
			return
		}
		var ender workEnder
		if surface != nil && surface.state != nil {
			ender, _ = surface.state.(workEnder)
		}
		if ender == nil {
			// Nothing on this node can be running work that it knows how to stop: the answer is
			// "nothing ended", not a failure — the caller asked about ITS session.
			writeJSON(w, endWorkResult{Ended: 0})
			return
		}
		ended, err := ender.EndSessionWork(r.Context(), sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, endWorkResult{Ended: ended})
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

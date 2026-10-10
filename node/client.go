package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Errors a caller decides on. Each one answers a DIFFERENT question, and the
// difference matters: "there is no service right now" may be worth waiting out,
// while "the service that answers is not the instance you attached to" is a fact
// about this client's stale handle and must never be papered over.
var (
	// ErrUnavailable — nothing usable is published (yet, or any more). The attach
	// path waits this one out until its budget runs out.
	ErrUnavailable = errors.New("node: no usable local node service")

	// ErrUnreachable — the published address did not answer.
	ErrUnreachable = errors.New("node: local node service unreachable")

	// ErrUnauthorized — the service is there and refused this credential.
	ErrUnauthorized = errors.New("node: local node service rejected the credential")

	// ErrInstanceChanged — a different service instance is (or is now) answering
	// where this client attached. The attached handle is stale.
	ErrInstanceChanged = errors.New("node: local node service instance changed")

	// ErrIdentityMismatch — the answering node's identity is not this data dir's
	// identity. Either the file or the peer is wrong; neither may be guessed at.
	ErrIdentityMismatch = errors.New("node: local node identity does not match this data dir")

	// ErrVersionMismatch — the answering node speaks a different API version.
	ErrVersionMismatch = errors.New("node: local node API version is incompatible")
)

// Client is the local control interface as seen from a Tachi process on this
// machine. It is created by StartOrAttach — from the published info when this
// process attached, and from its OWN published info when it hosts, so a hosting
// process and an attaching one take exactly the same code path from here on.
type Client struct {
	dataDir    string
	baseURL    string
	token      string
	instanceID string
	nodeID     string
	apiVersion int
	hc         *http.Client
}

func newClient(dataDir string, info *localInfo) *Client {
	return &Client{
		dataDir:    dataDir,
		baseURL:    "http://" + info.Addr,
		token:      info.Token,
		instanceID: info.InstanceID,
		nodeID:     info.NodeID,
		apiVersion: info.APIVersion,
		hc:         &http.Client{Timeout: localHTTPTimeout},
	}
}

// InstanceID is the service instance this client attached to. A caller that
// persists anything against the node records this as well: it is what tells a
// later reader whether the answers still describe the same process.
func (c *Client) InstanceID() string { return c.instanceID }

// NodeID is the answering node's stable identity.
func (c *Client) NodeID() string { return c.nodeID }

// APIVersion is the local interface version the client speaks.
func (c *Client) APIVersion() int { return c.apiVersion }

// Handshake asks the local service who it is.
func (c *Client) Handshake(ctx context.Context) (*Handshake, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+localRouteHandshake, nil)
	if err != nil {
		return nil, fmt.Errorf("node: local request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.classify(fmt.Errorf("%w: %s: %w", ErrUnreachable, c.baseURL, err))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, c.classify(fmt.Errorf("%w: %s", ErrUnauthorized, c.baseURL))
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxLocalErrorBody))
		return nil, c.classify(fmt.Errorf("%w: %s: status %d: %s",
			ErrUnreachable, c.baseURL, resp.StatusCode, strings.TrimSpace(string(body))))
	}

	var hs Handshake
	if err := json.NewDecoder(resp.Body).Decode(&hs); err != nil {
		return nil, c.classify(fmt.Errorf("%w: %s: decode handshake: %w", ErrUnreachable, c.baseURL, err))
	}
	if hs.APIVersion != APIVersion {
		return nil, fmt.Errorf("%w: local API %d, this build speaks %d", ErrVersionMismatch, hs.APIVersion, APIVersion)
	}
	return &hs, nil
}

// Ping re-checks that the instance this client attached to is still the one
// answering. It exists so a stale handle fails as a stale handle instead of
// having its next real call misread as a transient error.
func (c *Client) Ping(ctx context.Context) error {
	hs, err := c.Handshake(ctx)
	if err != nil {
		return err
	}
	if hs.InstanceID != c.instanceID {
		return fmt.Errorf("%w: attached to %s, %s is answering", ErrInstanceChanged, c.instanceID, hs.InstanceID)
	}
	return nil
}

// classify explains a failure the caller cannot see from the error alone.
//
// A refused credential or a dead address has two very different causes: the
// instance we attached to is gone (a new one took over, and its token and port
// are both new), or something is genuinely wrong with this credential. Reading
// what is published NOW separates them — if it names a different instance, that
// is the answer, and it is a fact rather than a guess.
func (c *Client) classify(err error) error {
	if c.dataDir == "" {
		return err
	}
	info, rerr := readLocalInfo(c.dataDir)
	if rerr != nil {
		return err
	}
	if info.InstanceID != c.instanceID {
		return fmt.Errorf("%w: attached to %s, %s is published now", ErrInstanceChanged, c.instanceID, info.InstanceID)
	}
	return err
}

// waitForRetry reports whether an attach attempt is worth repeating inside the
// caller's budget. A definitive answer (wrong identity, incompatible version,
// refused credential for the instance that IS published) ends the wait at once;
// anything that may simply be a host still coming up or being replaced is retried.
func waitForRetry(err error) bool {
	switch {
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrUnreachable), errors.Is(err, ErrInstanceChanged):
		return true
	default:
		return false
	}
}

// classifyAttachError is the attach path's own exit condition: it must never
// outlive its budget, and it reports WHY the last attempt failed.
func attachTimedOut(dir string, last error, budget time.Duration) error {
	if last == nil {
		last = ErrUnavailable
	}
	return fmt.Errorf("node: attach to %s: %w after %s: %w", dir, ErrUnavailable, budget, last)
}

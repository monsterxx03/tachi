package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/pkg/flock"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Handle is this process's view of the node: either the process that hosts the
// local interface (Hosting reports true, and Close stops it) or one attached to
// another process's (Hosting false, Close is a no-op).
//
// Either way Client is the ONE way to talk to the node, so "host" cannot quietly
// behave differently from "attached" — the host reaches itself over the same
// loopback interface, with the same credential check, as everybody else.
type Handle struct {
	dataDir  string
	identity *Identity
	logger   *logger.Logger

	hosting bool
	lock    *flock.Lock
	store   *taskStore
	ln      net.Listener
	srv     *http.Server
	remote  *a2aSurface
	info    *localInfo
	client  *Client
}

// StartOrAttach makes this process a node, or attaches to the one already running
// for this data dir.
//
// The lock decides, not the file: whoever takes it hosts, everybody else attaches.
// It is also why a stale local.json is never trusted — it is only ever read by a
// process that FAILED to take the lock, which means a live host exists, and that
// host's handshake is what settles every field.
func StartOrAttach(ctx context.Context, opts Options) (*Handle, error) {
	o, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.DataDir, dataDirPerm); err != nil {
		return nil, fmt.Errorf("node: create data dir %s: %w", o.DataDir, err)
	}

	identity, err := resolveIdentity(o)
	if err != nil {
		return nil, err
	}

	lock, err := flock.TryAcquire(filepath.Join(o.DataDir, lockFileName))
	switch {
	case err == nil:
		return host(ctx, o, identity, lock)
	case errors.Is(err, flock.ErrHeld):
		client, aerr := attach(ctx, o, identity)
		if aerr != nil {
			return nil, aerr
		}
		o.Logger.Info(ctx, "node: attached to local node service",
			"data_dir", o.DataDir, "instance", client.InstanceID(), "node_id", client.NodeID())
		return &Handle{
			dataDir:  o.DataDir,
			identity: identity,
			logger:   o.Logger,
			client:   client,
		}, nil
	default:
		return nil, fmt.Errorf("node: acquire %s: %w", o.DataDir, err)
	}
}

// resolveIdentity settles which identity this data dir serves under.
//
// An injected identity (tests, and later an installer that pre-provisions peers) is
// persisted when the dir has none, so a LATER attach — which reads the file and
// never mints — resolves to the same one. An injected identity that contradicts an
// existing file is an error rather than an overwrite: silently replacing a node's
// identity would orphan everything already bound to the old one.
func resolveIdentity(o Options) (*Identity, error) {
	if o.Identity == nil {
		return LoadOrCreateIdentity(o.DataDir)
	}
	if err := o.Identity.validate(); err != nil {
		return nil, err
	}
	onDisk, err := LoadIdentity(o.DataDir)
	switch {
	case err == nil:
		if onDisk.NodeID != o.Identity.NodeID {
			return nil, fmt.Errorf("%w: data dir has %s, injected identity is %s",
				ErrIdentityMismatch, onDisk.NodeID, o.Identity.NodeID)
		}
		return onDisk, nil
	case errors.Is(err, os.ErrNotExist):
		if err := o.Identity.save(o.DataDir); err != nil {
			return nil, err
		}
		return o.Identity, nil
	default:
		return nil, err
	}
}

// host binds the local interface for this process. The lock is already held; every
// failure path releases it, so a failed start never leaves this process believing
// it still owns a node it did not start.
func host(ctx context.Context, o Options, identity *Identity, lock *flock.Lock) (*Handle, error) {
	ln, err := net.Listen("tcp", o.LocalListen)
	if err != nil {
		_ = lock.Release()
		return nil, fmt.Errorf("node: listen %s: %w", o.LocalListen, err)
	}

	// The store loads (and resolves what the previous process left running) before the
	// surface that accepts work is up.
	store, err := openTaskStore(filepath.Join(o.DataDir, tasksDirName))
	if err != nil {
		_ = ln.Close()
		_ = lock.Release()
		return nil, err
	}

	// The remote surface binds before anything is published, so a failure here leaves
	// no half-started node behind: nothing on disk mentions a service that is not up.
	remote, err := startA2ASurface(o, store)
	if err != nil {
		_ = ln.Close()
		_ = lock.Release()
		return nil, err
	}

	info := &localInfo{
		InstanceID: newRandomHex(16),
		NodeID:     identity.NodeID,
		Addr:       ln.Addr().String(),
		PID:        os.Getpid(),
		APIVersion: APIVersion,
		Token:      newRandomHex(32),
		StartedAt:  time.Now().UTC(),
	}
	if err := publishLocalInfo(o.DataDir, info); err != nil {
		_ = remote.close()
		_ = ln.Close()
		_ = lock.Release()
		return nil, err
	}

	srv := &http.Server{
		Handler:           localHandler(info),
		ReadHeaderTimeout: localHTTPTimeout,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			o.Logger.Error(context.Background(), "node: local interface stopped", err, "addr", info.Addr)
		}
	}()

	o.Logger.Info(ctx, "node: hosting local node service",
		"data_dir", o.DataDir, "addr", info.Addr, "instance", info.InstanceID, "node_id", identity.NodeID)

	return &Handle{
		dataDir:  o.DataDir,
		identity: identity,
		logger:   o.Logger,
		hosting:  true,
		lock:     lock,
		store:    store,
		ln:       ln,
		srv:      srv,
		remote:   remote,
		info:     info,
		client:   newClient(o.DataDir, info),
	}, nil
}

// attach waits, within the caller's budget, for the hosting process to become
// answerable and returns a client bound to that instance.
//
// The wait is what makes the common case (this process started a moment after the
// host did) work: the host holds the lock before it has published anything, so the
// first read legitimately finds no info yet. The budget is what keeps a wedged host
// from turning into a wedged start.
func attach(ctx context.Context, o Options, identity *Identity) (*Client, error) {
	deadline := time.Now().Add(o.AttachTimeout)
	var last error

	for {
		client, err := attachOnce(ctx, o, identity)
		if err == nil {
			return client, nil
		}
		if !waitForRetry(err) {
			return nil, err
		}
		last = err

		if !time.Now().Before(deadline) {
			return nil, attachTimedOut(o.DataDir, last, o.AttachTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(attachRetryInterval):
		}
	}
}

// attachOnce is one attempt: read what is published, then make the published
// service prove it is that same instance.
func attachOnce(ctx context.Context, o Options, identity *Identity) (*Client, error) {
	info, err := readLocalInfo(o.DataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: nothing published in %s yet", ErrUnavailable, o.DataDir)
		}
		// Atomic publication means a reader never sees a half-written file, so an
		// unreadable one is genuinely broken. Say so instead of waiting out the
		// budget and reporting a vague timeout.
		return nil, fmt.Errorf("node: local info in %s is unreadable: %w", o.DataDir, err)
	}
	if info.NodeID != identity.NodeID {
		return nil, fmt.Errorf("%w: published %s, this data dir's identity is %s",
			ErrIdentityMismatch, info.NodeID, identity.NodeID)
	}

	client := newClient(o.DataDir, info)
	hs, err := client.Handshake(ctx)
	if err != nil {
		return nil, err
	}
	if hs.NodeID != identity.NodeID {
		return nil, fmt.Errorf("%w: answering node %s, this data dir's identity %s",
			ErrIdentityMismatch, hs.NodeID, identity.NodeID)
	}
	// The file and the process that answered must be the same instance: a port can
	// be reused between the two calls, and reading the old file's fields off the
	// new process is exactly the mistake the instance id exists to prevent.
	if hs.InstanceID != info.InstanceID {
		return nil, fmt.Errorf("%w: %s is published, %s answered", ErrInstanceChanged, info.InstanceID, hs.InstanceID)
	}
	return client, nil
}

// Hosting reports whether THIS process hosts the local interface.
func (h *Handle) Hosting() bool { return h != nil && h.hosting }

// Client is the one way to talk to the node, hosting or not.
func (h *Handle) Client() *Client { return h.client }

// NodeID is the node's stable identity.
func (h *Handle) NodeID() string {
	if h == nil || h.identity == nil {
		return ""
	}
	return h.identity.NodeID
}

// DataDir is the node's data directory.
func (h *Handle) DataDir() string { return h.dataDir }

// Addr is the local interface's address, empty when this process attached.
func (h *Handle) Addr() string {
	if h == nil || h.info == nil {
		return ""
	}
	return h.info.Addr
}

// RemoteAddr is the A2A interface's address — what another node would call — or
// empty when this process attached (only the host serves it).
func (h *Handle) RemoteAddr() string {
	if h == nil {
		return ""
	}
	return h.remote.Addr()
}

// Card is the agent card this node publishes about itself, nil when this process
// attached: it belongs to the hosting process's listener.
func (h *Handle) Card() *a2a.AgentCard {
	if h == nil {
		return nil
	}
	return h.remote.Card()
}

// Close stops hosting. An attached handle owns nothing and does nothing.
//
// local.json is left in place on purpose: removing it would open a window where a
// starting peer reads "no info" and reports a vague timeout, and it would not
// protect anything — the instance id is already what makes a stale file harmless.
func (h *Handle) Close() error {
	if h == nil || !h.hosting {
		return nil
	}
	h.hosting = false

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Both listeners go down before the lock is released: a caller that finds the
	// lock free may start hosting, and it must not have to race a service that is
	// still answering on the old instance.
	serr := h.srv.Shutdown(ctx)
	rerr := h.remote.close()
	lerr := h.lock.Release()
	h.logger.Info(ctx, "node: stopped hosting", "data_dir", h.dataDir, "instance", h.info.InstanceID)

	for _, err := range []error{serr, rerr} {
		if err != nil {
			return fmt.Errorf("node: shut down listener: %w", err)
		}
	}
	return lerr
}

package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/flock"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/stretchr/testify/require"
)

func testOptions(dir string) Options {
	return Options{
		DataDir:       dir,
		LocalListen:   "127.0.0.1:0",
		AttachTimeout: 3 * time.Second,
		Logger:        logger.New("node-test"),
	}
}

// start brings up a handle for dir and closes it when the test ends. Close is
// idempotent, so a test that closes it earlier is unaffected.
func start(t *testing.T, dir string) *Handle {
	t.Helper()
	h, err := StartOrAttach(context.Background(), testOptions(dir))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}

func attachTo(t *testing.T, dir string) *Handle {
	t.Helper()
	h, err := StartOrAttach(context.Background(), testOptions(dir))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	require.False(t, h.Hosting())
	return h
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

func TestFirstStartHostsAndPublishesLocalInfo(t *testing.T) {
	// A nested data dir on purpose: the assertion below is about the permissions
	// the NODE chooses when it creates its directory, not about whatever mode the
	// testing framework handed us.
	dir := filepath.Join(t.TempDir(), "node")
	h := start(t, dir)

	require.True(t, h.Hosting())
	require.NotEmpty(t, h.Addr())

	info, err := readLocalInfo(dir)
	require.NoError(t, err)
	require.Equal(t, h.Addr(), info.Addr)
	require.Equal(t, h.NodeID(), info.NodeID)
	require.Equal(t, APIVersion, info.APIVersion)
	require.Equal(t, os.Getpid(), info.PID)
	require.NotEmpty(t, info.InstanceID)
	require.NotEmpty(t, info.Token)

	// A hosting process talks to the node the same way anybody else does.
	require.Equal(t, info.InstanceID, h.Client().InstanceID())
	require.NoError(t, h.Client().Ping(context.Background()))

	require.Equal(t, os.FileMode(0o700), permOf(t, dir))
	require.Equal(t, os.FileMode(0o600), permOf(t, filepath.Join(dir, localFileName)))
	require.Equal(t, os.FileMode(0o600), permOf(t, filepath.Join(dir, identityFileName)))
}

func TestSecondHandleAttachesToTheSameInstance(t *testing.T) {
	dir := t.TempDir()
	host := start(t, dir)
	attached := attachTo(t, dir)

	require.Equal(t, host.Client().InstanceID(), attached.Client().InstanceID())
	require.Equal(t, host.NodeID(), attached.NodeID())
	require.Empty(t, attached.Addr(), "only the hosting process has a listener")
	require.NoError(t, attached.Client().Ping(context.Background()))

	// Closing an attached handle owns nothing and must not disturb the host.
	require.NoError(t, attached.Close())
	require.NoError(t, host.Client().Ping(context.Background()))
}

func TestIdentityIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	first := start(t, dir)
	nodeID := first.NodeID()
	firstInstance := first.Client().InstanceID()
	require.NoError(t, first.Close())

	second := start(t, dir)
	require.Equal(t, nodeID, second.NodeID(), "the identity outlives the process")
	require.NotEqual(t, firstInstance, second.Client().InstanceID(),
		"a restarted host is a NEW instance, and the instance id is what says so")
	require.Equal(t, os.FileMode(0o600), permOf(t, filepath.Join(dir, identityFileName)))
}

func TestInjectedIdentityIsPersistedForLaterAttachers(t *testing.T) {
	dir := t.TempDir()
	injected, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	opts := testOptions(dir)
	opts.Identity = injected
	host, err := StartOrAttach(context.Background(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, host.Close()) })
	require.True(t, host.Hosting())
	require.Equal(t, injected.NodeID, host.NodeID())

	// The attach path only ever READS the identity file, so an injected identity
	// has to land there or the next process would try to attach as a stranger.
	attached := attachTo(t, dir)
	require.Equal(t, injected.NodeID, attached.Client().NodeID())
}

func TestInjectedIdentityOverAnExistingOneIsRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	other, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	opts := testOptions(dir)
	opts.Identity = other
	_, err = StartOrAttach(context.Background(), opts)
	require.ErrorIs(t, err, ErrIdentityMismatch)
}

func TestAttachedClientReportsAnInstanceChange(t *testing.T) {
	dir := t.TempDir()

	first := start(t, dir)
	stale := attachTo(t, dir)
	require.NoError(t, stale.Client().Ping(context.Background()))

	require.NoError(t, first.Close())
	second := start(t, dir)
	require.True(t, second.Hosting())

	// Whether the new host reused the old port (refused credential) or took
	// another one (dead address), the honest answer is the same: the instance
	// this handle attached to is gone.
	err := stale.Client().Ping(context.Background())
	require.ErrorIs(t, err, ErrInstanceChanged)
}

func TestAHeldLockWithoutAServiceFailsWithoutTakingOver(t *testing.T) {
	dir := t.TempDir()
	identity, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)

	// What a crashed-then-replaced host leaves behind: info pointing at a port
	// nobody serves, under a lock that IS still held.
	require.NoError(t, fileutil.AtomicWriteJSONPrivate(filepath.Join(dir, localFileName), &localInfo{
		InstanceID: "deadbeef", NodeID: identity.NodeID, Addr: "127.0.0.1:1",
		PID: 1, APIVersion: APIVersion, Token: "stale", StartedAt: time.Now().UTC(),
	}))
	before, err := os.ReadFile(filepath.Join(dir, localFileName))
	require.NoError(t, err)

	held, err := flock.TryAcquire(filepath.Join(dir, lockFileName))
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Release()) }()

	opts := testOptions(dir)
	opts.AttachTimeout = 300 * time.Millisecond
	_, err = StartOrAttach(context.Background(), opts)
	require.ErrorIs(t, err, ErrUnavailable)

	after, err := os.ReadFile(filepath.Join(dir, localFileName))
	require.NoError(t, err)
	require.Equal(t, before, after, "a failed start must not touch what the live holder published")
}

func TestMalformedLocalInfoIsReportedInsteadOfWaitedOut(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, dataDirPerm))
	require.NoError(t, os.WriteFile(filepath.Join(dir, localFileName), []byte("{not json"), 0o600))

	held, err := flock.TryAcquire(filepath.Join(dir, lockFileName))
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Release()) }()

	opts := testOptions(dir)
	opts.AttachTimeout = 5 * time.Second // long on purpose: it must not wait it out
	started := time.Now()
	_, err = StartOrAttach(context.Background(), opts)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnavailable)
	require.Contains(t, err.Error(), "unreadable")
	require.Less(t, time.Since(started), time.Second)
}

func TestAFreeLockOverwritesStaleLocalInfo(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(dir, dataDirPerm))
	require.NoError(t, fileutil.AtomicWriteJSONPrivate(filepath.Join(dir, localFileName), &localInfo{
		InstanceID: "stale", NodeID: "someone-else", Addr: "127.0.0.1:1",
		PID: 1, APIVersion: APIVersion, Token: "stale", StartedAt: time.Now().UTC(),
	}))

	h := start(t, dir)
	require.True(t, h.Hosting())

	info, err := readLocalInfo(dir)
	require.NoError(t, err)
	require.NotEqual(t, "stale", info.InstanceID)
	require.Equal(t, h.NodeID(), info.NodeID)
}

func TestTwoDataDirsAreIndependent(t *testing.T) {
	a := start(t, t.TempDir())
	b := start(t, t.TempDir())

	require.True(t, a.Hosting())
	require.True(t, b.Hosting())
	require.NotEqual(t, a.NodeID(), b.NodeID())
	require.NotEqual(t, a.Addr(), b.Addr())
	require.NotEqual(t, a.Client().InstanceID(), b.Client().InstanceID())
	require.NoError(t, a.Client().Ping(context.Background()))
	require.NoError(t, b.Client().Ping(context.Background()))
}

func TestConcurrentStartsElectExactlyOneHost(t *testing.T) {
	dir := t.TempDir()
	// Seed the identity first so the race is about the LOCK, not about two
	// callers minting a key at the same time.
	if _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	const n = 5
	var (
		wg      sync.WaitGroup
		handles [n]*Handle
		errs    [n]error
		gate    = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			handles[i], errs[i] = StartOrAttach(context.Background(), testOptions(dir))
		}(i)
	}
	close(gate)
	wg.Wait()

	hosts := 0
	instance := ""
	for i := 0; i < n; i++ {
		h := handles[i]
		require.NoError(t, errs[i])
		require.NotNil(t, h)
		t.Cleanup(func() { require.NoError(t, h.Close()) })

		if h.Hosting() {
			hosts++
		}
		if instance == "" {
			instance = h.Client().InstanceID()
		}
		require.Equal(t, instance, h.Client().InstanceID())
	}
	require.Equal(t, 1, hosts, "the lock elects exactly one hosting process")
}

func TestHandshakeRequiresThePublishedCredential(t *testing.T) {
	dir := t.TempDir()
	h := start(t, dir)
	info, err := readLocalInfo(dir)
	require.NoError(t, err)

	url := "http://" + info.Addr + localRouteHandshake
	get := func(authorization string) *http.Response {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		require.NoError(t, rerr)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, derr := http.DefaultClient.Do(req)
		require.NoError(t, derr)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	require.Equal(t, http.StatusUnauthorized, get("").StatusCode)
	require.Equal(t, http.StatusUnauthorized, get("Bearer not-the-token").StatusCode)

	resp := get("Bearer " + info.Token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var hs Handshake
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&hs))
	require.Equal(t, info.InstanceID, hs.InstanceID)
	require.Equal(t, h.NodeID(), hs.NodeID)
	require.Equal(t, APIVersion, hs.APIVersion)
}

func TestHandshakeRejectsAnIncompatibleAPIVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Handshake{
			InstanceID: "other", NodeID: "other", APIVersion: APIVersion + 1,
		})
	}))
	defer srv.Close()

	client := newClient("", &localInfo{
		InstanceID: "other", NodeID: "other",
		Addr:  strings.TrimPrefix(srv.URL, "http://"),
		Token: "t", APIVersion: APIVersion,
	})
	_, err := client.Handshake(context.Background())
	require.ErrorIs(t, err, ErrVersionMismatch)
}

func TestLocalListenMustBeLoopback(t *testing.T) {
	_, err := StartOrAttach(context.Background(), Options{
		DataDir:     t.TempDir(),
		LocalListen: "0.0.0.0:0",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "loopback")
}

func TestDataDirIsRequired(t *testing.T) {
	_, err := StartOrAttach(context.Background(), Options{})
	require.ErrorContains(t, err, "DataDir")
}

// writeIdentityFile replaces the identity file with a tampered copy, the way a
// hand-edited, copied or half-restored one would look.
func writeIdentityFile(t *testing.T, dir string, mutate func(*Identity)) {
	t.Helper()
	id, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	mutate(id)
	data, err := json.Marshal(id)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, identityFileName), data, 0o600))
}

func TestIdentityValidationRejectsAMismatchedNodeID(t *testing.T) {
	dir := t.TempDir()
	writeIdentityFile(t, dir, func(id *Identity) { id.NodeID = "tampered" })

	_, err := LoadIdentity(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match its public key")
}

func TestIdentityValidationRejectsAPublicKeyFromAnotherPair(t *testing.T) {
	dir := t.TempDir()
	elsewhere, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	// The seed and the public key describe DIFFERENT key pairs: each field is
	// plausible on its own, and the node id still matches the stated public key,
	// so only a check that walks the whole chain catches it.
	writeIdentityFile(t, dir, func(id *Identity) { id.PrivateKey = elsewhere.PrivateKey })

	_, err = LoadIdentity(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not the one its private_key produces")
}

func TestIdentityValidationRejectsAWrongLengthSeed(t *testing.T) {
	dir := t.TempDir()
	writeIdentityFile(t, dir, func(id *Identity) { id.PrivateKey = "AAAA" })

	_, err := LoadIdentity(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "seed is")
}

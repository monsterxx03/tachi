package node

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Close is what an entry point calls on its way out, so the one thing it must never do is wait for
// something that will not happen. A store that was never started has no loop to wait for, and one
// that was started stops it — both are the promise that keeps a process able to quit, which is what
// breaking it cost once (node/discovery.go browseLoop).
func TestClosingAStoreReturnsWhetherOrNotItStarted(t *testing.T) {
	never := newSightingsStore(nil)
	require.NotPanics(t, never.Close)
	require.NotPanics(t, never.Close, "and calling it twice does not wait on something already waited for")

	started := newSightingsStore(nil)
	started.Start(t.Context())
	// A candidate that is already due, so the loop has something to do while it is being stopped: the
	// verifier answers nothing, which is the ordinary "not a Tachi node" result.
	started.setVerifier(func(context.Context, []string) (string, error) { return "", nil })
	started.feed(candidate{Identity: discoveredID, Addrs: []string{"https://192.168.71.168:9990"}})

	closed := make(chan struct{})
	go func() {
		started.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the verification loop does not leave on its cancel")
	}

	require.Empty(t, started.sightings(), "and what it held is dropped with it")
	require.False(t, started.verifyDue(context.Background()), "nothing is queued after a close")
}

package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/stretchr/testify/require"
)

// A disabled node is not an error and not a service: it is nothing at all. These
// tests exist because three entry points depend on that — "off by default" has to
// mean "the process behaves exactly as it did before the node existed", and that is
// a thing to assert, not a thing to promise.
func TestStartOrAttachNodeDisabledCreatesNothing(t *testing.T) {
	base := t.TempDir()
	config.SetBaseDir(base)
	t.Cleanup(func() { config.SetBaseDir("") })

	handle, err := StartOrAttachNode(context.Background(), &config.Config{})
	require.NoError(t, err)
	require.Nil(t, handle)

	_, statErr := os.Stat(config.A2ADir())
	require.True(t, os.IsNotExist(statErr), "disabled must not even create the data directory")
}

func TestStartOrAttachNodeEnabledHostsUnderTheConfigDir(t *testing.T) {
	base := t.TempDir()
	config.SetBaseDir(base)
	t.Cleanup(func() { config.SetBaseDir("") })

	cfg := &config.Config{}
	cfg.Node.Enabled = true
	// An enabled node needs a workspace, and a test must never let the default (~/workspace)
	// do the work: the fail-closed refusal for an empty one is asserted separately.
	cfg.Node.Workspace = filepath.Join(base, "workspace")

	handle, err := StartOrAttachNode(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, handle)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })

	require.True(t, handle.Hosting())
	// The data dir is DERIVED from the base dir, never configured: one directory is
	// one node identity, so a different --home is a different node with no switch to
	// set (and the integration harness gets its isolation for free).
	require.Equal(t, filepath.Join(base, "a2a"), handle.DataDir())
	require.Equal(t, config.A2ADir(), handle.DataDir())

	// What it published is where the next process will look for it.
	info, err := readLocalInfo(config.A2ADir())
	require.NoError(t, err)
	require.Equal(t, handle.Client().InstanceID(), info.InstanceID)
	require.Equal(t, handle.NodeID(), info.NodeID)
}

func TestStartOrAttachNodeWithoutConfigIsDisabled(t *testing.T) {
	handle, err := StartOrAttachNode(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, handle)
}

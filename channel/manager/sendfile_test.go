package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The channel's own attachment limit, enforced where the receiver knows it.
//
// The tool itself deliberately has no size check (agent/tools/sendfile.go): an IM platform's limit,
// a node's file policy and "the desktop sends nothing at all" are three different numbers, and the
// tighter one imposed on all three would silently decide what the other two may send.
func TestTheChannelRefusesAFileItsPlatformWillNotTake(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "report.txt")
	require.NoError(t, os.WriteFile(small, []byte("hello"), 0o600))

	// A SPARSE file: the size is what the check reads, and writing 50 MiB of bytes to prove it
	// would cost the test machine a disk for no extra confidence.
	huge := filepath.Join(dir, "huge.bin")
	f, err := os.Create(huge)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(channelMaxAttachmentBytes+1))
	require.NoError(t, f.Close())

	tool, sink := newSendFileTool()
	ctx := context.Background()

	out, err := tool.ExecuteContext(ctx, `{"path":`+quote(small)+`}`)
	require.NoError(t, err)
	require.Contains(t, out, "已交给用户")
	require.Len(t, sink.snapshot(), 1, "a file the platform takes has to reach the queue")
	require.Equal(t, small, sink.snapshot()[0].LocalPath)

	_, err = tool.ExecuteContext(ctx, `{"path":`+quote(huge)+`}`)
	require.Error(t, err, "a file the platform will not take must not be queued")
	require.Contains(t, err.Error(), "50.0 MB")
	require.Len(t, sink.snapshot(), 1, "and the refusal leaves the queue as it was")
}

// quote renders a path as a JSON string, so a temp directory with a quote in its name cannot make
// this test read as a tool-argument problem.
func quote(path string) string {
	b, err := json.Marshal(path)
	if err != nil {
		panic(err)
	}
	return string(b)
}

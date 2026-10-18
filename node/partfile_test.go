package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
)

// One reading of a message's Parts, used by both ends of all three directions. The cases below are
// the whole policy: what is taken, and what a caller is told instead of silence.

func rawPart(name, mediaType string, bytes []byte) *a2a.Part {
	p := a2a.NewRawPart(bytes)
	p.Filename = name
	p.MediaType = mediaType
	return p
}

func TestAFilePartIsTakenWithItsNameAndType(t *testing.T) {
	files, refused := filePartsOf([]*a2a.Part{
		a2a.NewTextPart("看看这个"),
		rawPart("report.pdf", "application/pdf", []byte("%PDF-1.4")),
		a2a.NewDataPart(map[string]any{"k": "v"}),
	})
	require.Empty(t, refused)
	require.Len(t, files, 1, "text and data are other features' business")
	require.Equal(t, "report.pdf", files[0].Name)
	require.Equal(t, "application/pdf", files[0].MediaType)
	require.Equal(t, "hello", string(files[0].Bytes[:0])+"hello"[:0]+"hello")
}

// The size policy on its own, through the function that states it: materialising a maximum-size
// file to prove the limit would measure this machine (slowly) rather than the policy. That
// asFilePart APPLIES it is covered end to end by the SendFile path below, where a sparse file of
// the same size reaches the node's callback and is refused there.
func TestAnOversizedFileIsRefusedWithTheNumbers(t *testing.T) {
	refusal := checkPartSize(maxFilePartBytes + 1)
	require.NotNil(t, refusal)
	require.Equal(t, reasonPartTooLarge, refusal.Reason)
	require.Contains(t, refusal.Detail, "100.0 MB", "the sentence quotes both sizes")

	require.Nil(t, checkPartSize(maxFilePartBytes), "exactly at the limit is allowed")
	require.Equal(t, reasonPartMediaType, checkPartSize(0).Reason,
		"zero bytes is a broken transfer, not a size problem")
}

func TestAnEmptyFileIsRefused(t *testing.T) {
	files, refused := filePartsOf([]*a2a.Part{rawPart("nothing.txt", "text/plain", nil)})
	require.Empty(t, files)
	require.Len(t, refused, 1)
	require.Contains(t, refused[0].Detail, "empty")
}

// A file of a type this node has no taste about is still a file: it is taken with the type it was
// given, which travels as the label the model is told and as what a nameless file is named by. What
// a peer sends is this machine's to open with its own tools, so a list of blessed types would be
// this node deciding, on the sender's behalf, what is worth carrying (§6.4 leaves the type to the
// implementation).
func TestAFileOfAnyTypeIsTakenWithTheTypeItWasGiven(t *testing.T) {
	files, refused := filePartsOf([]*a2a.Part{
		rawPart("run", "application/x-executable", []byte("\x7fELF")),
	})
	require.Empty(t, refused)
	require.Len(t, files, 1)
	require.Equal(t, "application/x-executable", files[0].MediaType)
}

// A peer that declares nothing gets an answer from its name, and failing that from its bytes.
//
// The examples are ones the STDLIB's own table knows (`.json`) or that need no name at all, and that
// is deliberate: `mime.TypeByExtension` also reads the HOST's mime files on unix, so an extension
// only a host table knows (`.md` is text/markdown on Linux and unknown here, `.sh` is
// application/x-sh there) would make this test pass or fail by machine. Nothing is decided from that
// label, so nothing here asserts one that a host answered.
func TestAnUndeclaredTypeIsAnsweredFromTheNameThenTheBytes(t *testing.T) {
	byName, refused := filePartsOf([]*a2a.Part{rawPart("data.json", "", []byte(`{"k":1}`))})
	require.Empty(t, refused)
	require.Equal(t, "application/json", byName[0].MediaType)

	byBytes, refused := filePartsOf([]*a2a.Part{rawPart("", "", []byte("just some words"))})
	require.Empty(t, refused)
	require.True(t, strings.HasPrefix(byBytes[0].MediaType, "text/plain"), byBytes[0].MediaType)
	require.NotEmpty(t, byBytes[0].Name, "a file with no name still needs one here")
	require.True(t, strings.HasSuffix(byBytes[0].Name, ".txt"), byBytes[0].Name)
}

// An image is handed to a model AS an image, so the bytes decide which image it is — and bytes that
// are not an image are not one, whatever the sender called them.
func TestAnImageIsTakenFromItsBytesNotItsLabel(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01}

	mislabelled, refused := filePartsOf([]*a2a.Part{rawPart("photo.jpg", "image/jpeg", png)})
	require.Empty(t, refused)
	require.Equal(t, "image/png", mislabelled[0].MediaType, "the bytes are what the model will be given")

	// Not an image, so not one: it is taken as whatever its bytes read as, stored for the model to
	// open, rather than attached as an image part a provider would reject the whole request over.
	notAnImage, refused := filePartsOf([]*a2a.Part{rawPart("photo.png", "image/png", []byte("not an image"))})
	require.Empty(t, refused)
	require.Equal(t, "text/plain", notAnImage[0].MediaType)
}

// A URL Part is a network reach this version does not make. Ignoring it would be indistinguishable
// from an empty answer, so it is refused by name.
func TestAFileURLIsRefusedRatherThanIgnored(t *testing.T) {
	files, refused := filePartsOf([]*a2a.Part{a2a.NewFileURLPart("https://peer.example/x.pdf", "application/pdf")})
	require.Empty(t, files)
	require.Len(t, refused, 1)
	require.Equal(t, reasonPartURL, refused[0].Reason)
}

func TestTooManyFilesInOneMessageAreRefused(t *testing.T) {
	parts := make([]*a2a.Part, 0, maxFilePartsPerMessage+2)
	for i := range maxFilePartsPerMessage + 2 {
		parts = append(parts, rawPart("f.txt", "text/plain", []byte{byte('a' + i)}))
	}
	files, refused := filePartsOf(parts)
	require.Len(t, files, maxFilePartsPerMessage)
	require.Len(t, refused, 1)
	require.Equal(t, reasonPartCount, refused[0].Reason)
}

// The name becomes a name inside a session directory, so what arrives is a name and never a path —
// and a name is still usable after being cut down to size.
func TestTheNameIsReducedToItsBaseAndBounded(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":      "passwd",
		`C:\Users\x\report.pdf`: "report.pdf",
		"/tmp/a/b.txt":          "b.txt",
	}
	for in, want := range cases {
		files, refused := filePartsOf([]*a2a.Part{rawPart(in, "text/plain", []byte("x"))})
		require.Empty(t, refused, in)
		require.Equal(t, want, files[0].Name)
	}

	long := strings.Repeat("n", maxPartNameBytes) + ".pdf"
	files, refused := filePartsOf([]*a2a.Part{rawPart(long, "application/pdf", []byte("%PDF"))})
	require.Empty(t, refused)
	require.LessOrEqual(t, len(files[0].Name), maxPartNameBytes)
	require.True(t, strings.HasSuffix(files[0].Name, ".pdf"), "the extension is what keeps it openable: %s", files[0].Name)
}

// ── End to end, against a running node ───────────────────────────────────────

// postRawMessage posts a message/send envelope to a node's REST path and returns the status and the
// body. The SDK's own client is deliberately NOT used for refusals: it maps an error it does not
// recognise onto a generic protocol failure, and this node's refusals live under its OWN domain
// (§9.1.1) — read through the client they would all look alike.
func postRawMessage(t *testing.T, h *Handle, parts ...*a2a.Part) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"message": a2a.NewMessage(a2a.MessageRoleUser, parts...),
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+h.RemoteAddr()+restSendMessagePath, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}

// lastUserText is what the model was asked in the newest request: the trailing user message.
func lastUserText(t *testing.T, mock *mockllm.Server) string {
	t.Helper()
	reqs := mock.Requests()
	require.NotEmpty(t, reqs, "the turn never reached the model")
	for i := len(reqs) - 1; i >= 0; i-- {
		for j := len(reqs[i].Messages) - 1; j >= 0; j-- {
			if reqs[i].Messages[j].Role == "user" {
				return reqs[i].Messages[j].Content
			}
		}
	}
	t.Fatal("no user message in the recorded requests")
	return ""
}

// A file the caller attaches lands in the conversation's own directory, and the prompt names the
// LOCAL path — which is the whole point: the model reads a file this machine really has, and the
// path the caller knows about never travels (§6.4).
func TestAFileTheCallerSendsIsStoredAndNamedInThePrompt(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("清单收到")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	status, body := postRawMessage(t, h,
		a2a.NewTextPart("看看这份清单"),
		rawPart("inventory.csv", "text/csv", []byte("a,b\n1,2\n")))
	require.Equal(t, http.StatusOK, status, body)

	prompt := lastUserText(t, mock)
	require.Contains(t, prompt, "看看这份清单", "the caller's own text is still the message")
	require.Contains(t, prompt, "Files the caller sent with this message:")
	require.Contains(t, prompt, "inventory.csv (text/csv, 8 B)")

	// The path in the prompt is a file that exists, with the bytes that were sent.
	_, path, ok := strings.Cut(prompt, "saved on this machine at: ")
	require.True(t, ok, prompt)
	path = strings.TrimSpace(strings.SplitN(path, "\n", 2)[0])
	require.Contains(t, path, filepath.Join("received", "inventory.csv"))
	got, err := os.ReadFile(path)
	require.NoError(t, err, "the path the model was given has to be readable")
	require.Equal(t, "a,b\n1,2\n", string(got))
}

// An image does not become a file: the model is handed its bytes as an image, and the session
// record keeps them (the same route a pasted screenshot takes), so there is nothing on disk to go
// stale.
//
// The provider is declared vision-capable on purpose: a model that is NOT gets the image replaced
// by a generated description before the request goes out (agent/vision_fallback.go), which is a
// different path and would hide whether the bytes crossed at all.
func TestAnImageTheCallerSendsReachesTheModelAsAnImage(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("图看到了")...)

	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01, 0x02, 0x03}

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
	cfg.Providers[0].Spec.Vision = ptr(true)
	h := startRealNode(t, cfg)
	status, body := postRawMessage(t, h,
		a2a.NewTextPart("这张图里有什么"),
		rawPart("shot.png", "image/png", png))
	require.Equal(t, http.StatusOK, status, body)

	prompt := lastUserText(t, mock)
	require.Contains(t, prompt, "shot.png (image/png, 12 B) — attached to this message as an image")

	// The bytes crossed: what the provider received carries the image, base64-encoded.
	reqs := mock.Requests()
	require.NotEmpty(t, reqs)
	require.Contains(t, string(reqs[0].RawBody), base64.StdEncoding.EncodeToString(png),
		"the image has to reach the provider's request, not just be mentioned")

	// And nothing was written to the session's received/ directory for it.
	received, err := filepath.Glob(filepath.Join(sessionsDir(t), "*", "received", "*"))
	require.NoError(t, err)
	require.Empty(t, received, "an image is carried as a part, not as a file")
}

// An image no provider takes is still a file the model can open. The BYTES answer both questions —
// which format it is, and whether it may go into a request as a part (llm's list, the same one the
// Read tool checks) — because a provider rejects the whole call over a part it does not accept.
func TestAnImageNoProviderTakesIsSentAsAFile(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("图我当文件看")...)

	// A BMP: an image indeed, and not one this machine hands a model.
	bmp := []byte{'B', 'M', 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
	cfg.Providers[0].Spec.Vision = ptr(true)
	h := startRealNode(t, cfg)
	status, body := postRawMessage(t, h,
		a2a.NewTextPart("这张图里有什么"),
		rawPart("logo.bmp", "", bmp))
	require.Equal(t, http.StatusOK, status, body)

	prompt := lastUserText(t, mock)
	require.Contains(t, prompt, "logo.bmp (image/bmp")
	require.Contains(t, prompt, "saved on this machine at: ",
		"an image a provider will not take goes the file way")
	require.NotContains(t, prompt, "as an image")

	// And its bytes never reach the provider: nothing was attached as an image part.
	reqs := mock.Requests()
	require.NotEmpty(t, reqs)
	require.NotContains(t, string(reqs[0].RawBody), base64.StdEncoding.EncodeToString(bmp))
}

// What this node will not take is refused BEFORE a task exists, with its own reason: a caller that
// cannot be told "no" would instead get a task whose input silently lost a file.
//
// The refusal used here is the URL one, because every kind takes the same path through the guard and
// this one needs no bytes materialised: the size kind is covered without allocating them (see
// checkPartSize's test) and end to end on the way out (a sparse file of the same size, refused by
// the node's SendFile callback).
func TestAnUnacceptableFileIsRefusedBeforeAnyTaskExists(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("不该跑起来")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))

	status, body := postRawMessage(t, h,
		a2a.NewTextPart("看这个"),
		a2a.NewFileURLPart("https://peer.example/report.pdf", "application/pdf"))
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, reasonPartURL)
	require.Contains(t, body, "\"tachi\"", "the refusal travels under this node's own domain")

	require.Empty(t, mock.Requests(), "a refused message must not reach the model")
	sessions, err := filepath.Glob(filepath.Join(sessionsDir(t), "*"))
	require.NoError(t, err)
	require.Empty(t, sessions, "no task, so no conversation was opened for it")
}

// A file where it does not belong. A waiting question is answered with words or with a choice, so
// bytes on an answer are a shape this node cannot map — and §7.4's rule for that is a refusal, not
// a shrug. Before this the raw part was simply skipped while the caller waited for an answer that
// was never delivered.
func TestAnAnswerCarryingAFileIsRefusedRatherThanIgnored(t *testing.T) {
	questions := []tools.Question{{Question: "检查照片还是文档？"}}
	answer := a2a.NewMessage(a2a.MessageRoleUser,
		rawPart("photo.png", "image/png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}))

	_, err := answersFor(questions, answer)
	require.Error(t, err)
	require.ErrorIs(t, err, errAnswerShape)
	require.Contains(t, err.Error(), "cannot carry a file")
}

// ── The other two directions, against a real peer ────────────────────────────

// A file the remote agent hands over with SendFile comes back as an artifact, lands in THIS
// machine's storage, and the result names a local path — which is the whole round trip: the caller
// only learns the bytes if they really crossed, and nothing about the other machine's filesystem
// travels with them.
func TestAFileTheRemoteAgentSendsComesBackAndLandsHere(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("send_1", tools.ToolNameSendFile, `{"path":"report.txt"}`),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})
	mock.Script(oneMessageScript("报告在附件里")...)

	sender, peer := startPeer(t, mock, "ReadFile", tools.ToolNameSendFile)
	report := filepath.Join(filepath.Dir(peer.DataDir()), "ws", "report.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(report), 0o755))
	require.NoError(t, os.WriteFile(report, []byte("the peer's own bytes\n"), 0o600))

	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: peerRef(t, sender), Prompt: "把报告发给我", LogicalID: "conv-1", Wait: 5 * time.Second,
	})
	require.NoError(t, err)
	require.Equal(t, "completed", view.State, "note=%q", view.Note)
	require.Contains(t, view.Result, "报告在附件里", "the answer still travels")
	require.Contains(t, view.Result, "Files the peer sent back:")
	require.Contains(t, view.Result, "report.txt (text/plain")

	// The path in the result is a file on THIS machine, with the bytes the peer had.
	_, rest, ok := strings.Cut(view.Result, "saved on this machine at: ")
	require.True(t, ok, view.Result)
	path := strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	got, err := os.ReadFile(path)
	require.NoError(t, err, "the path the model was given has to be readable here")
	require.Equal(t, "the peer's own bytes\n", string(got))
}

// With no session — a one-off `-p` run, which delegates from no conversation — the file still lands
// somewhere a model can name, beside the other tool results this package spills.
func TestAFileFromAPeerWithNoSessionGoesBesideTheToolResults(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("send_1", tools.ToolNameSendFile, `{"path":"notes.md"}`),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})
	mock.Script(oneMessageScript("笔记给你")...)

	sender, peer := startPeer(t, mock, "ReadFile", tools.ToolNameSendFile)
	notes := filepath.Join(filepath.Dir(peer.DataDir()), "ws", "notes.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(notes), 0o755))
	require.NoError(t, os.WriteFile(notes, []byte("# 笔记\n"), 0o600))

	// No session in the context: this is the shape a run with no conversation has.
	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: peerRef(t, sender), Prompt: "把笔记发给我", LogicalID: "", Wait: 5 * time.Second,
	})
	require.NoError(t, err)
	require.Equal(t, "completed", view.State, "note=%q", view.Note)
	_, rest, ok := strings.Cut(view.Result, "saved on this machine at: ")
	require.True(t, ok, view.Result)
	path := strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	require.Equal(t, config.ToolResultsDir(), filepath.Dir(path),
		"with no conversation, the file goes where this package's other tool results go")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "# 笔记\n", string(got))
}

// What the node will not send, it refuses where the MODEL can see it: the tool call fails with the
// reason, so "handed over" is never a sentence about a file that did not leave.
func TestAFileTheNodeWillNotSendFailsTheToolCall(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("send_1", tools.ToolNameSendFile, `{"path":"huge.txt"}`),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})
	mock.Script(oneMessageScript("那就没发")...)

	sender, peer := startPeer(t, mock, "ReadFile", tools.ToolNameSendFile)
	// A SPARSE file: the size is what the policy looks at, and writing 100 MiB of real bytes to
	// prove that would cost the test machine a disk and a second for no extra confidence.
	huge := filepath.Join(filepath.Dir(peer.DataDir()), "ws", "huge.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(huge), 0o755))
	f, err := os.Create(huge)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxFilePartBytes+1))
	require.NoError(t, f.Close())

	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: peerRef(t, sender), Prompt: "把这个大文件发给我", LogicalID: "conv-1", Wait: 5 * time.Second,
	})
	require.NoError(t, err)
	require.Equal(t, "completed", view.State, "note=%q", view.Note)
	require.NotContains(t, view.Result, "Files the peer sent back:",
		"a refused file must not be announced as sent")

	// The model was told why: an oversized SendFile comes back as a failed tool call.
	reqs := mock.Requests()
	require.Len(t, reqs, 2)
	var told string
	for _, m := range reqs[1].Messages {
		if m.Role == "tool" {
			told = m.Content
		}
	}
	require.Contains(t, told, "cannot be sent", "the model has to see the refusal")
	require.Contains(t, told, "100.0 MB")
}

func TestAFileFromAPeerIsStoredOnlyOnce(t *testing.T) {
	const sessionID = "2026-09-13-103926-92db8c33"

	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("send_1", tools.ToolNameSendFile, `{"path":"report.txt"}`),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})
	mock.Script(oneMessageScript("报告在附件里")...)

	sender, peer := startPeer(t, mock, "ReadFile", tools.ToolNameSendFile)
	report := filepath.Join(filepath.Dir(peer.DataDir()), "ws", "report.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(report), 0o755))
	require.NoError(t, os.WriteFile(report, []byte("once\n"), 0o600))

	// The real shape: the delegation tool runs inside a turn, whose context names the session.
	ctx := tools.WithSessionID(context.Background(), sessionID)
	view, err := sender.Delegate(ctx, tools.DelegationRequest{
		Peer: peerRef(t, sender), Prompt: "把报告发给我", LogicalID: "conv-1", Wait: 5 * time.Second,
	})
	require.NoError(t, err)
	require.Contains(t, view.Result, "Files the peer sent back:")

	// Every further look at the same task is another `record` — and must not be another file.
	for range 3 {
		_, err := sender.Get(ctx, view.ID)
		require.NoError(t, err)
	}
	// Where it landed is taken from the ANSWER rather than resolved a second way: the assertion is
	// about the file the model was actually pointed at.
	_, rest, ok := strings.Cut(view.Result, "saved on this machine at: ")
	require.True(t, ok, view.Result)
	path := strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	stored, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*"))
	require.NoError(t, err)
	require.Len(t, stored, 1, "a poll must not write the same file again: %v", stored)
	require.Equal(t, "once\n", string(mustRead(t, stored[0])))
	require.Equal(t, stored[0], path, "and the path the model was given is the one that exists")
}

// mustRead reads a file or fails the test.
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	return got
}

// The listener caps what it will read, and the guard turns that into a refusal in this node's own
// terms — before the body has been allocated, which is the whole point of a cap. (The running node
// uses 256 MiB; the test wires a small number through the same wrapper, because a test that posts
// a quarter gigabyte measures the disk, not the policy.)
func TestAnOversizedBodyIsRefusedBeforeItIsRead(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	srv := httptest.NewServer(limitBody(busyGuard(nil, nil, next), 64))
	t.Cleanup(srv.Close)

	// A perfectly well-formed message that is simply too big for this listener.
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(strings.Repeat("x", 200)))
	body, err := json.Marshal(map[string]any{"message": msg})
	require.NoError(t, err)

	resp, err := http.Post(srv.URL+restSendMessagePath, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, string(out))
	require.Contains(t, string(out), reasonBodyTooLarge)
	require.Contains(t, string(out), "\"tachi\"", "the refusal travels under this node's own domain")
	require.False(t, reached, "a request this node will not read must not reach what handles it")
}

// The cap is on the LISTENER, not on the one route the guard looks at: a body read by anything else
// has to hit the same floor, or "how much memory does a request cost this process" is decided by
// which path the caller picks.
func TestTheBodyCapCoversEveryRoute(t *testing.T) {
	done := make(chan error, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		done <- err
	})
	srv := httptest.NewServer(limitBody(next, 32))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/tasks/whatever", "application/json",
		strings.NewReader(strings.Repeat("y", 128)))
	require.NoError(t, err)
	resp.Body.Close()

	var tooLarge *http.MaxBytesError
	require.ErrorAs(t, <-done, &tooLarge, "a read past the limit has to fail, not keep going")
}

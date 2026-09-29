package link

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"testing"
	"time"
)

// newVoicePeer 和 link_test.go 的 newPeer 是同一件事：一条真的 TCP 连接当设备那一头。
// 区别只有一个——它把 Server 交出来，因为语音的账是 OnVoice 报出来的，而那个回调必须
// 在 Run 之前挂上。
func newVoicePeer(t *testing.T, src Source, sink chan VoiceUpload) *peer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srvConn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	srv := NewServer(src, srvConn)
	srv.OnVoice = func(u VoiceUpload) { sink <- u }
	go func() {
		defer close(done)
		_ = srv.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		<-done
	})

	return &peer{t: t, conn: conn, r: bufio.NewReader(conn), srv: srv}
}

func waitVoice(t *testing.T, sink chan VoiceUpload) VoiceUpload {
	t.Helper()
	select {
	case u := <-sink:
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("no voice report arrived")
	}
	return VoiceUpload{}
}

// ramp 造一段可辨认的原始字节：内容对不对是转写那一步的事，这里钉的是**字节有没有
// 原样走完这条链**。
func ramp(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i * 7)
	}
	return out
}

func sendChunk(p *peer, id uint64, raw []byte) {
	p.write(map[string]any{
		"t": "voice_chunk", "id": id,
		"data": base64.StdEncoding.EncodeToString(raw),
	})
}

func TestVoiceUploadIsAssembledAndMeasured(t *testing.T) {
	sink := make(chan VoiceUpload, 4)
	p := newVoicePeer(t, newFakeSource(), sink)

	raw := ramp(1024)
	p.write(map[string]any{
		"t": "voice_begin", "id": 7, "codec": "ima-adpcm", "hz": 16000, "bytes": len(raw),
	})
	sendChunk(p, 7, raw[:512])
	sendChunk(p, 7, raw[512:])
	p.write(map[string]any{"t": "voice_end", "id": 7, "parts": 2})

	u := waitVoice(t, sink)
	if u.Err != nil {
		t.Fatalf("upload reported an error: %v", u.Err)
	}
	if !bytes.Equal(u.Payload, raw) {
		t.Errorf("payload came out different from what went in (%d bytes)", len(u.Payload))
	}
	if u.Declared != 1024 || len(u.Payload) != 1024 {
		t.Errorf("declared %d, got %d bytes, want 1024/1024", u.Declared, len(u.Payload))
	}
	if u.GotParts != 2 || u.Parts != 2 {
		t.Errorf("parts = %d of %d received, %d reported", u.GotParts, u.Parts, u.Parts)
	}
	if u.ID != 7 || u.Codec != "ima-adpcm" || u.Hz != 16000 {
		t.Errorf("identity lost: id=%d codec=%q hz=%d", u.ID, u.Codec, u.Hz)
	}
	// 时长就是吞吐算式的分母：它是 begin 到 end 的墙钟时间，必须有个正数。
	if u.Duration <= 0 {
		t.Errorf("duration = %v, want a positive begin→end interval", u.Duration)
	}
}

// 少一片（丢包、或者设备那边一片没发出去）：整段作废，而不是拿半截音频去转写。
// 「一半的话」转出来的文本看不出少了什么，那比没有更坏。
func TestVoiceUploadNoticesMissingChunks(t *testing.T) {
	sink := make(chan VoiceUpload, 4)
	p := newVoicePeer(t, newFakeSource(), sink)

	raw := ramp(1024)
	p.write(map[string]any{"t": "voice_begin", "id": 1, "bytes": len(raw)})
	sendChunk(p, 1, raw[:512])
	// 第二片不发了。
	p.write(map[string]any{"t": "voice_end", "id": 1, "parts": 2})

	u := waitVoice(t, sink)
	if u.Err == nil {
		t.Fatal("a short upload was accepted as complete")
	}
	if len(u.Payload) != 512 {
		t.Errorf("payload = %d bytes, want the 512 that did arrive (it is the evidence)", len(u.Payload))
	}
}

// begin 里的字节数是**上限**，不是承诺：设备是边录边发的，录了多长要到结束才知道
// （见设备侧 badge_voice.h）。所以「收到得比声明的少」本身不是错误——只要片数对得上。
// 用户按下录音、说了 2 秒就松手，就是这种情形。
func TestVoiceUploadAcceptsLessThanDeclared(t *testing.T) {
	sink := make(chan VoiceUpload, 4)
	p := newVoicePeer(t, newFakeSource(), sink)

	raw := ramp(1024)
	p.write(map[string]any{"t": "voice_begin", "id": 3, "bytes": 48000}) // 上限：能录 6 秒
	sendChunk(p, 3, raw[:512])
	sendChunk(p, 3, raw[512:])
	p.write(map[string]any{"t": "voice_end", "id": 3, "parts": 2}) // 实际只录了 2 片

	u := waitVoice(t, sink)
	if u.Err != nil {
		t.Fatalf("a shorter-than-declared recording was rejected: %v", u.Err)
	}
	if u.Declared != 48000 || len(u.Payload) != 1024 {
		t.Errorf("declared %d, payload %d bytes, want 48000/1024", u.Declared, len(u.Payload))
	}
}

// 声明的量不可信就整段不要：这不是「尽量收一点」的场景。上限挡的是内存。
func TestVoiceUploadRejectsAnUnreasonableSize(t *testing.T) {
	sink := make(chan VoiceUpload, 4)
	p := newVoicePeer(t, newFakeSource(), sink)

	p.write(map[string]any{"t": "voice_begin", "id": 1, "bytes": voiceMaxBytes + 1})
	sendChunk(p, 1, ramp(16))
	p.write(map[string]any{"t": "voice_end", "id": 1, "parts": 1})

	u := waitVoice(t, sink)
	if u.Err == nil {
		t.Fatal("an over-limit upload was accepted")
	}
	if len(u.Payload) != 0 {
		t.Errorf("payload = %d bytes, want 0 (nothing should be buffered)", len(u.Payload))
	}
}

// 坏 base64 是设备端的编码 bug（少一个 '=' 就会这样）。它必须在**收到时就**炸出来，
// 而不是把垃圾拼进音频里。
func TestVoiceUploadRejectsBadBase64(t *testing.T) {
	sink := make(chan VoiceUpload, 4)
	p := newVoicePeer(t, newFakeSource(), sink)

	p.write(map[string]any{"t": "voice_begin", "id": 1, "bytes": 32})
	p.write(map[string]any{"t": "voice_chunk", "id": 1, "data": "AAAA!not-base64"})
	p.write(map[string]any{"t": "voice_end", "id": 1, "parts": 1})

	u := waitVoice(t, sink)
	if u.Err == nil {
		t.Fatal("a corrupted chunk was accepted")
	}
}

// 上一段还没收完就又来一段（设备重录、或者它把 id 复用了）：旧的必须以失败报出来，
// 而不是让两段音频在同一个缓冲里串起来。
func TestANewUploadDiscardsTheStaleOne(t *testing.T) {
	sink := make(chan VoiceUpload, 4)
	p := newVoicePeer(t, newFakeSource(), sink)

	p.write(map[string]any{"t": "voice_begin", "id": 1, "bytes": 512})
	sendChunk(p, 1, ramp(256))
	p.write(map[string]any{"t": "voice_begin", "id": 2, "bytes": 512})
	sendChunk(p, 2, ramp(512))
	p.write(map[string]any{"t": "voice_end", "id": 2, "parts": 1})

	first := waitVoice(t, sink)
	if first.Err == nil || first.ID != 1 {
		t.Fatalf("first report = %+v, want the stale upload (id 1) reported as failed", first)
	}
	second := waitVoice(t, sink)
	if second.Err != nil || second.ID != 2 {
		t.Fatalf("second report = %+v, want the new upload (id 2) reported as fine", second)
	}
}

// 片属于别的一次上传（id 对不上）：丢掉，不报错——它不是错误，只是过期的片。
func TestChunksFromAnotherUploadAreIgnored(t *testing.T) {
	sink := make(chan VoiceUpload, 4)
	p := newVoicePeer(t, newFakeSource(), sink)

	raw := ramp(512)
	p.write(map[string]any{"t": "voice_begin", "id": 5, "bytes": len(raw)})
	sendChunk(p, 99, ramp(512)) // 另一次上传的片
	sendChunk(p, 5, raw)
	p.write(map[string]any{"t": "voice_end", "id": 5, "parts": 1})

	u := waitVoice(t, sink)
	if u.Err != nil {
		t.Fatalf("the stray chunk broke the upload: %v", u.Err)
	}
	if u.GotParts != 1 || !bytes.Equal(u.Payload, raw) {
		t.Errorf("payload = %d bytes in %d part(s), want exactly the one chunk that belonged here",
			len(u.Payload), u.GotParts)
	}
}

// 转写失败也得在设备屏幕上有个结果。VoiceError 走的是与 voice_text 同一条回执路（转写
// 跑在链路自己的 goroutine 之外，所以两条都用 emit 自己算 seq），而它比 voice_text 更
// 容易漏——「没成」这条路上什么都不发，和设备坏了看起来一模一样。
func TestVoiceErrorReachesTheDevice(t *testing.T) {
	p := newVoicePeer(t, newFakeSource(), make(chan VoiceUpload, 1))

	const want = "转写失败：还没有 英文 的离线识别资产（正在下载）"
	p.srv.VoiceError(7, want)

	m := p.readOf("error")
	if got := m["message"]; got != want {
		t.Errorf("message = %v，期望 %q", got, want)
	}
	if got := m["id"]; got != float64(7) {
		t.Errorf("id = %v，期望 7——设备用它把这条回执对回那次录音", got)
	}
}

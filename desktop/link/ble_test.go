package link

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// 这条链路上唯一能脱离硬件测的部分：一条消息怎么切片。
//
// 它之所以必须有测试，是因为切错的后果在真机上是**静默**的——超长的一次写直接
// 返回错误、整条消息丢掉，而小消息永远暴露不出问题（实测一条 857 字节的 state
// 全丢，同时 131 字节的消息一直好好的）。
func TestALongMessageIsCutToWhatCoreBluetoothAccepts(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 857)

	chunks := bleChunks(body)

	if len(chunks) != 5 {
		t.Fatalf("857 字节应切成 5 片，实际 %d 片", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) > bleWriteChunk {
			t.Errorf("第 %d 片有 %d 字节，超过单次写的上限 %d", i, len(chunk), bleWriteChunk)
		}
	}
	if got := bytes.Join(chunks, nil); !bytes.Equal(got, body) {
		t.Error("切片拼回来和原文不一致——切丢或切重了字节")
	}
}

func TestAShortMessageStaysInOnePiece(t *testing.T) {
	body := []byte("@@{\"t\":\"sync\"}\n")

	chunks := bleChunks(body)

	if len(chunks) != 1 {
		t.Fatalf("短消息不该被切开，实际 %d 片", len(chunks))
	}
	if !bytes.Equal(chunks[0], body) {
		t.Error("单片的内容和原文不一致")
	}
}

func TestAnEmptyMessageProducesNoWrite(t *testing.T) {
	// 空消息必须产生零片：哪怕一片空的 without-response 写也是一次真实的 ATT 写，
	// 而它在设备那边会被当成一个空行。
	if chunks := bleChunks(nil); len(chunks) != 0 {
		t.Errorf("空消息不该产生任何片，实际 %d 片", len(chunks))
	}
}

// 设备名有两个来源（配置、TACHI_BADGE），都可能留下空白；而空白不等于「用默认名」
// 还是「找一个空名字的设备」，这里把它钉死成前者。
func TestTheDefaultNameCoversAnEmptyAddress(t *testing.T) {
	for _, name := range []string{"", "   "} {
		if got := NewBLEListener(name).name; got != DefaultBadgeName {
			t.Errorf("NewBLEListener(%q) 用的名字是 %q，期望 %q", name, got, DefaultBadgeName)
		}
	}
	if got := NewBLEListener(" Tachi-Badge-2 ").name; got != "Tachi-Badge-2" {
		t.Errorf("名字两边的空白应该去掉，得到 %q", got)
	}
}

// 对端消失之后 Read 必须返回。这是整条链路能不能自愈的那条线。
//
// 为什么值得测：库不把断开告诉调用方，incoming 从此再不会有东西，所以除了这一问，
// Read 没有任何别的出口。而它一旦卡住，Server.Run 就永不返回、badgeLoop 也就永远
// 不会 Accept 下一条连接——用户看到的是「设备屏幕停在最后一帧，之后的会话再也刷不
// 上去」，两边却都不报错（第一起真机故障正是这个样子）。
//
// alive 在这里是个假的（真的那半是问 CoreBluetooth，只有真机能验），因为这条判据
// 本身就是这一层唯一与硬件无关的部分。
func TestReadEndsWhenThePeerIsGone(t *testing.T) {
	var alive atomic.Bool
	alive.Store(true)
	tr := &bleTransport{
		incoming: make(chan []byte, 1),
		closed:   make(chan struct{}),
		alive:    alive.Load,
	}

	tr.incoming <- []byte("@@{\"t\":\"sync\"}\n")
	buf := make([]byte, 64)
	if n, err := tr.Read(buf); err != nil || n == 0 {
		t.Fatalf("链路还在时应当读到字节，实际 n=%d err=%v", n, err)
	}

	alive.Store(false)
	done := make(chan error, 1)
	go func() {
		_, err := tr.Read(buf)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errLinkGone) {
			t.Fatalf("对端消失后 Read 报的是 %v，期望 %v", err, errLinkGone)
		}
	case <-time.After(3 * bleLivenessEvery):
		t.Fatal("对端消失后 Read 一直没有返回——这条链路会永远卡住，重连不会发生")
	}
}

// 已经断掉的链路不该再往里写：without-response 的写在发不出去时会一次轮询 10 秒，
// 而它轮询的正是同一件事。这里要的是立刻返回。
func TestWriteRefusesOnceThePeerIsGone(t *testing.T) {
	tr := &bleTransport{
		incoming: make(chan []byte, 1),
		closed:   make(chan struct{}),
		alive:    func() bool { return false },
	}

	if _, err := tr.Write([]byte("@@{\"t\":\"state\"}\n")); !errors.Is(err, errLinkGone) {
		t.Fatalf("链路已断时 Write 报的是 %v，期望 %v", err, errLinkGone)
	}
}

package link

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// 语音上传（设备 → 主机）的接收侧：把分片按 id 攒回一段原始字节。
//
// 为什么分片，见设计文档 §5——一条 64 KB 的行要求两端一起把它捧在内存里，中途失败就
// 整条重来；分片之后丢一片只损失一片，而且设备能显示「已发 3.1/5.0 秒」。
//
// 这一层只管**字节**：攒、验、交出去。不解码、不转写、不碰会话——那是 P1 的事，也是这
// 条链路上唯一需要「理解」的地方。
const (
	// 一次上传最多能声明多少字节。它挡的是**内存**，不是协议：设计里的量是 6 秒的
	// 16 kHz ADPCM（≈48 KB），这里给到 1 MiB——再多就不是一段人说的话了。
	voiceMaxBytes = 1 << 20
	// 半截上传多久没有下文就算死了。判据是**惰性**的（每次收到语音消息顺手看一眼，
	// 见 touchVoice），不引定时器：Server 本就随连接生死，这个超时只防「连接还在、
	// 设备却发了一半就不动了」这一种情况把内存挂住。
	voiceIdleTimeout = 30 * time.Second
)

var (
	errVoiceTooLarge  = errors.New("voice: declared size beyond the limit")
	errVoiceTruncated = errors.New("voice: fewer bytes than declared")
	errVoiceOverrun   = errors.New("voice: more bytes than declared")
	// 中性说法：片数对不上有两个方向——丢了（收到的比报的少）和**作废**（设备取消了，
	// 它报 0 而路上还有已经飞出去的片）。后者正是取消那条路，把它写成「少了」会让人
	// 在日志里读反。
	errVoiceParts = errors.New("voice: chunk count mismatch")
	errVoiceIdle  = errors.New("voice: upload went idle")
)

// VoiceUpload 是一次语音上传的账。
//
// P0-①（量链路吞吐）用它算「每秒多少原始字节」，它同时也是 P1 的输入：Payload 会被
// 送去解码、转写，然后投给当前活跃会话。
type VoiceUpload struct {
	ID       uint64
	Codec    string
	Hz       int
	Declared int           // begin 声明的**上限**（录音的长度要到结束才知道，见下）
	Payload  []byte        // 解出来的原始字节（P1 用它，P0 只看长度）
	Parts    int           // end 报的片数
	GotParts int           // 实际收到的片数
	Duration time.Duration // begin → end 的墙钟时间

	// Err 非 nil 表示这一次没成：声明的量不合理、片数对不上、base64 坏了、超时，
	// 或者连接在中间断了。调用方应当把它当成「一段没有音频的语音」——P1 会据此回
	// 一条 error 给设备，而不是拿半截音频去转写（半句转写出来人看不出少了什么）。
	Err error
}

type voiceUpload struct {
	id       uint64
	codec    string
	hz       int
	declared int
	payload  []byte
	gotParts int
	started  time.Time
	lastSeen time.Time
}

// voiceBegin 开一次上传。
func (s *Server) voiceBegin(msg inbound) {
	// 上一段还没收完就又来一段：那段已经没有下文了（设备一次只录一段），丢掉它，
	// 而不是让两段音频在同一个缓冲里串起来。
	s.discardVoice("a new upload started")

	if msg.Bytes <= 0 || msg.Bytes > voiceMaxBytes {
		s.emitVoice(VoiceUpload{
			ID: msg.ID, Codec: msg.Codec, Hz: msg.Hz, Declared: msg.Bytes,
			Err: fmt.Errorf("%w: %d", errVoiceTooLarge, msg.Bytes),
		})
		return
	}
	now := time.Now()
	s.voice = &voiceUpload{
		id:       msg.ID,
		codec:    msg.Codec,
		hz:       msg.Hz,
		declared: msg.Bytes,
		// 按声明的量先开好：一次上传的大小是 begin 自己报的，攒的过程中不该反复扩容
		// （这块内存是一次性的一段音频，不是长期缓冲）。
		payload:  make([]byte, 0, msg.Bytes),
		started:  now,
		lastSeen: now,
	}
}

// voiceChunk 收一片。
func (s *Server) voiceChunk(msg inbound) {
	v := s.touchVoice()

	if v == nil || msg.ID != v.id {
		return // 不是这一次的片（或者这一次已经死了）：丢掉，不报——它不是错误，只是过期的片
	}
	raw, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		// 解不开就整段作废：少一片的音频转写出来是「半句话」，而屏幕上和日志里都
		// 看不出少了什么。
		s.failVoice(fmt.Errorf("voice: bad chunk %d: %w", v.gotParts+1, err))
		return
	}
	if len(v.payload)+len(raw) > v.declared {
		s.failVoice(fmt.Errorf("%w: %d > %d", errVoiceOverrun, len(v.payload)+len(raw), v.declared))
		return
	}
	v.payload = append(v.payload, raw...)
	v.gotParts++
	v.lastSeen = time.Now()
}

// voiceEnd 收尾：报一次账（成功或失败），并清掉这次的状态。
func (s *Server) voiceEnd(msg inbound) {
	v := s.touchVoice()

	if v == nil {
		return
	}
	s.voice = nil
	out := VoiceUpload{
		ID: v.id, Codec: v.codec, Hz: v.hz,
		Declared: v.declared, Payload: v.payload,
		Parts: msg.Parts, GotParts: v.gotParts,
		Duration: time.Since(v.started),
	}
	// 判据是**片数对上了没有**，而不是「收到的字节 == begin 声明的那一堆」：
	// begin 里的字节数是**上限**（录音多长要到结束才知道，见设备侧 badge_voice.h），
	// 而「到底录了多长」由 end 的 parts 说了算。少片才是真的丢了东西。
	switch {
	case msg.ID != v.id:
		out.Err = fmt.Errorf("voice: end id %d does not match begin id %d", msg.ID, v.id)
	case v.gotParts != msg.Parts:
		out.Err = fmt.Errorf("%w: got %d, device reported %d", errVoiceParts, v.gotParts, msg.Parts)
	case len(v.payload) == 0:
		out.Err = errVoiceTruncated
	}
	s.emitVoice(out)
}

// touchVoice 返回当前这次上传，顺手做惰性超时：没有下文太久就丢掉并报一次。
func (s *Server) touchVoice() *voiceUpload {
	v := s.voice

	if v == nil {
		return nil
	}
	if time.Since(v.lastSeen) > voiceIdleTimeout {
		s.discardVoice(errVoiceIdle.Error())
		return nil
	}
	return v
}

// discardVoice 丢掉半截上传。why 说明为什么——它进日志，因为「收了一半没了」是那种
// 事后一定要能回答「为什么」的事。
func (s *Server) discardVoice(why string) {
	v := s.voice

	if v == nil {
		return
	}
	s.voice = nil
	s.emitVoice(VoiceUpload{
		ID: v.id, Codec: v.codec, Hz: v.hz,
		Declared: v.declared, Payload: v.payload,
		GotParts: v.gotParts, Duration: time.Since(v.started),
		Err: errors.New("voice: " + why),
	})
}

// failVoice 把「这一次已经不可能成了」表达出来：报账，然后清掉状态。
// 剩下的片到达时会被当成过期的片丢掉（见 voiceChunk 的第一道判断）。
func (s *Server) failVoice(err error) {
	v := s.voice

	if v == nil {
		return
	}
	s.voice = nil
	s.emitVoice(VoiceUpload{
		ID: v.id, Codec: v.codec, Hz: v.hz,
		Declared: v.declared, Payload: v.payload,
		GotParts: v.gotParts, Duration: time.Since(v.started),
		Err: err,
	})
}

func (s *Server) emitVoice(u VoiceUpload) {
	if s.OnVoice != nil {
		s.OnVoice(u)
	}
}

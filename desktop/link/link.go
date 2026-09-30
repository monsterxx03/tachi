// Package link 把 desktop 的「等一下」搬到一台小屏设备上：会话在干什么、谁在等一个
// 回答、以及把回答送回去。
//
// 走行分隔 JSON 而不是某个设备的私有二进制：同一份载荷要能过串口、过 BLE，
// 而这两条链路以后还会换（见 docs/2026-09-28-agent-badge-link-design.md）。协议与
// 传输解耦，是这里唯一要紧的结构决定。
//
// 每一行以 "@@" 开头。这不是洁癖：设备与控制台日志共用同一条 USB-Serial-JTAG，
// 日志字节一定会混进这条流，前缀是唯一能把两者分开的东西——ai-passport 那份串口
// 截图协议的经验已经替这件事付过代价（docs/reference/y2lin/serial-screenshot-protocol）。
package link

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/monsterxx03/tachi/agent/tools"
)

// Source 是宿主（desktop）提供的全部事实。link 包自己不持有任何状态：不知道哪个
// 会话在跑、哪条命令在等人——它只把 Source 的变化翻译成消息，把回答送回去。
//
// 这条边界是刻意的：让扛着 agent 的进程持有那张「在等什么」的表，link 才可能只是
// 一层薄翻译，而不是第二本账。
type Source interface {
	// Snapshot 是当前要同步给设备的全部内容。实现必须返回内容和顺序都稳定的结果
	// （见 Server.push 的去抖），且可以并发调用。
	Snapshot() Snapshot

	// Answer 投递一个来自设备的回答。返回的错误会被送回设备显示——设备上按下的
	// 那一下不能没有回音。
	Answer(a Answer) error

	// Subscribe 注册变化回调，返回注销函数。回调可能从任意 goroutine 触发，实现
	// 要假定它随时会来；它只用来叫醒服务循环，不做别的事。
	Subscribe(fn func()) (cancel func())

	// Key 是设备送回来的一个手势（短上/短下/双击确定）。
	//
	// 设备只说「哪一下」，含义由宿主决定——它不知道主窗口在哪里、也不懂「翻一屏」是多大，
	// 那些事全在 desktop/badge.go 的 Key。
	Key(k Key) error

	// TakeAlerts 交出自上次调用以来攒下的提示音请求并清空。
	//
	// 它和 Snapshot 分开，是因为提示音是**事件**（响一次）而不是状态：一次回合完成与
	// 下一次之间可能什么都没变，挂在快照的去抖上就会被吞掉。Server 在每次收到变化
	// 通知时取一次，并在每条连接开始时把攒下的丢掉——连上之前那一声，现在响已经晚了。
	TakeAlerts() []Alert
}

// Key 是设备送回来的一个手势。
//
// Action 是**手势**而不是动作：设备说「你按了短上」，宿主决定那意味着什么（在窗口的会话
// 里往上翻半屏）。动作名留给设备去猜，就是让两端的语义各长一半。
type Key struct {
	Action string `json:"key"`
}

// 三个手势，与设备侧 main.c 的按键路由一一对应。
const (
	KeyUp       = "up"       // 短按上：会话往上翻半屏（与窗口里的 Ctrl+U 同一个动作）
	KeyDown     = "down"     // 短按下：往下翻半屏（Ctrl+D）
	KeyActivate = "activate" // 双击确定：把主窗口换到前台
)

// Alert 是一次提示音请求：Kind 决定设备放哪一段音频（它自己不认识这些含义，只是照做）。
//
// 谁该响（前台/活跃会话）由宿主判断——它才知道这两件事，而设备一条都不知道。
type Alert struct {
	Kind string `json:"kind"`
}

// 提示音的两种含义，与设备侧 badge_proto.h 的 badge_alert_kind_t 一一对应。
const (
	AlertKindAsk  = "ask"  // 出现了一条等你回答的等待（权限确认 / 提问）
	AlertKindDone = "done" // 一个回合跑完了
)

// Snapshot 是要同步给设备的全部内容。
type Snapshot struct {
	Sessions []Session
	Asks     []Ask
}

// Session 是一个会话此刻的状态。形状照抄 desktop 的 AgentState（同样的值域），
// 因为设备要显示的就是那个状态，多一层映射只会多一处漂移。
type Session struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status"`
	Label  string `json:"label,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Tools is how many tool calls this session's CURRENT TURN has started executing
	// (0 = none yet, or nothing running). The device shows it on its own line —
	// 「第 N 个工具调用」— and treats 0 as "draw nothing": a turn that has not touched a
	// tool has no progress to report, and a stale number would read as progress on
	// something that already finished.
	//
	// Omitted when zero, so it also costs nothing on the wire for idle sessions.
	Tools int `json:"tools,omitempty"`
}

// Ask 是一条待答项。
//
// Key 是宿主侧的稳定标识（session + tool call），不发给设备；设备看到的是 ref——
// 本次服务期间分配的行号。设备只回 ref，Server 用它查出这条回答属于谁，这与前端
// 递回 toolCallId 是同一套校验：一条过期的回答必须被拒，而不是批准了此刻正等着的
// 另一件事。
type Ask struct {
	Key       string           `json:"-"`
	Kind      string           `json:"kind"`
	SessionID string           `json:"session"`
	ToolID    string           `json:"toolId"`
	Title     string           `json:"title,omitempty"`
	Body      string           `json:"body,omitempty"`
	Options   []Option         `json:"options,omitempty"`
	Questions []tools.Question `json:"questions,omitempty"`
	Hint      string           `json:"hint,omitempty"`
}

// Option 是设备上可选的一项，Value 是回传的机器值。设备只负责把它送回来，不知道
// allow_once 是什么意思——语义留在宿主这边。
type Option struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// 权限回答的三个机器值，与 desktop 的卡片选择同一套（desktop/permission.go）。
const (
	DecisionAllowOnce    = "allow_once"
	DecisionAllowSession = "allow_session"
	DecisionDeny         = "deny"
)

// Answer 是设备送回来的一个回答。
type Answer struct {
	SessionID   string
	ToolID      string
	Kind        string
	Decision    string            // kind=permission
	Answers     map[string]string // kind=ask_user：问题全文 → 选中的标签
	Annotations map[string]string
}

// Transport 是一条和设备之间的双向字节流。
type Transport interface {
	io.ReadWriteCloser
}

// Listener 提供 Transport：串口上「打开设备」就是 Accept（独占资源，一次一条），
// BLE 上是等一个连接。两者都以 error 表示「现在没有设备」，由调用方退避重试。
type Listener interface {
	Accept(ctx context.Context) (Transport, error)
	Close() error
}

const (
	// prefix 标记一行属于协议（见包注释）。
	prefix = "@@"

	// maxLine 是一条消息的长度上限，超出即丢弃整行。防御性的：一个没有上限的
	// 读循环会被任何一段不含换行的字节喂爆内存，而这条流上恰好可能混进一段
	// 二进制（设备误发了一张图之类的）。
	maxLine = 256 * 1024

	// Proto 是协议版本，设备在 hello 里报，不匹配就断开而不是猜。
	//
	// 3：删掉 `hud`（桌面上那块置顶小窗没了，设备也不再需要据此改底栏），设备的按键手势
	// 从上下/确定/双击变成 上下/双击（`activate`，换主窗口到前台）。旧固件配上新 app 会在
	// 这里得到一句明确的 mismatch——比让它继续跑、而双击确定在屏幕上什么也不发生要强。
	Proto = 3
)

// Server 服务一条连接：把 Source 的变化推出去，把设备的回答送回来。
//
// 除了 write，Server 的字段只在 Run 自己的 goroutine 里读写——推送、回答、去抖
// 全在同一个循环里，所以不需要为它们加锁。wmu 单独立着，是因为 Close 可以从外面
// 进来，而一条写到一半的行比断掉的连接更糟。
type Server struct {
	src Source
	tr  Transport

	// OnVoice 在设备完成（或放弃）一次语音上传时被调用。nil 时那次上传只被丢掉——
	// 攒字节是链路层的事，拿这段音频干什么由宿主决定（P0-① 是量吞吐，P1 是转写）。
	//
	// 它在 handle 的那条 goroutine 上被调用（和 Answer 一样），实现要当自己随时
	// 会被叫到，并且不要阻塞太久——那会挡住这条链路上其余的消息。
	OnVoice func(VoiceUpload)

	wmu sync.Mutex

	seq          uint64
	byKey        map[string]uint64 // ask.Key → ref
	byRef        map[uint64]Ask    // ref → ask
	sentBody     map[uint64]string // ref → 上次发的正文，用于去抖
	lastSessions string
	voice        *voiceUpload // 正在收的那一次语音上传（同时只有一段，见 voice.go）
}

func NewServer(src Source, tr Transport) *Server {
	return &Server{
		src:      src,
		tr:       tr,
		byKey:    make(map[string]uint64),
		byRef:    make(map[uint64]Ask),
		sentBody: make(map[uint64]string),
	}
}

// Run 服务一条连接直到它断了或 ctx 结束。返回错误即代表这条流不可用，重开是调用
// 方的事（desktop 侧的 supervisor 负责退避重试）。
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	changes := make(chan struct{}, 1)
	unsubscribe := s.src.Subscribe(func() {
		select {
		case changes <- struct{}{}:
		default: // 已经有一次待处理的推送，合并掉
		}
	})
	defer unsubscribe()

	lines := make(chan []byte)
	errs := make(chan error, 1)
	go func() { errs <- s.readLoop(ctx, lines) }()

	// 每条连接都是一个全新的 ref 空间：Server 是每条连接一个（见 NewServer），
	// nextSeq 从 0 开始。设备上次拿到的那套 ref 在这条连接上不再有意义，它手里那些
	// 等待项谁也答不了——先让它清空，再收下面这份全量。不清的话，同一条等待会以新
	// ref 再入队一次：屏幕上出现两条，而旧的那条按下去没有任何反应。
	s.writeReset()
	// 这条连接开始之前的提示音请求就地丢掉：那一声是为「刚才」响的，现在响已经晚了
	// ——而连上之后的第一件事是上面那次全量推送，用户看到的是最新的屏幕。
	_ = s.src.TakeAlerts()
	// 先推一份全量：设备可能在主机这边推送过之后才连上，而它需要的一切都在快照里。
	s.push(true)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			return err
		case <-changes:
			// 提示音先发：它是事件，不能被下面那次去抖吞掉（内容没变时 push 一个字
			// 都不写）。
			s.flushAlerts()
			s.push(false)
		case line := <-lines:
			s.handle(line)
		}
	}
}

// flushAlerts 把宿主攒下的提示音请求发给设备。
//
// 不参与 push 的去抖：一次回合完成与下一次之间可能什么都没变，而「什么都没变」正是
// 提示音要说的那件事（「你不在看的时候，事情做完了」）。
func (s *Server) flushAlerts() {
	for _, a := range s.src.TakeAlerts() {
		if a.Kind == "" {
			continue
		}
		s.write(map[string]any{"t": "alert", "seq": s.nextSeq(), "kind": a.Kind})
	}
}

// readLoop 按行读，把属于协议的行交给上层。
//
// 混在这条流上的设备日志必须照样读掉——不读，缓冲区迟早堵死在这条链路上——但只有
// @@ 前缀的行有意义，其余原样丢弃。
func (s *Server) readLoop(ctx context.Context, lines chan<- []byte) error {
	var (
		buf      bytes.Buffer
		chunk    = make([]byte, 512)
		dropping bool // 正在丢弃一条超长行，直到下一个换行
	)
	for {
		n, err := s.tr.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			for {
				i := bytes.IndexByte(buf.Bytes(), '\n')
				if i < 0 {
					break
				}
				raw := buf.Next(i + 1)
				if dropping {
					dropping = false
					continue
				}
				if body, ok := protocolLine(raw); ok {
					select {
					case lines <- body:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
			if buf.Len() > maxLine {
				buf.Reset()
				dropping = true
			}
		}
		if err != nil {
			return err
		}
	}
}

// protocolLine 判断一行是不是给我们的，是就返回去掉前缀与行尾后的正文（拷贝一份：
// 上游的读缓冲马上会被复用）。
func protocolLine(raw []byte) ([]byte, bool) {
	line := bytes.TrimRight(raw, "\r\n")
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil, false
	}
	return append([]byte(nil), line[len(prefix):]...), true
}

// inbound 是设备 → 主机的消息。一个结构承载所有类型：消息短，分支少，
// 拆成一堆类型只会让「这条消息是什么」变得难读。
type inbound struct {
	T           string            `json:"t"`
	Proto       int               `json:"proto,omitempty"`
	Since       uint64            `json:"since,omitempty"`
	Ref         uint64            `json:"ref,omitempty"`
	Decision    string            `json:"decision,omitempty"`
	Answers     map[string]string `json:"answers,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// 语音上传（见 voice.go）。id 由**设备**给（不是主机的 ref）：一次上传不属于
	// 任何一条等待，它只是要送一段音频上来。
	ID    uint64 `json:"id,omitempty"`
	Codec string `json:"codec,omitempty"`
	Hz    int    `json:"hz,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	Data  string `json:"data,omitempty"`
	Parts int    `json:"parts,omitempty"`

	// Key 是设备送回来的一个手势（见 Key 与 Source.Key）。
	Key string `json:"key,omitempty"`
}

func (s *Server) handle(line []byte) {
	var msg inbound
	if err := json.Unmarshal(line, &msg); err != nil {
		return // 看不懂的协议行：忽略这一行，不因为一行坏数据断掉连接
	}

	// 任何一条消息都是检查「半截语音上传是不是已经死了」的时机——设备每 10 秒一条
	// heartbeat，所以一段没有下文的上传不会一直挂在内存里（voice.go 的 touchVoice）。
	s.touchVoice()

	switch msg.T {
	case "hello":
		if msg.Proto != Proto {
			s.write(map[string]any{
				"t": "error", "seq": s.nextSeq(),
				"message": fmt.Sprintf("protocol mismatch: device speaks %d, host speaks %d", msg.Proto, Proto),
			})
			return
		}
		s.push(true)
	case "sync":
		// 设备重连后认为自己漏了东西。全量重发是被去抖拦下的，所以这条路径要 force。
		s.push(true)
	case "answer":
		s.answer(msg)
	case "voice_begin":
		s.voiceBegin(msg)
	case "voice_chunk":
		s.voiceChunk(msg)
	case "voice_end":
		s.voiceEnd(msg)
	case "key":
		// 设备的按键：语义在宿主那边（见 Source.Key）。这里不校验取值——设备的手势
		// 将来可能多一个，而一个不认识的动作用户按下去什么也不发生，
		// 比让整条链路变得脆弱要好。
		_ = s.src.Key(Key{Action: msg.Key})
	}
}

func (s *Server) answer(msg inbound) {
	ask, ok := s.byRef[msg.Ref]
	if !ok {
		// 一条过期的回答（设备界面还停在某个已经被回答或取消的项上）。丢掉，不回话：
		// 这正是 ref 校验存在的意义——「一条不再等待的回答必须被拒」，和
		// AgentService.AnswerPermission 拒绝过期卡片是同一个判断。
		return
	}
	err := s.src.Answer(Answer{
		SessionID:   ask.SessionID,
		ToolID:      ask.ToolID,
		Kind:        ask.Kind,
		Decision:    msg.Decision,
		Answers:     msg.Answers,
		Annotations: msg.Annotations,
	})
	if err != nil {
		// 回音：设备上按下的那一下不能没有结果，否则它会一直举着这个问题。
		s.write(map[string]any{
			"t": "error", "seq": s.nextSeq(), "ref": msg.Ref, "message": err.Error(),
		})
	}
}

// push 把当前快照同步给设备：会话状态、新出现的待答项、消失的待答项。
//
// 去抖在这里：思考中的每个 delta 都会 setSessionState，内容却常常一个字都没变，
// 不去抖就是这条链路一直在说话而设备什么变化都没有。内容比对是可行的，因为
// Source.Snapshot 被要求返回稳定的顺序（见 snapshotAsks）。
func (s *Server) push(force bool) {
	snap := s.src.Snapshot()

	if force {
		// 重同步：让下面每一条都无条件重发一遍。但 ref 的分配要留着——设备上已经
		// 画出来的项必须原地更新，不能换一个身份：换了等于让它重新排一次队，而
		// 旧的那条在这个 ref 表里永远等不到 ask_gone。
		s.sentBody = make(map[uint64]string)
		s.lastSessions = ""
	}

	if body, err := json.Marshal(snap.Sessions); err == nil {
		if string(body) != s.lastSessions {
			s.lastSessions = string(body)
			s.write(map[string]any{"t": "state", "seq": s.nextSeq(), "sessions": snap.Sessions})
		}
	}

	current := make(map[string]Ask, len(snap.Asks))
	for _, a := range snap.Asks {
		current[a.Key] = a
	}

	for _, a := range snap.Asks {
		body, err := json.Marshal(a)
		if err != nil {
			continue
		}
		ref, known := s.byKey[a.Key]
		switch {
		case !known:
			ref = s.nextSeq()
			s.byKey[a.Key] = ref
			s.byRef[ref] = a
			s.sentBody[ref] = string(body)
			s.write(askMessage(ref, a))
		case s.sentBody[ref] != string(body):
			// 同一条待答项的内容变了（罕见，但模型的提问在流式阶段可能被改写）：
			// 用同一个 ref 重发，设备上那一项原地更新，游标不会跳。
			s.byRef[ref] = a
			s.sentBody[ref] = string(body)
			s.write(askMessage(ref, a))
		}
	}

	for key, ref := range s.byKey {
		if _, ok := current[key]; ok {
			continue
		}
		// 已经从宿主这边消失（用户在电脑上答掉了，或者 turn 被停了）。设备必须知道
		// 撤掉它，否则它会一直举着一条已经作废的问题等人按。
		delete(s.byKey, key)
		delete(s.byRef, ref)
		delete(s.sentBody, ref)
		s.write(map[string]any{"t": "ask_gone", "seq": s.nextSeq(), "ref": ref})
	}
}

func askMessage(ref uint64, a Ask) map[string]any {
	msg := map[string]any{
		"t": "ask", "seq": ref, "ref": ref,
		"kind":    a.Kind,
		"session": a.SessionID,
		"toolId":  a.ToolID,
		"title":   askTitle(a),
	}
	if a.Body != "" {
		msg["body"] = a.Body
	}
	if a.Hint != "" {
		msg["hint"] = a.Hint
	}
	if len(a.Options) > 0 {
		msg["options"] = a.Options
	}
	if len(a.Questions) > 0 {
		msg["questions"] = a.Questions
	}
	return msg
}

// askTitle 是设备上那一屏的标题。宿主通常会给一个（它知道这是什么），空着时这里
// 兜底——设备不该显示一块空的标题栏。
func askTitle(a Ask) string {
	if a.Title != "" {
		return a.Title
	}
	switch a.Kind {
	case string(kindPermission):
		return "需要确认"
	case string(kindAskUser):
		return "一个问题"
	}
	return "等待中"
}

// kindPermission / kindAskUser 是 Ask.Kind 的两个值，与 desktop 侧 pendingAskKind
// 的取值一一对应（协议上的字符串，不是内部类型）。
const (
	kindPermission = "permission"
	kindAskUser    = "ask_user"
)

func (s *Server) nextSeq() uint64 {
	s.seq++
	return s.seq
}


// writeReset 告诉设备丢掉它手里的全部等待项，见 Run 里的注释。
func (s *Server) writeReset() {
	s.write(map[string]any{"t": "reset", "seq": s.nextSeq()})
}

// VoiceText 把一次语音转写的结果回给设备：它说了什么，以及那句话到哪了。
//
// 这是唯一一条**从链路自己的 goroutine 之外**写出去的消息（转写要几百毫秒，跑在另一条
// goroutine 上），所以它用 emit 自己算 seq。
func (s *Server) VoiceText(id uint64, text, state string) {
	s.emit(map[string]any{"t": "voice_text", "id": id, "text": text, "state": state})
}

// VoiceError 把「这句话没能变成文本」回给设备：它按了、也说了，屏幕上就得有个结果，
// 否则看起来像是设备坏了（设计文档 §5：屏幕上有字，胜过「按了半天没反应」）。
//
// 与 VoiceText 同一条路（emit），理由也一样：转写跑在链路自己的 goroutine 之外。
// id 是设备给的那次录音的号——设备侧现在不看它（一次只录一段，回执必然是给最近那次的），
// 但协议里有它，这里就别省。
func (s *Server) VoiceError(id uint64, message string) {
	s.emit(map[string]any{"t": "error", "id": id, "message": message})
}

// emit 是 write 的「连 seq 一起算」版本：递增与写出在**同一把锁**里完成，所以跨 goroutine
// 调用不会得到两条顺序颠倒的 seq（谁先加的不一定谁先写出去）。
//
// write 的调用方全在 Run 那条 goroutine 里，自己拿 nextSeq 是安全的——两套写法不是冗余，
// 而是「谁在写」这件事的差别。
func (s *Server) emit(body map[string]any) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	s.seq++
	body["seq"] = s.seq
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	_, _ = s.tr.Write(append(append([]byte(prefix), raw...), '\n'))
}

func (s *Server) write(v any) {
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.tr.Write(append(append([]byte(prefix), body...), '\n'))
}

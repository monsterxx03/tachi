//go:build !headless

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/desktop/stt"
)

// 桌面的语音输入：一个按钮，把麦克风里的一段话变成输入框里的一行字。
//
// 和 badge 那条路的关系要说清楚，因为它决定了这个文件里几乎没有音频代码：
//
//   - **共用转写那一半**（desktop/stt）：同一套 zh-CN/en-US 双模块、同一个 Choose 判据、
//     同一句「为什么没转出来」。它吃的是「16 kHz 单声道 Int16」，而那段样本从哪来不关心
//     ——设备那条路从 BLE 上来（desktop/badge.go 解 ADPCM），这里从麦克风来（stt 的采集
//     半边）。所以这里不重复任何识别逻辑。
//   - **不共用投递**：设备上没有键盘，所以它转出来的字直接成为一条消息（badge:voice，
//     前端 submit→route）；桌面有键盘，所以这里给的是**草稿**——填进输入框，由人改完再
//     回车。这就是为什么是两个事件而不是一个（voice:text vs badge:voice）。
//
// 状态机在 Go 侧，因为「还在录吗」「录了多久」是后端知道的事实：界面只反映它，并决定什么
// 时候请求变化（点一下开始、再点一下结束）。看门狗也在这里——它是一次**后端**的收尾，界面
// 只是被通知到。

// 三个事件：状态、文本、提示。
//
// 分开而不是压成一个「语音事件」：界面要拿它们做三件不同的事——按钮换样子、往输入框里插字、
// 在输入框上方显示一句话。压成一个就得在 payload 里放个 kind 字段，而那正是三个事件名在
// 做的事，只是没人看得见。
const (
	voiceStateEvent  = "voice:state"
	voiceTextEvent   = "voice:text"
	voiceNoticeEvent = "voice:notice"
)

// voiceMaxRecord 是一次录音的上限（看门狗）。
//
// 存在的理由不是「录太长了不好」：是**没有它按钮会永远停在录音态**。人点了开始然后走开、
// 或者被别的事打断，麦克风就一直开着——而这不是用户看得见的状态（只有那个按钮知道他没停）。
// 到点自动收尾，把已经录到的交给识别：说了一半的话总比丢掉强。
//
// 是变量而不是常量，只为单测能把它调到几十毫秒（见 voice_test.go）：看门狗那条路和用户点
// 停止走的是同一个收尾，值得被真的跑一次，而不是等六十秒。生产代码里没有第二处会改它。
var voiceMaxRecord = 60 * time.Second

// voiceState 是按钮要显示的那几个状态。界面只画它，不自己推它——「在录」这件事的后端是
// 麦克风本身，前端无法知道它有没有真的开着。
type voiceState string

const (
	// voiceIdle：什么都没发生（也是每一次收尾的落点）。
	voiceIdle voiceState = "idle"
	// voicePreparing：正在准备——多半是首次使用那次授权窗在等人点，也可能是引擎在起。
	voicePreparing voiceState = "preparing"
	// voiceRecording：麦克风开着，再点一下就结束。
	voiceRecording voiceState = "recording"
	// voiceTranscribing：音频已经到手，识别在跑（几百毫秒到几秒）；此间不接受第二次点击。
	voiceTranscribing voiceState = "transcribing"
)

// voiceInput 是这台机器上那一次录音的状态。
//
// 自己的锁，不是 app 的 mu：录一次是一条几十秒的活，而它绝不能和「一次转写要跑几百毫秒」
// 一起压在 app 级那把锁上（settingsMu 那种长活已经有过一次教训）。锁只护这三个字段，
// 识别本身在锁外跑。
type voiceInput struct {
	mu sync.Mutex
	// recording 是「这次录音还活着」的唯一判据。开始与结束都抢它：谁把它从 true 翻成
	// false，谁就负责收尾——看门狗与用户点停止同时到场时，只有一个人会真的收尾。
	recording bool
	// stopTimer 是上面那条看门狗。
	stopTimer *time.Timer
	// state 是最后一次推给界面的状态（只用于推送去重，不参与判定）。
	state voiceState
	// engine 是这台机器上真正做采集/识别的两个动作。零值表示用真的（engineOf）——正常启动
	// 那条路不必记得初始化它，而单测能换成一个假的（见 voice_test.go）。
	//
	// 它在任何一次录音开始之前写好就不再变，所以读取不必进锁：写入方只有单测的 setup，而
	// 那时还没有任何 goroutine 跑在这上面。
	engine *voiceEngine
}

// voiceEngine 是采集与识别这两个动作，默认就是 stt 的那两个函数。
//
// 抽成字段（而不是直接调 stt）的理由和 link 那条 alive 一样：这一层真正能在主机上验的是
// **状态机**——「重复开始是空操作」「识别中不接受新的开始」「起不来要把占位还回去」「看门狗
// 到点自己收尾」。它们一个都不需要麦克风，而真麦克风、真授权、真识别只能手测。
type voiceEngine struct {
	start      func() error
	stop       func() ([]int16, int, error)
	transcribe func([]int16, int) (stt.Result, error)
}

// defaultVoiceEngine 是这台机器上真的那两个动作。
func defaultVoiceEngine() voiceEngine {
	return voiceEngine{start: stt.StartRecording, stop: stt.StopRecording, transcribe: stt.Transcribe}
}

// engineOf 拿这次要用的实现：没被换过就是真的。
func (v *voiceInput) engineOf() voiceEngine {
	if v.engine != nil {
		return *v.engine
	}
	return defaultVoiceEngine()
}

var (
	errVoiceUnsupported  = errors.New("这台机器做不了本机语音识别（需要 macOS 26 及以上）")
	errVoiceNotRecording = errors.New("当前没有在录音")
	errVoiceBusy         = errors.New("上一次语音还在转写")
)

// VoiceInputAvailable 回答「这个按钮在这台机器上有没有意义」。
//
// 界面据此决定显不显示它。macOS 26 之前转写那半根本不存在（stt.Available 为 false），
// 放一个按了只能报错的按钮不如不放。
func (s *AgentService) VoiceInputAvailable() bool {
	// demo 替身（见 voiceDemoText）不走真麦克风也不走真识别，所以与这台机器的版本无关——
	// smoke 场景因此能在任何 macOS 上跑。
	if _, ok := voiceDemoText(); ok {
		return true
	}
	return stt.Available()
}

// ToggleVoiceInput 是麦克风那一下：**在录就结束，不在录就开始**。
//
// 为什么是「切换」而不是两个方法（开始/停止）：方向只有一个地方知道——后端（它持有录音位）。
// 让调用方各判一次，就多了一处会和它分叉的地方：按钮看的是一个由事件推来的状态，而那个事件
// 可能还在路上（首次授权、识别收尾），按下去就反了。所以按钮和快捷键都调这一个方法。界面当然
// 还是要读状态——那是为了**画样子**（红脉冲、禁用），不是为了让路。
//
// **首次使用时它会先弹一次授权窗并等用户点完**（stt 那半负责，最多 30 秒）。等，是因为调用
// 它的就是「用户按了麦克风」那一下：不等的话第一次使用要按两下，而用户按的正是「开始说话」。
//
// 「这台机器做不做得了」这一关在这里，不在切换里：它是个 UI 前提（转写要在 26 之上），而切换
// 那部分与系统版本无关，所以单测直接测下面那个 toggleVoice。
func (s *AgentService) ToggleVoiceInput() error {
	if !s.VoiceInputAvailable() {
		return errVoiceUnsupported
	}
	return s.desk.toggleVoice()
}

// toggleVoice 是那一台机器上的切换（见 ToggleVoiceInput）。
//
// 这里判的是**方向**（开始还是结束）；「能不能」由下面两个函数各自在锁里再判一次——那是并发
// 面前必须的（双击、按下的瞬间看门狗正好到点），两层不是重复：这一层回答「用户想干什么」，
// 那一层回答「现在允不允许」。
func (d *desktopApp) toggleVoice() error {
	switch d.voice.currentState() {
	case voiceRecording:
		// 收尾那一下不必额外判断：finishVoice 自己抢录音位（看门狗可能正好同时到）。
		return d.finishVoice()
	case voicePreparing, voiceTranscribing:
		// 授权窗还开着 / 识别还在跑：没有可切换的东西。
		//
		// 这里**不报错**：按下去的那一刻用户并没有做错什么，只是还没轮到——而那条「上一次
		// 语音还在转写」的提示在这种时序里只会是噪声（transcribing 通常不足一秒）。
		return nil
	default:
		// voiceIdle。default 而不是 case voiceIdle 是有意的：**零值也走这里**——app 刚起来
		// 时 state 还是空串（一次状态都还没推过），而那显然该走「开始」。
		return d.startVoice()
	}
}

// startVoice 从零开始一次录音（见 toggleVoice）。
func (d *desktopApp) startVoice() error {
	d.voice.mu.Lock()
	if d.voice.recording {
		d.voice.mu.Unlock()
		return nil
	}
	if d.voice.state == voiceTranscribing {
		// 上一次的识别还在跑：这时开新的一条会让两段文字同时插进输入框，而顺序无从保证。
		// 这个判断必须和下面那次「占住」在同一把锁里——否则两次点击可以一起通过它。
		d.voice.mu.Unlock()
		return errVoiceBusy
	}
	// 先占住这次录音，再去做那件可能要等几十秒的事（授权窗）：等的过程中第二次按下必须
	// 看到「已经在录」，而不是又开一条。
	d.voice.recording = true
	d.voice.mu.Unlock()

	// 这个状态在授权之前就要说出去，界面才能显示「准备中」而不是一个没反应的按钮。
	d.setVoiceState(voicePreparing)
	if _, demo := voiceDemoText(); !demo {
		if err := d.voice.engineOf().start(); err != nil {
			d.abandonVoice()
			return voiceStartError(err)
		}
	}

	d.voice.mu.Lock()
	d.voice.stopTimer = time.AfterFunc(voiceMaxRecord, func() {
		badgeLog().Info(context.Background(), "desktop voice: 到上限自动收尾", "max", voiceMaxRecord)
		_ = d.finishVoice()
	})
	d.voice.mu.Unlock()

	d.setVoiceState(voiceRecording)
	return nil
}

// finishVoice 是这次录音唯一的收尾入口（用户点停止、看门狗到点，两条路都走这里）。
//
// 抢 d.voice.recording 来决定谁收尾：另一个调用者拿到的是一句「当前没有在录音」，那不是用户
// 的错误，所以它只是一条日志，不进界面。识别不等结果——转写要几百毫秒到几秒，而这一次点击该
// 立刻有反应（按钮进入「识别中」）。文本或失败随后由 voice:text / voice:notice 送到前端，
// 和 badge 那条路同一个形状（那边也是识别完才回执）。
func (d *desktopApp) finishVoice() error {
	d.voice.mu.Lock()
	if !d.voice.recording {
		d.voice.mu.Unlock()
		return errVoiceNotRecording
	}
	d.voice.recording = false
	if d.voice.stopTimer != nil {
		d.voice.stopTimer.Stop()
		d.voice.stopTimer = nil
	}
	d.voice.mu.Unlock()

	text, demo := voiceDemoText()
	if demo {
		// 替身（smoke 专用）：采集与识别都换掉，但**走的状态与事件和真的一模一样**——
		// 场景要证明的就是这一半（按钮、事件、文字落到哪），它不该依赖一台有麦克风的机器。
		d.setVoiceState(voiceTranscribing)
		go d.emitVoiceText(text)
		return nil
	}

	pcm, hz, err := d.voice.engineOf().stop()
	if err != nil {
		// 垫片那边已经没有这次采集了（重复收尾之外，也可能是引擎自己掉了）。没地方转写，
		// 只能如实说一句。
		badgeLog().Warn(context.Background(), "desktop voice: 采集没有收下来", "err", err)
		d.voiceNotice("录音已经停了")
		d.setVoiceState(voiceIdle)
		return nil
	}
	d.setVoiceState(voiceTranscribing)
	// 识别放另一个 goroutine：它要几百毫秒到几秒，而这次调用是「用户点了停止」——让它等
	// 在那里只是把按钮的响应也一起拖住（badge 那条路同理）。
	go d.transcribeVoiceInput(pcm, hz)
	return nil
}

// transcribeVoiceInput 把刚录下来的样本交给识别，然后把结果送到前端。
//
// 三条出路都要落到界面上：文本进输入框、失败说一句为什么、没听到也要说（空文本不是错误，
// 但一个点了没反应的按钮才是真的坏）。这里没有第四条「悄悄失败」。
func (d *desktopApp) transcribeVoiceInput(pcm []int16, hz int) {
	ctx := context.Background()
	if len(pcm) == 0 {
		// 按下了但没说话：识别会如实回答一句空文本，但没必要为此跑一趟引擎。
		d.voiceNotice("没有听到内容")
		d.setVoiceState(voiceIdle)
		return
	}
	started := time.Now()
	got, err := d.voice.engineOf().transcribe(pcm, hz)
	if err != nil {
		// 原因原文进日志（「哪门语言缺离线资产」和「引擎坏了」在 err 里分得开），界面上那句
		// 人话由 voiceFailureText 决定——和设备那条路用的是同一个函数。
		badgeLog().Warn(ctx, "desktop voice: transcription failed",
			"err", err, "missing", strings.Join(got.Missing, ","))
		d.voiceNotice(voiceFailureText(got.Installing))
		d.setVoiceState(voiceIdle)
		return
	}
	if len(got.Missing) > 0 {
		// 降级：少的那门语言这次没跑成（多半是它的离线资产不在）。这件事不进日志，就等于
		// 悄悄给出一句质量更差的文本。
		badgeLog().Warn(ctx, "desktop voice: 有语言没跑成，这次只用剩下的那门转写",
			"missing", strings.Join(got.Missing, ","),
			"installing", strings.Join(got.Installing, ","))
	}
	d.setVoiceState(voiceIdle)
	d.emitVoiceText(got.Text)
	badgeLog().Info(ctx, "desktop voice: transcribed",
		"samples", len(pcm), "ms", time.Since(started).Milliseconds(),
		"chars", len([]rune(got.Text)))
}

// emitVoiceText 把一行识别结果送给前端，让它填进输入框，并让按钮回到空闲。
//
// 空文本在这里止步：往里塞一个空串会让输入框动一下（焦点、光标），而用户什么都没说——但那种
// 情况同样要收尾，所以「没有听到内容」和 idle 都在这儿。
//
// 收尾（idle）放在这里而不是每个调用点：这条路有三条出口（替身、识别成功、没听清），把它们
// 各写一遍的代价是——漏掉一处，按钮就永远停在「识别中」，而那个状态是禁用的。
func (d *desktopApp) emitVoiceText(text string) {
	if text = strings.TrimSpace(text); text == "" {
		d.voiceNotice("没有听到内容")
	} else {
		d.emitVoice(voiceTextEvent, map[string]any{"text": text})
	}
	d.setVoiceState(voiceIdle)
}

// abandonVoice 放弃这次录音：采集那半边起不来，而上面已经把 recording 占住了——不还回去，
// 这个按钮就再也开不了口。
func (d *desktopApp) abandonVoice() {
	d.voice.mu.Lock()
	d.voice.recording = false
	d.voice.mu.Unlock()
	d.setVoiceState(voiceIdle)
}

// emitVoice 推一条语音事件。
//
// 守卫 d.app 的处境和 emitState 一样：单测里没有 Wails app（那是启动期才有的东西），而状态机
// 的判定不该因此测不了——它跑在 d.app 为 nil 的 app 上照样要给出正确的状态。
func (d *desktopApp) emitVoice(name string, payload map[string]any) {
	if d.app == nil {
		return
	}
	d.emit(name, payload)
}

// voiceNotice 把一句要讲给用户的话送到界面（转写失败、没听到内容）。
func (d *desktopApp) voiceNotice(message string) {
	badgeLog().Info(context.Background(), "desktop voice: notice", "message", message)
	d.emitVoice(voiceNoticeEvent, map[string]any{"message": message})
}

// currentState 读当前状态（单测用它断言收尾落在哪；判定本身不靠它——见 startVoice 里那次
// 「同一把锁里」的注释）。
func (v *voiceInput) currentState() voiceState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

// setVoiceState 推进状态并推给界面（同值不重推）。
// 去重不是省流量的洁癖：一次录音里同一个状态会被设两次（收尾那条路与识别那条路都要落到
// idle），重复的状态推送会让界面把同一个动画从头播一遍。
func (d *desktopApp) setVoiceState(state voiceState) {
	d.voice.mu.Lock()
	if d.voice.state == state {
		d.voice.mu.Unlock()
		return
	}
	d.voice.state = state
	d.voice.mu.Unlock()
	d.emitVoice(voiceStateEvent, map[string]any{"state": string(state)})
}

// voiceStartError 把采集那半的失败变成一句能讲给用户听的话。
//
// 三句话对应三种不同的事，它们的**出路**不同，所以不能压成一句「录音失败」：
// 授权窗还开着是「再点一次」、被拒是「去系统设置里改」、起不来是「这台机器/这个设备的问题」。
// 用户能做的动作既然不一样，话就得不一样。
func voiceStartError(err error) error {
	switch {
	case errors.Is(err, stt.ErrMicUnasked):
		return errors.New("麦克风授权窗口还开着：点完「允许」之后，再按一次麦克风")
	case errors.Is(err, stt.ErrMicDenied):
		return errors.New("麦克风权限被拒绝：系统设置 → 隐私与安全性 → 麦克风 里打开 Tachi（改完可能要重开这个应用）")
	case errors.Is(err, stt.ErrMicUnavailable):
		return errors.New("麦克风起不来：这台机器没有输入设备，或者它正被别的程序占着")
	default:
		return err
	}
}

// voiceDemoText 是 smoke 用的采集替身（TACHI_DEMO_VOICE_TEXT）。
//
// 为什么必须有它：smoke 跑在**用户自己的机器**上，而真的开麦会弹一次 TCC 窗、并录进房间里
// 正在发生的事。一个自动化用例两样都不能要。它要证明的是**按钮这一半**——点一下进入录音态、
// 再点一下把文字填进输入框、而且**不**发出去——而那一半与音频从哪里来无关。
//
// 替身只换「采集 + 识别」，换不掉状态与事件：那两条路走的还是同一份代码。这正是它值得作为
// 场景的理由，也是它的边界（真麦克风、真权限、真识别的质量，只能手测）。
//
// 只在 demo 模式下生效：普通启动里 demoEnabled() 为 false，这个环境变量也就无从影响。
func voiceDemoText() (string, bool) {
	if !demoEnabled() {
		return "", false
	}
	if text := os.Getenv(voiceDemoTextEnv); text != "" {
		return text, true
	}
	return "", false
}

// voiceDemoTextEnv 是上面那个替身读的环境变量名。
//
// 名字在这里定义，而 smoke 的沙箱那边是**另抄一份**的（itest/desktop/sandbox.go）——它不能
// 导入 app 自己的包，TACHI_DEMO_BROWSER_FETCH 与那个专属可执行名也是同样的抄法。
const voiceDemoTextEnv = "TACHI_DEMO_VOICE_TEXT"

package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/desktop/stt"
)

// 这个文件测的是**状态机**——「重复开始是空操作」「起不来要把占位还回去」「识别中不接受新的
// 开始」「看门狗到点自己收尾」——因为它是桌面语音这一层里唯一能在没有麦克风的机器上验的部分。
//
// 真麦克风、真授权、真识别只能手测（`make -C desktop run`，按一次按钮），所以这里的假引擎
// 不是「为了测试而测试」：没有它，上面那几条判定就只有一条出路——人肉试。这和 link 里那条
// alive 是同一个道理（那条判据是那一层唯一能在主机上验的部分）。
//
// 事件本身断言不了：单测里没有 Wails app（emit 会被 emitVoice 的 nil 守卫丢掉，见那边的
// 注释）。「文字落到输入框、且没发出去」那半边由 smoke 的 voice-input 场景负责。

// fakeVoice 是一个假的采集/识别。零值就是「一切正常、什么都没录到」。
type fakeVoice struct {
	mu sync.Mutex

	startCalls int
	startErr   error
	// startGate 非 nil 时，开始采集会一直等到它关上——那是**授权窗**那一段（真人点「允许」
	// 之前的几十秒），也是「第二次按下必须看到已经在录」这条规则真正生效的地方。
	startGate chan struct{}
	// samples 是 stop 要交回的东西；hz 是它的采样率。
	samples []int16
	hz      int
	stopErr error

	transcribeCalls int
	transcribeErr   error
	text            string
	gotSamples      []int16
	gotHz           int
	// gate 非 nil 时，识别会一直等到它关上——用来把「识别中」那一段停住观察。
	gate chan struct{}
}

func (f *fakeVoice) engine() voiceEngine {
	return voiceEngine{start: f.start, stop: f.stop, transcribe: f.transcribe}
}

func (f *fakeVoice) start() error {
	f.mu.Lock()
	f.startCalls++
	gate := f.startGate
	err := f.startErr
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

func (f *fakeVoice) stop() ([]int16, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return nil, 0, f.stopErr
	}
	hz := f.hz
	if hz == 0 {
		hz = 16000
	}
	return f.samples, hz, nil
}

func (f *fakeVoice) transcribe(pcm []int16, hz int) (stt.Result, error) {
	f.mu.Lock()
	f.transcribeCalls++
	f.gotSamples = pcm
	f.gotHz = hz
	gate := f.gate
	err := f.transcribeErr
	text := f.text
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return stt.Result{}, err
	}
	return stt.Result{Text: text}, nil
}

func (f *fakeVoice) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls, f.transcribeCalls
}

// newVoiceApp 造一个只装了假引擎的 app。
//
// 用 newTestApp（fileservice_test.go）而不是自己拼一个：它就是这个包里「一个 app 的最小
// 形状」，多一处自建的东西就多一处会走偏的初始化。d.app 保持 nil——状态机不该依赖它。
func newVoiceApp(f *fakeVoice) *desktopApp {
	d := newTestApp()
	e := f.engine()
	d.voice.engine = &e
	return d
}

// waitVoiceState 等状态机上某一点（收尾那条路是另一个 goroutine 跑的）。
func waitVoiceState(t *testing.T, d *desktopApp, want voiceState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d.voice.currentState() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等状态 %q 超时，当前是 %q", want, d.voice.currentState())
}

// TestVoiceSecondStartIsANoOp 钉的是双击：第二次按下不该重建引擎，也不该报错——界面上的
// 「开始」按钮和第一声点击之间隔着一次授权、一次引擎启动，人完全可能再点一下。
func TestVoiceSecondStartIsANoOp(t *testing.T) {
	f := &fakeVoice{}
	d := newVoiceApp(f)

	if err := d.startVoice(); err != nil {
		t.Fatalf("第一次开始: %v", err)
	}
	if err := d.startVoice(); err != nil {
		t.Errorf("已经在录时再开始应当是空操作，得到 %v", err)
	}
	if starts, _ := f.counts(); starts != 1 {
		t.Errorf("采集被开了 %d 次, want 1（重复点击不该重建引擎）", starts)
	}
	if got := d.voice.currentState(); got != voiceRecording {
		t.Errorf("状态 = %q, want %q", got, voiceRecording)
	}
	if err := d.finishVoice(); err != nil {
		t.Fatalf("收尾: %v", err)
	}
	waitVoiceState(t, d, voiceIdle)
}

// TestVoiceFailedStartReleasesTheMicrophone：采集起不来时，那个占位必须还回去——不还的话
// 这个按钮就再也开不了口了（records 已经占住，而引擎根本没在录）。
func TestVoiceFailedStartReleasesTheMicrophone(t *testing.T) {
	f := &fakeVoice{startErr: stt.ErrMicDenied}
	d := newVoiceApp(f)

	err := d.startVoice()
	if err == nil {
		t.Fatal("采集起不来，startVoice 却成功了")
	}
	// 那句话要说清「怎么办」：被拒的出路是去系统设置里改，不是再点一次。
	if !strings.Contains(err.Error(), "系统设置") {
		t.Errorf("错误 %q 没说清出路", err.Error())
	}
	if got := d.voice.currentState(); got != voiceIdle {
		t.Errorf("失败后状态 = %q, want %q（失败的开始不该把按钮留在录音态）", got, voiceIdle)
	}

	// 占位真的还回去了：这一次能从头再走一遍。
	f.mu.Lock()
	f.startErr = nil
	f.mu.Unlock()
	if err := d.startVoice(); err != nil {
		t.Errorf("重新开始: %v（上一次失败把占位留下了）", err)
	}
	if starts, _ := f.counts(); starts != 2 {
		t.Errorf("采集被开了 %d 次, want 2", starts)
	}
	_ = d.finishVoice()
	waitVoiceState(t, d, voiceIdle)
}

// TestVoiceEmptyRecordingNeverReachesTheRecognizer：按下了没说话。识别会如实回一句空文本，
// 但那一趟没必要跑（引擎要几百毫秒），而且它必须**收尾**——不然按钮停在「识别中」，而那是
// 一个禁用状态。
func TestVoiceEmptyRecordingNeverReachesTheRecognizer(t *testing.T) {
	f := &fakeVoice{text: "不该被问到"}
	d := newVoiceApp(f)

	if err := d.startVoice(); err != nil {
		t.Fatalf("开始: %v", err)
	}
	if err := d.finishVoice(); err != nil {
		t.Fatalf("收尾: %v", err)
	}
	waitVoiceState(t, d, voiceIdle)
	if _, calls := f.counts(); calls != 0 {
		t.Errorf("空录音跑了 %d 次识别, want 0", calls)
	}
}

// TestVoiceRecordsWhatWasCaptured：录到的东西要原样交给识别——样本和采样率都对。
func TestVoiceRecordsWhatWasCaptured(t *testing.T) {
	f := &fakeVoice{samples: []int16{1, 2, 3, 4}, hz: 16000, text: "帮我看一下这个报错"}
	d := newVoiceApp(f)

	if err := d.startVoice(); err != nil {
		t.Fatalf("开始: %v", err)
	}
	if err := d.finishVoice(); err != nil {
		t.Fatalf("收尾: %v", err)
	}
	waitVoiceState(t, d, voiceIdle)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.gotSamples) != 4 || f.gotSamples[0] != 1 || f.gotSamples[3] != 4 {
		t.Errorf("交给识别的样本 = %v, want [1 2 3 4]", f.gotSamples)
	}
	if f.gotHz != 16000 {
		t.Errorf("采样率 = %d, want 16000", f.gotHz)
	}
}

// TestVoiceRefusesANewRecordingWhileTranscribing：识别在跑的那几百毫秒里不接受新的录音——
// 两条录下来会同时往输入框里插字，而顺序无从保证。这一段也是按钮被禁用的原因。
func TestVoiceRefusesANewRecordingWhileTranscribing(t *testing.T) {
	f := &fakeVoice{samples: []int16{1}, text: "第一句", gate: make(chan struct{})}
	d := newVoiceApp(f)

	if err := d.startVoice(); err != nil {
		t.Fatalf("开始: %v", err)
	}
	if err := d.finishVoice(); err != nil {
		t.Fatalf("收尾: %v", err)
	}
	waitVoiceState(t, d, voiceTranscribing)
	if err := d.startVoice(); !errors.Is(err, errVoiceBusy) {
		t.Errorf("识别中的开始 = %v, want errVoiceBusy", err)
	}

	close(f.gate)
	waitVoiceState(t, d, voiceIdle)
	// 收尾之后又能开了。
	if err := d.startVoice(); err != nil {
		t.Errorf("识别结束后的开始: %v", err)
	}
	_ = d.finishVoice()
}

// TestVoiceWatchdogEndsAnAbandonedRecording：人点了开始然后走开。到点必须自己收尾——麦克风
// 不能一直开着，而屏幕上看不出这件事（那个按钮是唯一的提示）。录到的一半照样交出去。
func TestVoiceWatchdogEndsAnAbandonedRecording(t *testing.T) {
	saved := voiceMaxRecord
	voiceMaxRecord = 20 * time.Millisecond
	t.Cleanup(func() { voiceMaxRecord = saved })

	f := &fakeVoice{samples: []int16{1, 2}, text: "说了一半"}
	d := newVoiceApp(f)

	if err := d.startVoice(); err != nil {
		t.Fatalf("开始: %v", err)
	}
	waitVoiceState(t, d, voiceIdle) // 没人点停止，看门狗自己收的尾
	if _, calls := f.counts(); calls != 1 {
		t.Errorf("看门狗收尾后跑了 %d 次识别, want 1（已经录到的要交出去）", calls)
	}
	// 收尾是干净的：录音位已经放开，下一次能开。
	if err := d.startVoice(); err != nil {
		t.Errorf("看门狗收尾后再开始: %v", err)
	}
	_ = d.finishVoice()
}

// TestVoiceStopWithoutARecordingSaysSo：没有在录时收尾不是用户的错误——它只该是一条「没事
// 发生」的返回值（看门狗与手点停止撞在一起时，那条路就是这里进来的）。
func TestVoiceStopWithoutARecordingSaysSo(t *testing.T) {
	d := newVoiceApp(&fakeVoice{})
	if err := d.finishVoice(); !errors.Is(err, errVoiceNotRecording) {
		t.Errorf("没在录时收尾 = %v, want errVoiceNotRecording", err)
	}
}

// TestVoiceToggleFlipsThroughOneEntryPoint 是按钮和快捷键共用的那一下：**在录就结束，不在录
// 就开始**。方向由后端决定，所以两个入口不可能对同一次按下给出两种意思。
func TestVoiceToggleFlipsThroughOneEntryPoint(t *testing.T) {
	f := &fakeVoice{samples: []int16{1, 2}, text: "帮我看一下这个报错"}
	d := newVoiceApp(f)

	if err := d.toggleVoice(); err != nil {
		t.Fatalf("第一次切换（开始）: %v", err)
	}
	if got := d.voice.currentState(); got != voiceRecording {
		t.Fatalf("开始后状态 = %q, want %q", got, voiceRecording)
	}
	if err := d.toggleVoice(); err != nil {
		t.Fatalf("第二次切换（结束）: %v", err)
	}
	waitVoiceState(t, d, voiceIdle)
	if _, calls := f.counts(); calls != 1 {
		t.Errorf("跑了一次完整的开始+结束，识别次数 = %d, want 1", calls)
	}
}

// TestVoiceToggleIsSilentWhileWaitingForPermission：授权窗还开着的时候再按一下，什么也不该
// 发生——不能开第二条采集，也不该报错（那一刻用户没做错什么，只是还没轮到）。
//
// 这一段正是「方向由后端决定」要防的：界面此时收到的是 preparing，本地按一下会以为该「停止」。
func TestVoiceToggleIsSilentWhileWaitingForPermission(t *testing.T) {
	f := &fakeVoice{startGate: make(chan struct{})}
	d := newVoiceApp(f)

	started := make(chan error, 1)
	go func() { started <- d.toggleVoice() }() // 卡在授权窗那一步
	waitVoiceState(t, d, voicePreparing)

	if err := d.toggleVoice(); err != nil {
		t.Errorf("授权窗开着时的第二次按下 = %v, want nil（不该报错）", err)
	}
	if starts, _ := f.counts(); starts != 1 {
		t.Errorf("采集被开了 %d 次, want 1（授权窗里第二次按下不该再开一条）", starts)
	}

	close(f.startGate)
	if err := <-started; err != nil {
		t.Fatalf("首次切换: %v", err)
	}
	waitVoiceState(t, d, voiceRecording)
	_ = d.finishVoice()
	waitVoiceState(t, d, voiceIdle)
}

// TestVoiceToggleIsSilentWhileTranscribing：识别在跑的那不到一秒里再按一下，同样什么也不该
// 发生——不报错（按钮此刻是禁用的，只有快捷键能走到这里，而一条「上一次语音还在转写」的提示
// 在这种时序里只会是噪声），也不开第二条。
func TestVoiceToggleIsSilentWhileTranscribing(t *testing.T) {
	f := &fakeVoice{samples: []int16{1}, text: "第一句", gate: make(chan struct{})}
	d := newVoiceApp(f)

	if err := d.toggleVoice(); err != nil {
		t.Fatalf("开始: %v", err)
	}
	if err := d.toggleVoice(); err != nil {
		t.Fatalf("结束: %v", err)
	}
	waitVoiceState(t, d, voiceTranscribing)

	if err := d.toggleVoice(); err != nil {
		t.Errorf("识别中的按下 = %v, want nil", err)
	}
	if starts, _ := f.counts(); starts != 1 {
		t.Errorf("采集被开了 %d 次, want 1", starts)
	}

	close(f.gate)
	waitVoiceState(t, d, voiceIdle)
}

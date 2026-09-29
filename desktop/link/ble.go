package link

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// 服务与特征的 UUID，与设备侧 main/badge_ble.c 里那三个常量一一对应。
const (
	badgeServiceUUID = 0xFFF0
	badgeRXUUID      = 0xFFF1 // 主机 → 设备（write）
	badgeTXUUID      = 0xFFF2 // 设备 → 主机（notify）
)

// DefaultBadgeName 是设备广播的名字（设备侧 BADGE_BLE_NAME）。
const DefaultBadgeName = "Tachi-Badge"

const (
	// bleWriteChunk 是一次 without-response 写的长度上限。
	//
	// macOS 上 CoreBluetooth 的 maximumWriteValueLength(for:.withoutResponse) 大约是
	// 182 字节，而 tinygo 的库没有把它暴露出来。超过它的一次写会直接失败——实测一条
	// 854 字节的 state 整个丢掉，而 131 字节的消息永远暴露不出这个问题。所以这里自己
	// 切片；设备侧本来就把多段写累积到换行为止，它天生接得住。
	bleWriteChunk = 180

	// bleWritePace 是两片之间的停顿。连着灌满 CoreBluetooth 的发送队列会拿到
	// "timed out waiting for buffer space"，而这个错误在 without-response 下没有
	// 别的地方能提前发现。
	bleWritePace = 15 * time.Millisecond

	// bleScanWindow 是等设备出现在广播里的最长时间。
	bleScanWindow = 15 * time.Second

	// blePairTimeout 是「连上之后等链路变安全」的上限。
	//
	// 第一次配对要用户在电脑上敲 6 位码，所以它必须比一次点击长得多；又不能是无限的
	// ——对方一直不输码时，这条循环得能退出来重试（见 desktop 侧的 badgeLoop）。
	blePairTimeout = 90 * time.Second

	// blePairRetry 是两次探测写之间的间隔。
	//
	// 库自己的写超时**短于**「读码 + 敲码」那段时间，所以一次失败绝不足以判定配对
	// 失败：配对是在 CoreBluetooth 那边继续跑的，隔一会儿再写一次就能看见它已经生效。
	blePairRetry = 2 * time.Second

	// bleLivenessEvery 是「它还在吗」的检查间隔。
	//
	// 这一问是必须的，因为**库不会把断开告诉调用方**：darwin 侧的
	// DidDisconnectPeripheral 只喂给 Connect 自己那个等待通道，而 Read 除了 Close
	// 之外没有任何返回路径。于是对端消失时这条链路会原地静默：Server 读不到东西、
	// 因而永不退出，badgeLoop 也就永远不会 Accept 下一条连接——症状是「设备屏幕停在
	// 最后一帧，之后的会话再也刷不上去」，而两侧都不报错。
	bleLivenessEvery = 2 * time.Second
)

// errLinkGone 表示对端已经不在了。
//
// 它必须让 Read 返回，而不是只在某处记一行日志：Server.Run 靠 readLoop 的返回值退出、
// badgeLoop 靠 Server.Run 的返回值重新 Accept——链路死掉的信号只有走完这条路，
// 重连才会发生。
var errLinkGone = errors.New("bluetooth link is gone")

// 适配器是进程级的单例，Enable 只能成功调一次，所以用 Once 记住结果。
var (
	bleAdapterOnce sync.Once
	bleAdapterErr  error
)

func bleAdapter() (*bluetooth.Adapter, error) {
	bleAdapterOnce.Do(func() {
		bleAdapterErr = bluetooth.DefaultAdapter.Enable()
	})
	if bleAdapterErr != nil {
		return nil, fmt.Errorf("bluetooth unavailable: %w", bleAdapterErr)
	}
	return bluetooth.DefaultAdapter, nil
}

// disconnect 关掉一条连接——只在真有连接的时候。
//
// 零值 Device 上的 Disconnect 是 nil 解引用 panic（库在连接失败时返回的正是「零值
// Device + nil error」，所以这条路径真的会走到），而「地址是零」恰好就是连接没建立
// 的判据，Accept 里判的也是它。
func disconnect(device bluetooth.Device) {
	if device.Address == (bluetooth.Address{}) {
		return
	}
	_ = device.Disconnect()
}

// BLEListener 按广播名字找一个 badge，连上它，并等到链路**已认证加密**才交出去。
//
// 等待那一步不是可选的：这条链路上的一个回答就是在批准一条要执行的命令，而设备侧
// 对明文写入是直接拒绝的（所以早交出去只会让协议层收到一堆错误）。加密的过程——
// 第一次还包括用户在电脑上输 6 位配对码——就发生在这里。
type BLEListener struct {
	name string
}

// NewBLEListener 构造一个蓝牙链路。name 为空时用设备默认的广播名；传名字是为了
// 同一台电脑旁边可能有第二块板子（改过名的）。
func NewBLEListener(name string) *BLEListener {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultBadgeName
	}
	return &BLEListener{name: name}
}

// Close 什么都不用做：链路由 Transport 自己断开，下一次 Accept 重新找一遍。
func (l *BLEListener) Close() error { return nil }

func (l *BLEListener) Accept(ctx context.Context) (Transport, error) {
	adapter, err := bleAdapter()
	if err != nil {
		return nil, err
	}
	found, err := l.scan(adapter, ctx)
	if err != nil {
		return nil, err
	}
	device, err := adapter.Connect(found.Address, bluetooth.ConnectionParams{})
	// ⚠️ 这里的零值检查不是多余的：库在连接失败时会返回**零值 Device 加 nil error**，
	// 之后任何一次方法调用都是 nil 解引用 panic（实测崩过两次）。也就是说，单看 err
	// 判断连接成功与否会直接让 desktop 挂掉——而下面这个分支里连 Disconnect 都不能
	// 调，它也是那些方法之一。
	if err != nil || device.Address == (bluetooth.Address{}) {
		disconnect(device)
		if err == nil {
			err = errors.New("connect failed")
		}
		return nil, err
	}

	rx, tx, err := findBadgeCharacteristics(device)
	if err != nil {
		disconnect(device)
		return nil, err
	}
	tr := newBLETransport(device, rx)
	if err := tr.subscribe(tx); err != nil {
		_ = tr.Close()
		return nil, err
	}
	if err := tr.waitSecure(ctx); err != nil {
		_ = tr.Close()
		return nil, err
	}
	return tr, nil
}

// scan 等一个广播名字匹配的设备出现。设备不在广播范围内时这就是「现在没有设备」，
// 与串口找不到端口是同一件事，交给调用方退避重试。
func (l *BLEListener) scan(adapter *bluetooth.Adapter, ctx context.Context) (bluetooth.ScanResult, error) {
	found := make(chan bluetooth.ScanResult, 1)

	go func() {
		_ = adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
			if r.LocalName() != l.name {
				return
			}
			select {
			case found <- r:
			default: // 已经抓到一个了，重复的回调丢掉
			}
		})
	}()

	timer := time.NewTimer(bleScanWindow)
	defer timer.Stop()
	select {
	case r := <-found:
		_ = adapter.StopScan()
		return r, nil
	case <-ctx.Done():
		_ = adapter.StopScan()
		return bluetooth.ScanResult{}, ctx.Err()
	case <-timer.C:
		_ = adapter.StopScan()
		return bluetooth.ScanResult{}, fmt.Errorf("no device named %q", l.name)
	}
}

// findBadgeCharacteristics 在设备上找到 RX（写）和 TX（订阅）两个特征。
//
// 按 UUID 找而不是按顺序取：GATT 表里的顺序是实现细节，而这两个特征一写一读，
// 认错了就是把协议写进了通知口。
func findBadgeCharacteristics(device bluetooth.Device) (rx, tx bluetooth.DeviceCharacteristic, err error) {
	services, err := device.DiscoverServices(nil)
	if err != nil {
		return rx, tx, fmt.Errorf("discover services: %w", err)
	}
	wantService := bluetooth.New16BitUUID(badgeServiceUUID)
	wantRX := bluetooth.New16BitUUID(badgeRXUUID)
	wantTX := bluetooth.New16BitUUID(badgeTXUUID)

	for _, service := range services {
		if service.UUID() != wantService {
			continue
		}
		chars, err := service.DiscoverCharacteristics(nil)
		if err != nil {
			return rx, tx, fmt.Errorf("discover characteristics: %w", err)
		}
		for _, chr := range chars {
			switch chr.UUID() {
			case wantRX:
				rx = chr
			case wantTX:
				tx = chr
			}
		}
	}
	if rx.UUID() != wantRX || tx.UUID() != wantTX {
		return rx, tx, errors.New("badge service is missing its RX/TX characteristics")
	}
	return rx, tx, nil
}

// bleTransport 把一条 GATT 连接包装成字节流：Read 收通知，Write 写特征。
//
// 长度上两边都不对称，这里各自交代：出方向按 bleWriteChunk 切片（CoreBluetooth 的
// 上限），入方向的每一段通知可能是半条协议行——所以这里只交字节，切行是 link.Server
// 的事（它本来就按换行切）。
type bleTransport struct {
	device bluetooth.Device
	rx     bluetooth.DeviceCharacteristic

	// alive 回答「对端还在吗」。默认直接问 CoreBluetooth（peripheral 的 state 是它
	// 自己维护的事实，不是我们猜的阈值）；测试里换成一个假的——这条判据是这一层
	// 唯一能在主机上验的部分。
	alive func() bool

	mu       sync.Mutex
	pending  []byte      // 通知收进来、还没交给 Read 的字节
	incoming chan []byte // 通知回调 → Read
	closed   chan struct{}
	once     sync.Once
}

func newBLETransport(device bluetooth.Device, rx bluetooth.DeviceCharacteristic) *bleTransport {
	return &bleTransport{
		device:   device,
		rx:       rx,
		incoming: make(chan []byte, 64),
		closed:   make(chan struct{}),
		alive: func() bool {
			connected, err := device.Connected()
			return err == nil && connected
		},
	}
}

// subscribe 开始收设备发来的通知。回调跑在 CoreBluetooth 自己的线程上，只能做
// 入队——这也是这里唯一的动作。
func (t *bleTransport) subscribe(tx bluetooth.DeviceCharacteristic) error {
	return tx.EnableNotifications(func(buf []byte) {
		// 回调的缓冲可能随下一次回调被复用，所以必须拷一份再交出去。
		chunk := append([]byte(nil), buf...)
		select {
		case t.incoming <- chunk:
		default:
			// 队列满了说明对端发得比我们读得快。丢一段而不是阻塞回调：阻塞住
			// CoreBluetooth 的线程会让整条链路一起卡死。上层的 sync 心跳会把
			// 状态补齐。
		}
	})
}

// waitSecure 触发加密并等它完成。
//
// 写法是写一个裸换行：设备只认 @@ 开头的协议行，所以它在设备那边是一个被丢掉的空行，
// 唯一的用途是把「这次写需要认证」这个要求交给 macOS。不加密时这次写会失败
// （insufficient encryption），而第一次配对时它会一直挂着——用户在电脑上输码的那
// 段时间就花在这里。
//
// ⚠️ 判据不能是「一次写成功了没」。真机上实测到的是：库自己的写超时**短于**读码敲码
// 那段时间，于是写先返回超时，而配对其实还在 CoreBluetooth 那边顺利进行（设备侧日志
// 显示 13 秒后加密成功）。所以这里循环重试，只有整体超时（blePairTimeout）才算失败。
func (t *bleTransport) waitSecure(ctx context.Context) error {
	deadline := time.Now().Add(blePairTimeout)

	for {
		done := make(chan error, 1)

		go func() {
			_, err := t.rx.Write([]byte("\n"))
			done <- err
		}()

		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			// 失败就重试。明文阶段的 "Encryption is insufficient" 同样要重试：
			// macOS 常常是在被拒一次之后才真正开始配对。
		case <-ctx.Done():
			return ctx.Err()
		}

		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the link to become encrypted")
		}
		select {
		case <-time.After(blePairRetry):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *bleTransport) Read(p []byte) (int, error) {
	liveness := time.NewTicker(bleLivenessEvery)
	defer liveness.Stop()

	for {
		t.mu.Lock()
		if len(t.pending) > 0 {
			n := copy(p, t.pending)
			t.pending = t.pending[n:]
			t.mu.Unlock()
			return n, nil
		}
		t.mu.Unlock()

		select {
		case chunk := <-t.incoming:
			t.mu.Lock()
			t.pending = append(t.pending, chunk...)
			t.mu.Unlock()
		case <-t.closed:
			return 0, io.EOF
		case <-liveness.C:
			// 对端消失时这里是唯一的出口：CoreBluetooth 会自己把连接收掉，而
			// 库不通知我们，incoming 也从此再不会有东西。
			if !t.alive() {
				_ = t.Close()
				return 0, errLinkGone
			}
		}
	}
}

func (t *bleTransport) Write(p []byte) (int, error) {
	// 已经不在的链路就别往里写了：without-response 的写在发不出去时会一次轮询
	// 10 秒，而它轮询的正是同一件事——先问一句，把一次卡住主循环的写变成立刻返回。
	if !t.alive() {
		_ = t.Close()
		return 0, errLinkGone
	}

	written := 0

	for _, chunk := range bleChunks(p) {
		count, err := t.rx.WriteWithoutResponse(chunk)
		if err != nil {
			return written, err
		}
		written += count
		// 给发送队列留出排空的时间：连着灌满会拿到 "timed out waiting for buffer
		// space"，而 without-response 下没有别的地方能提前发现它。
		time.Sleep(bleWritePace)
	}
	return written, nil
}

// bleChunks 把一条消息切成 CoreBluetooth 一次写得下的片。
//
// 它值得是个能被测到的独立函数，因为那个上限带来的失败是**静默**的：一次超长写
// 直接返回错误、整条消息丢掉，而小消息永远暴露不出问题（实测 854 字节全丢，而
// 131 字节的一直好好的）。切完之后的片是原文的切片，不拷贝。
func bleChunks(p []byte) [][]byte {
	size := bleWriteChunk
	out := make([][]byte, 0, (len(p)+size-1)/size)

	for off := 0; off < len(p); off += size {
		end := off + size
		if end > len(p) {
			end = len(p)
		}
		out = append(out, p[off:end])
	}
	return out
}

func (t *bleTransport) Close() error {
	t.once.Do(func() {
		close(t.closed)
		disconnect(t.device)
	})
	return nil
}

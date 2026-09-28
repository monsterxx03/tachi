package link

import (
	"context"
	"fmt"
	"strings"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// esprVendorID 是 Espressif 的 USB 厂商号：这块开发板的 USB-Serial-JTAG 报的就是
// 它。用它自动认设备是有必要的——端口名（/dev/cu.usbmodem2101 之类）随插拔漂移，
// 让用户记住它是没有道理的。
const esprVendorID = "303a"

// SerialListener 按需打开一个串口。串口是独占资源，所以一次只有一条链路：Accept
// 就是「打开」，前一条没断时打开会失败，交给调用方退避重试。
type SerialListener struct {
	port string
	baud int
}

// NewSerialListener 构造一个串口监听。port 为空时每次 Accept 都重新按 VID 找一遍
// ——设备重新插上会换端口名，缓存它就等于把「第一次插的那个口」写死。
func NewSerialListener(port string, baud int) *SerialListener {
	if baud <= 0 {
		baud = 115200
	}
	return &SerialListener{port: port, baud: baud}
}

func (l *SerialListener) Accept(ctx context.Context) (Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	port := l.port
	if port == "" {
		found, err := FindSerialPort()
		if err != nil {
			return nil, err
		}
		port = found
	}
	// 波特率对 USB-Serial-JTAG 没有意义（它是 USB CDC），但真实 UART 上要点，
	// 而这条链路以后可能接到别的板上。
	//
	// ⚠️ 别去动 InitialStatusBits。看上去"打开串口会把 ESP32 复位"很有道理
	// （esptool 确实靠 DTR/RTS 进下载模式），但实测是**反的**：默认的
	// (DTR=1, RTS=1) 打开时设备安然无恙，而设成 (0, 0) 反倒落进芯片的复位组合、
	// 每次都把设备重启一遍。连续开关五次的对照实验里，默认是 0/5 次复位，
	// 设 false 是 5/5。这条路径上唯一需要处理的是「重连太急」（见 badge.go 的
	// badgeFlapWindow）。
	return serial.Open(port, &serial.Mode{BaudRate: l.baud})
}

// Close 什么都不用做：链路由 Transport 自己关闭，下一次 Accept 重新打开。
func (l *SerialListener) Close() error { return nil }

// FindSerialPort 返回一个 Espressif 设备的串口名。
//
// 走详细的端口列表（而不是名字前缀）是为了看 USB 厂商号：端口名在 macOS 上是
// usbmodem + 序列号，在 Linux 上是 ttyACM，靠名字认设备只是碰巧今天成立。
func FindSerialPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", err
	}
	for _, p := range ports {
		if p.IsUSB && strings.Contains(strings.ToLower(p.VID), esprVendorID) {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("no Espressif USB serial device found (looked at %d ports)", len(ports))
}

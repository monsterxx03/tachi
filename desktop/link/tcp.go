package link

import (
	"context"
	"net"
)

// TCPListener 监听一个本地端口，等设备连上来。
//
// 存在的理由有两个：一是设备以后走 Wi-Fi 时就是这条路；二是现在就能在没有设备、
// 不用烧固件的情况下把协议跑通——一个脚本冒充设备连过来即可。
type TCPListener struct {
	ln net.Listener
}

// ListenTCP 监听 addr（例如 "127.0.0.1:8765"）。绑回环地址是默认的安全姿态：
// 这条链路上的一个回答就是在批准一条要执行的命令。
func ListenTCP(addr string) (*TCPListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &TCPListener{ln: ln}, nil
}

func (l *TCPListener) Accept(ctx context.Context) (Transport, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	// Accept 不能被打断，所以它待在自己的 goroutine 里，由 ctx 决定谁先返回。
	// 取消时 Close 会让它立刻带着错误退出，不留下悬挂的 goroutine。
	done := make(chan result, 1)
	go func() {
		conn, err := l.ln.Accept()
		done <- result{conn, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.conn, r.err
	}
}

func (l *TCPListener) Close() error { return l.ln.Close() }

func (l *TCPListener) Addr() net.Addr { return l.ln.Addr() }

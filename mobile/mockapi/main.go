// mockapi 是给手机端单独开发用的假服务端。用法：
//
//	go run ./mobile/mockapi -token dev-token
//
// 然后手机端配对时填这台机器的局域网地址和这个令牌。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/monsterxx03/tachi/pkg/logger"
)

func main() {
	token := flag.String("token", "dev-token", "配对令牌（手机端填这个）")
	listen := flag.String("listen", "0.0.0.0:8787", "监听地址")
	flag.Parse()

	srv := New(*token)
	log := logger.New("mockapi")
	log.Info(context.Background(), "mockapi 起来了", "listen", *listen)

	// 把局域网地址打出来，省得再去查一遍
	if ip := lanIP(); ip != "" {
		_, port, _ := net.SplitHostPort(*listen)
		fmt.Printf("\n手机端配对用：\n  地址  http://%s:%s\n  令牌  %s\n\n", ip, port, *token)
	}

	if err := http.ListenAndServe(*listen, srv.Handler()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "mockapi:", err)
		os.Exit(1)
	}
}

// lanIP 找一个非 loopback 的 IPv4
func lanIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

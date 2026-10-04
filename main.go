// CHPilot：基于 CH340 USB 串口的继电器控制服务。
// 单二进制 + 内嵌 Web 页面，跨平台（Windows/Linux/macOS）。
// 用法：chpilot [-addr 127.0.0.1:8080]
package main

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"chpilot/internal/serialport"
	"chpilot/internal/server"
)

//go:embed web
var staticFS embed.FS

// version 版本号，构建时通过 -ldflags "-X main.version=vX.Y.Z" 注入
var version = "dev"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP 监听地址（如 :8080 允许局域网访问）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("CHPilot " + version)
		return
	}
	server.Version = version

	mgr := serialport.NewManager()
	hub := server.NewHub()
	srv := server.New(mgr, hub, staticFS)

	// 退出时同步关闭串口，避免 device busy；3s 兜底防底层驱动卡死
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("收到 %v，正在释放串口...", sig)
		done := make(chan struct{})
		go func() {
			mgr.Shutdown()
			close(done)
		}()
		select {
		case <-done:
			log.Printf("串口已释放，退出")
		case <-time.After(3 * time.Second):
			log.Printf("关闭超时，强制退出（操作系统会回收串口句柄）")
		}
		os.Exit(0)
	}()

	log.Printf("CHPilot %s 启动: http://%s", version, *addr)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		mgr.Shutdown()
		fmt.Fprintln(os.Stderr, "HTTP 服务退出:", err)
		os.Exit(1)
	}
}

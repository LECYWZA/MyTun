// Command mytun is a single-binary, single-port multi-tunnel proxy console.
//
// MyTun keeps only two ideas from Bi-Tunnel: one TLS port that multiplexes an
// unlimited number of tunnels, and a local mixed HTTP/SOCKS5 proxy whose egress
// is either this machine ("local") or the far side of a tunnel ("remote").
// Proxy pools, chains, routing rules and the DHCP router are intentionally gone.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/LECYWZA/mytun/internal/logging"
	"github.com/LECYWZA/mytun/internal/runtime"
	"github.com/LECYWZA/mytun/internal/store"
	"github.com/LECYWZA/mytun/internal/sysproxy"
	"github.com/LECYWZA/mytun/internal/web"
)

// Version is injected at build time with -ldflags "-X main.Version=...".
var Version = "0.1.0"

func main() {
	dataDir := defaultDataDir()
	webPort := 8899
	webHost := "0.0.0.0"
	autoStart := true
	generateOnly := false

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			usage()
			return
		case "--version":
			fmt.Println("MyTun", Version)
			return
		case "--data":
			if i+1 < len(args) {
				dataDir = args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &webPort)
				i++
			}
		case "--host":
			if i+1 < len(args) {
				webHost = args[i+1]
				i++
			}
		case "--no-autostart":
			autoStart = false
		case "--init-only":
			generateOnly = true
		default:
			if len(args[i]) > 0 && args[i][0] != '-' {
				fmt.Sscanf(args[i], "%d", &webPort)
			}
		}
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[mytun] ")

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	os.Setenv("MYTUN_HOME", dataDir)

	dbPath := filepath.Join(dataDir, "mytun.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// First run: seed a server row with a generated password so the console is
	// immediately usable.
	if err := st.SeedDefaults(randomPassword()); err != nil {
		log.Fatalf("初始化默认配置失败: %v", err)
	}

	collector := logging.New(st, 5000)
	defer collector.Close()

	rt := runtime.New(runtime.Options{
		Store: st, Log: collector, CertDir: dataDir, DataDir: dataDir, APIPort: webPort,
		AutoStart: autoStart,
	})

	console, err := web.New(web.Options{Store: st, Runtime: rt, Log: collector, DataDir: dataDir})
	if err != nil {
		log.Fatalf("初始化控制台失败: %v", err)
	}
	addr := net.JoinHostPort(webHost, fmt.Sprint(webPort))
	if err := console.Start(addr); err != nil {
		log.Fatalf("控制台监听 %s 失败: %v", addr, err)
	}

	if generateOnly {
		log.Printf("初始化完成，数据库: %s", dbPath)
		return
	}

	if err := rt.Start(); err != nil {
		log.Printf("启动隧道组件时出现问题: %v", err)
	}

	printBanner(webHost, webPort, dataDir, dbPath, st, rt)
	checkResidue(rt)

	// Periodic status push to websocket clients keeps the UI live without
	// polling, and lets us detect a dead peer quickly.
	ticker := time.NewTicker(3 * time.Second)
	go func() {
		for range ticker.C {
			console.Broadcast("status", rt.Status())
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ticker.Stop()
	log.Println("正在退出，恢复系统状态…")
	if rt.SystemProxyEnabled() {
		if err := rt.EnableSystemProxy(false, "", 0); err != nil {
			log.Printf("恢复系统代理失败: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx
	console.Stop()
	rt.Stop()
	log.Println("已停止")
}

func usage() {
	fmt.Print(`MyTun ` + Version + `
用法: mytun [选项]
  --port <端口>      Web 控制台端口（默认 8899）
  --host <地址>      Web 控制台监听地址（默认 0.0.0.0）
  --data <目录>      数据目录（默认系统配置目录下的 MyTun）
  --no-autostart     启动时不自动拉起已启用的隧道/代理
  --init-only        仅初始化数据库后退出
  --version          显示版本
`)
}

func defaultDataDir() string {
	if v := os.Getenv("MYTUN_HOME"); v != "" {
		return v
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "MyTun")
	}
	exe, err := os.Executable()
	if err == nil {
		return filepath.Join(filepath.Dir(exe), "data")
	}
	return "./data"
}

func randomPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			b[i] = alphabet[0]
			continue
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

func printBanner(host string, port int, dataDir, dbPath string, st *store.Store, rt *runtime.Runtime) {
	displayHost := host
	if host == "0.0.0.0" || host == "" || host == "::" {
		displayHost = "127.0.0.1"
	}
	fmt.Println()
	fmt.Println("  ███╗   ███╗██╗   ██╗████████╗██╗   ██╗███╗   ██╗")
	fmt.Println("  ████╗ ████║╚██╗ ██╔╝╚══██╔══╝██║   ██║████╗  ██║")
	fmt.Println("  ██╔████╔██║ ╚████╔╝    ██║   ██║   ██║██╔██╗ ██║")
	fmt.Println("  ██║╚██╔╝██║  ╚██╔╝     ██║   ██║   ██║██║╚██╗██║")
	fmt.Println("  ██║ ╚═╝ ██║   ██║      ██║   ╚██████╔╝██║ ╚████║")
	fmt.Println("  ╚═╝     ╚═╝   ╚═╝      ╚═╝    ╚═════╝ ╚═╝  ╚═══╝")
	fmt.Println()
	fmt.Printf("  控制台:   http://%s:%d\n", displayHost, port)
	fmt.Printf("  用户名:   %s\n", st.GetSetting("web_user", "admin"))
	fmt.Printf("  数据目录: %s\n", dataDir)
	fmt.Printf("  数据库:   %s\n", dbPath)
	servers, _ := st.Servers()
	for _, sv := range servers {
		fmt.Printf("  隧道端口: %s:%d  (密码: %s)\n", sv.BindHost, sv.Port, sv.Password)
	}
	fmt.Println()
	fmt.Println("  按 Ctrl+C 退出（会恢复系统代理等全局状态）")
	fmt.Println()
}

func checkResidue(rt *runtime.Runtime) {
	residue, detail := runtime.DetectSystemProxyResidue()
	if residue {
		log.Printf("检测到上次异常退出残留的系统代理设置: %s（可在控制台一键清理）", detail)
	}
	_ = sysproxy.Name()
}

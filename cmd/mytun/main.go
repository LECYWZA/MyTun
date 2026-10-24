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
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/LECYWZA/mytun/internal/logging"
	"github.com/LECYWZA/mytun/internal/runtime"
	"github.com/LECYWZA/mytun/internal/store"
	"github.com/LECYWZA/mytun/internal/tunnel"
	"github.com/LECYWZA/mytun/internal/sysproxy"
	"github.com/LECYWZA/mytun/internal/web"
)

// Version is injected at build time with -ldflags "-X main.Version=...".
var Version = "0.1.0"

func main() {
	dataDir := defaultDataDir()
	webPort := 8899
	// Loopback by default: the console holds the tunnel passwords. Pass
	// --host 0.0.0.0 explicitly to expose it on the LAN.
	webHost := "127.0.0.1"
	autoStart := true
	generateOnly := false
	useTLS := false
	hideConsole := false
	dnsServer := ""

	// Watchdog mode runs before any normal initialisation.
	if healthURL, dataDir, ok := sysproxy.IsWatchdogArgs(os.Args[1:]); ok {
		sysproxy.RunWatchdog(healthURL, dataDir)
		return
	}

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
		case "--dns":
			if i+1 < len(args) {
				dnsServer = args[i+1]
				i++
			}
		case "--tls":
			useTLS = true
		case "--hide":
			hideConsole = true
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

	applyDNS(dnsServer)

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[mytun] ")

	if hideConsole {
		sysproxy.HideConsole()
	}
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
	if useTLS {
		certPair, err := tunnel.LoadOrCreateCert(filepath.Join(dataDir, "console-tls"), "mytun-console")
		if err != nil {
			log.Fatalf("生成控制台证书失败: %v", err)
		}
		if err := console.StartTLS(addr, certPair.Cert); err != nil {
			log.Fatalf("控制台监听 %s(TLS) 失败: %v", addr, err)
		}
	} else if err := console.Start(addr); err != nil {
		log.Fatalf("控制台监听 %s 失败: %v", addr, err)
	}

	if generateOnly {
		log.Printf("初始化完成，数据库: %s", dbPath)
		return
	}

	// MyTun no longer manages the OS proxy. Older versions did, so if one is
	// still pinned to a dead port, restore the original settings once here.
	if sysproxy.HasBackup() {
		log.Println("检测到旧版本遗留的系统代理设置，正在还原…")
		if err := sysproxy.Disable(); err != nil {
			log.Printf("还原系统代理失败: %v", err)
		}
	}

	if err := rt.Start(); err != nil {
		log.Printf("启动隧道组件时出现问题: %v", err)
	}

	printBanner(webHost, webPort, dataDir, dbPath, st, rt, useTLS)

	// Periodic status push to websocket clients keeps the UI live without
	// polling, and lets us detect a dead peer quickly.
	ticker := time.NewTicker(3 * time.Second)
	go func() {
		n := 0
		for range ticker.C {
			console.Broadcast("status", rt.Status())
			n++
			// Housekeeping every ~20 minutes keeps the audit table bounded on a
			// long-running box (this is one of the ways "the console gets slow
			// after a few days" happens).
			if n%(20*60/3) == 0 {
				if err := st.PruneAudit(20000); err != nil {
					log.Printf("审计日志清理失败: %v", err)
				}
				goruntime.GC()
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ticker.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx
	console.Stop()
	rt.Stop()
	log.Println("已停止")
}

// applyDNS points Go's resolver at an explicit server. Android has no usable
// /etc/resolv.conf, so the app passes the live network DNS servers in.
func applyDNS(server string) {
	if server == "" {
		return
	}
	addr := server
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "53")
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "udp", addr)
		},
	}
	log.Printf("DNS 解析强制走 %s", addr)
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func usage() {
	fmt.Print(`MyTun ` + Version + `
用法: mytun [选项]
  --port <端口>      Web 控制台端口（默认 8899）
  --host <地址>      Web 控制台监听地址（默认 127.0.0.1；局域网访问用 0.0.0.0）
  --tls              控制台启用 HTTPS（自签证书，自动生成在数据目录）
  --data <目录>      数据目录（默认系统配置目录下的 MyTun）
  --hide             隐藏控制台窗口（Windows 开机自启时用，避免留黑窗口）
  --no-autostart     启动时不自动拉起已启用的隧道/代理
  --init-only        仅初始化数据库后退出
  --dns <ip>         指定 DNS 服务器（Android 等无 resolv.conf 的环境）
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

func printBanner(host string, port int, dataDir, dbPath string, st *store.Store, rt *runtime.Runtime, useTLS bool) {
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
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	fmt.Printf("  控制台:   %s://%s:%d\n", scheme, displayHost, port)
	fmt.Printf("  用户名:   %s\n", st.GetSetting("web_user", "admin"))
	fmt.Printf("  数据目录: %s\n", dataDir)
	fmt.Printf("  数据库:   %s\n", dbPath)
	servers, _ := st.Servers()
	for _, sv := range servers {
		fmt.Printf("  隧道端口: %s:%d  (密码: %s)\n", sv.BindHost, sv.Port, sv.Password)
	}
	fmt.Println()
	fmt.Println("  按 Ctrl+C 退出")
	fmt.Println()
}


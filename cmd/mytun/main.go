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
	"strconv"
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
	// Track whether the operator passed the flag explicitly: only then does the
	// command line override the address saved from the console UI.
	hostFlagSet, portFlagSet := false, false
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
				portFlagSet = true
				i++
			}
		case "--host":
			if i+1 < len(args) {
				webHost = args[i+1]
				hostFlagSet = true
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

	// A click in the Windows console window would otherwise freeze the whole
	// program: QuickEdit mark mode blocks writes to stdout until the selection
	// is cleared. Disable it unconditionally, not just when the window is
	// hidden -- the visible window is exactly where users click.
	if switched := sysproxy.DisableQuickEdit(); switched {
		log.Printf("已关闭控制台快速编辑（点击窗口不会再卡住程序）")
	}
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
		// A corrupt database used to abort start-up entirely. Move it aside so
		// the service still comes up (config and audit history are lost, but
		// nothing is destroyed) and say loudly where the old file went.
		aside := fmt.Sprintf("%s.corrupt-%s", dbPath, time.Now().Format("20060102-150405"))
		if mvErr := os.Rename(dbPath, aside); mvErr == nil {
			log.Printf("数据库无法打开（%v），已备份为 %s，将新建空库启动", err, aside)
			st, err = store.Open(dbPath)
		}
		if err != nil {
			log.Fatalf("打开数据库失败: %v", err)
		}
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
	// A saved console address wins over the built-in default, so the value set in
	// the UI survives a restart however the program was launched.
	if !hostFlagSet {
		if v := st.GetSetting("web_host", ""); v != "" {
			webHost = v
		}
	}
	if !portFlagSet {
		if v := st.GetSetting("web_port", ""); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
				webPort = n
			}
		}
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
			// nil payload: the web layer builds the canonical status object once per push
			console.Broadcast("status", nil)
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

// isLoopbackBind reports whether addr only accepts local connections.
func isLoopbackBind(host string) bool {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return host == "" // empty means all interfaces in net.Listen
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host == "localhost"
	}
	return ip.IsLoopback()
}

// localIPv4s lists real LAN addresses. Container/bridge/VPN interfaces are
// skipped: on a host with Docker installed they otherwise flood the banner with
// a dozen unusable 172.17/172.18 style addresses.
func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(ifc.Name)
		skip := false
		for _, prefix := range []string{"docker", "veth", "br-", "virbr", "vmnet", "tun", "tap", "utun", "zt", "wg"} {
			if strings.HasPrefix(name, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipn.IP.To4()
			if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, v4.String())
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
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
	// Show what is actually bound, plus a usable URL for every reachable address.
	// Printing 127.0.0.1 while listening on 0.0.0.0 used to make the console look
	// loopback-only.
	bindDesc := host
	if bindDesc == "" {
		bindDesc = "0.0.0.0"
	}
	fmt.Printf("  监听地址: %s（%s）\n", bindDesc, map[bool]string{true: "仅本机可访问", false: "局域网/公网可访问"}[isLoopbackBind(host)])
	fmt.Printf("  本机访问: %s://127.0.0.1:%d\n", scheme, port)
	if !isLoopbackBind(host) {
		for _, ip := range localIPv4s() {
			fmt.Printf("  局域网:   %s://%s:%d\n", scheme, ip, port)
		}
	}
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


# MyTun · 单端口多隧道 + 混合代理

一条 TLS 长连接承载全部端口映射与代理，服务端只需开放**一个端口**。
Go 单文件静态编译，无运行时依赖，Windows / Linux / Android 三端可用，配置与控制台为中文界面。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20Android-blue)

> 基于 [Bi-Tunnel](https://github.com/LECYWZA/Bi-Tunnel) `dev-0910` 分支重写。
> **隧道传输格式与原版完全一致**（同样的 TLS + AES-256-GCM 帧协议、同样的密钥派生），
> 已用原版代码双向互通实测：原版客户端能连本服务端、本客户端能连原版服务端。

---

## 目录

- [快速开始](#快速开始)
- [部署](#部署)
- [使用](#使用)
- [命令行参数](#命令行参数)
- [构建](#构建)
- [常见问题](#常见问题)
- [安全说明](#安全说明)
- [附录：协议与实测数据](#附录协议与实测数据)

---

## 快速开始

```bash
# 1. 下载对应平台的可执行文件（见 Releases），直接运行
./mytun --data ./data --host 0.0.0.0 --port 8899

# 2. 浏览器打开控制台
#    http://<本机IP>:8899
#    默认账号 admin  默认密码 ewq@123456（登录后可在「控制台账号」里改）

# 3. 控制台里已有默认服务端（端口 33891），把密码复制给客户端即可对接
```

从本机 `127.0.0.1` 访问控制台**不需要登录**；从局域网或公网访问需要登录。

---

## 部署

### Linux（systemd）

```bash
sudo mkdir -p /opt/mytun /var/lib/mytun
sudo cp mytun-linux-amd64 /opt/mytun/mytun && sudo chmod +x /opt/mytun/mytun
sudo useradd -r -s /usr/sbin/nologin mytun || true
sudo chown -R mytun:mytun /var/lib/mytun

sudo cp deploy/mytun.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mytun
```

- 数据目录 `/var/lib/mytun` 保存数据库、证书与隧道配置，**备份它就等于备份全部配置**
- 默认只监听 `127.0.0.1`；需要局域网访问时把 `--host` 改成 `0.0.0.0`
- 开机自启由 systemd 负责

### Docker

```bash
docker build -t mytun:latest .
docker run -d --name mytun --network host -v /var/lib/mytun:/data mytun:latest
```

**必须用 `--network host`**：端口映射与代理端口是运行时动态创建的，桥接网络下宿主机访问不到这些端口。

### Windows

```powershell
C:\mytun\mytun.exe --data C:\mytun\data --host 0.0.0.0 --port 8899 --hide
```

开机自启推荐用计划任务（不需要登录即可运行）：

```
schtasks /create /tn MyTun /tr "C:\mytun\mytun.exe --data C:\mytun\data --host 0.0.0.0 --port 8899 --hide" /sc onstart /ru SYSTEM
```

也可以把这条命令做成快捷方式放进 `shell:startup`——**必须带参数**，否则会用默认数据目录
`%AppData%\MyTun` 并只监听 127.0.0.1。

`--hide` 隐藏控制台黑窗口。防火墙放行一次：

```
netsh advfirewall firewall add rule name="MyTun" dir=in action=allow program="C:\mytun\mytun.exe"
```

### Android

安装 `mytun-android.apk`（内置 arm64-v8a / armeabi-v7a / x86_64 三个架构）。

- 首次启动会把内置核心作为前台服务拉起，通知栏常驻显示**实时连接状态**
- 持有 `PARTIAL_WAKE_LOCK`，熄屏不掉线；建议加入系统「电池优化白名单」
- **不占用系统 VPN 槽位**，可与其他 VPN 共存
- Android 上不接管系统代理，避免与专门的代理软件冲突

---

## 使用

### 1. 建立隧道

**服务端（被连接方）**：控制台 → 隧道配置 → 填监听端口和密码，开启后等待客户端接入。
**客户端（主动连接）**：填对端地址、端口、密码，开启后自动连接并在断线后重连。

两端密码必须一致——密码同时用于派生隧道加密密钥。

### 2. 端口映射

控制台 → 端口映射，分两张卡片：

| 卡片 | 用途 |
|---|---|
| 服务端转发 | 本机监听端口 → 流量经隧道送到**对端客户端**侧的目标地址（把内网服务发布出去） |
| 客户端转发 | 本机监听端口 → 流量经**上游隧道**送出到目标地址；可指定走哪一条上游隧道 |
| **本机转发** | 不经任何隧道，本机直接转发到目标，**支持 TCP 与 UDP**（类似 nginx 的 `stream{}`） |

同一条隧道可挂任意多条映射，服务端始终只开一个端口。本机转发适合「同机或同网段的端口中转」，
比如把局域网某台设备的 TCP/UDP 端口换一个地址暴露出来。

### 3. 混合代理

一个端口同时支持 HTTP 与 SOCKS5（自动识别），出口可选：

- **本机直连**：由本机直接出网
- **经隧道**：由对端客户端出网（可指定客户端）

**认证支持多个账号**：在「认证账号」里可以添加任意多组用户名/密码（账号A、账号B、账号C……），
任一账号都能连，删除某个账号即立刻失效。也支持按来源 IP 的 allow/deny 访问控制（CIDR）。

### 4. 流量公平调度

开启后自动测量实际带宽，**多条连接抢带宽时按份额公平分配**：

- 单条连接**不限速**（没有竞争就让 TCP 决定，跑满线路）
- 只有并发时才分份额：下载型压到自己的份额，浏览/聊天类保留保底带宽
- 不需要填任何数字；要硬限速就把「容量模式」改成「按上限限速」再填 Mbps

实测：两条大下载打满时，同时进行的交互请求延迟 p50 7~11ms，不会被大流量饿死。

### 5. 消息与文件传输

控制台 → 消息传输，可经隧道发消息、传文件（多客户端时可选择接收方）。收到的文件保存在数据目录 `files/`。

---

## 命令行参数

| 参数 | 说明 |
|---|---|
| `--data <目录>` | 数据目录（数据库、证书、收到的文件）。默认 Windows 为 `%AppData%\MyTun`，其他平台为 `~/.config/MyTun` |
| `--port <端口>` | 控制台端口，默认 8899 |
| `--host <地址>` | 控制台监听地址，默认 `127.0.0.1`（局域网访问需设为 `0.0.0.0`） |
| `--tls` | 控制台启用 HTTPS（自签证书，自动生成在数据目录） |
| `--dns <地址>` | 覆盖 DNS 服务器（Android 客户端自动注入） |
| `--hide` | 隐藏控制台窗口（Windows 开机自启用） |
| `--no-autostart` | 启动时不自动拉起配置里已启用的隧道/代理 |
| `--init-only` | 只初始化数据目录与数据库，不启动服务 |

---

## 打包与加壳

发布用 `scripts/build-release.sh`：编译四平台 → 用 UPX 压缩（Linux/Windows）→ 自动跑一次冒烟测试。

```bash
MYTUN_UPX=/path/to/upx bash scripts/build-release.sh
```

- 加壳后 Windows/Linux 二进制约 **5MB**（未加壳约 15MB）
- 冒烟测试会真正启动压缩后的 Linux 二进制并请求 `/api/health`，跑不通直接失败
- macOS 不加壳（UPX 对 Mach-O 支持需要 `--force-macos`，仍是实验特性）
- 注意：`upx` 自己会用 `UPX` 这个环境变量存默认参数，所以脚本用 `MYTUN_UPX` 指定路径

**关于加壳与杀毒软件**：UPX 是通用压缩壳，多数杀软认识它。但它同时会隐去字符串特征，
个别引擎对"加壳 + 未签名 + 监听端口"的组合会更敏感。如果公司策略软件仍报毒，
最稳的办法是把文件的 SHA256 交给 IT 加白，或买一张代码签名证书。

## 构建

```bash
# 前端（产物写进 internal/web/static 供 go:embed 使用，必须先构建）
cd webui && npm install && npm run build && cd ..

# 桌面平台（纯静态，无 CGO）
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/mytun-linux-amd64     ./cmd/mytun
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/mytun-windows-amd64.exe ./cmd/mytun
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/mytun-darwin-arm64    ./cmd/mytun

# Android APK（需要 Android SDK + NDK + JDK 17，脚本会交叉编译三个 ABI 的核心）
bash scripts/build-apk.sh
```

---

## 常见问题

**连不上隧道 / 提示「认证失败」**
两端密码不一致是最常见原因。隧道用密码派生加密密钥，密码不同时连 AUTH 帧都解不开，报错会是
「认证失败：EOF（多半是两端隧道密码不一致）」。核对两端密码；用域名连接时确认 SNI 伪装填的是对端证书的 CN。

**局域网打不开控制台**
控制台默认只监听 `127.0.0.1`，启动时加 `--host 0.0.0.0`。

**映射建好了但访问不了**
检查三件事：映射的「目标客户端」是否在线、目标地址端口从**对端**是否可达、本机监听端口有没有被占用。

**改了密码登录不了**
控制台账号密码和隧道密码是两回事。控制台账号改完立即生效，旧登录态会失效；隧道密码改了需要两端同步。

**升级后界面没变化**
浏览器可能缓存了旧页面，强制刷新（Ctrl+Shift+R）。程序已对 `index.html` 设 `no-cache`，正常会自动更新。

**Windows 重启后没自动起来**
用计划任务 `/sc onstart`（不需要登录），而不是只往启动文件夹放快捷方式。

---

## 安全说明

- 隧道传输：TLS（自签证书，用于混淆）+ AES-256-GCM 逐帧加密，密钥由密码派生
- 控制台：默认只监听回环；局域网入口需登录，access token 1 小时、refresh token 7 天，**登出即吊销**
- 密码以加盐 SHA-256 存储；配置接口只返回掩码，聚焦输入框时才按需取真实值
- 系统代理功能已移除，程序不再改动操作系统代理设置

---

## 附录：协议与实测数据

### 隧道协议

```
帧格式: [1 字节类型][4 字节通道][4 字节长度][payload]
payload: AES-256-GCM，iv(12) ‖ 密文 ‖ tag(16)，密钥 = sha256(隧道密码)
类型: 1 DATA / 2 CREATE / 3 CLOSE / 4 AUTH / 5 AUTH_RES / 6 CREATE_ACK
扩展: 7 PING / 8 PONG / 9 HELLO（原版忽略未知类型，因此双向兼容）
```

### 实测数据（本机环回，WSL Ubuntu，32 核）

| 项目 | 结果 |
|---|---|
| 200MB 经隧道传输 | 完整传输，约 750 MB/s（直连基线 3.2 GB/s） |
| 100 并发通道 | 全部成功，p95 64ms，fd 无增长 |
| 两条大下载 + 交互请求 | 交互 p50 7-11ms，无饥饿 |
| 5 分钟压力浸泡 | 0 错误，fd/内存稳定 |
| 与原版 dev-0910 互通 | 双向认证成功，20MB 传输完整 |

控制台的每个页面、按钮、输入框都做过真实点击测试（含错误输入、越界值、并发操作），
发现并修复的缺陷记录在 [BUGLOG.md](BUGLOG.md)。

---

## 已知限制

- 聊天消息只保存在内存，重启后历史清空（传输过的文件保留在磁盘上）
- 审计日志按条数/天数轮转，高频写入有 200 条/秒限流（超出部分只进内存视图，控制台会显示丢弃计数）
- iOS 未验证（需要 macOS 构建环境）
- Android 未接管系统代理

## License

MIT

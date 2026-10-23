# WoL（网络唤醒）排查记录与自助清单

> 记录时间：2026-10-23，目标机器 `rtx`（Windows 11 build 26300，Realtek PCIe 2.5GbE，MAC `10:ff:e0:3b:a0:57`）。
> 本次由 DSH 在 OpenWrt 侧发送魔术包，机器**未能被唤醒**，用户手动开机。

## 一、已确认正常的部分（软件侧全部就绪）

| 检查项 | 结果 |
|---|---|
| 网卡高级属性「魔术封包唤醒」 | 开启 |
| 网卡高级属性「样式比对唤醒」 | 开启 |
| 网卡高级属性「关机 网络唤醒」 | 开启 |
| `Get-NetAdapterPowerManagement` WakeOnMagicPacket / WakeOnPattern | Enabled / Enabled |
| `powercfg /devicequery wake_armed` | 列表包含 Realtek PCIe 2.5GbE |
| 快速启动 `HiberbootEnabled` | 原本为 1，**已改为 0** |
| 网卡节能（`*EEE` / `EnableGreenEthernet` / `PowerSavingMode`） | 原本开启，**已全部关闭** |

## 二、实测过程

1. 关闭快速启动后，用 `rundll32 powrprof.dll,SetSuspendState 0,1,0` 让机器进入睡眠；
2. 分别从**两个不同源**发送魔术包（广播 `192.168.123.255` + 定向 `192.168.123.147`，UDP 端口 7 和 9，标准 6×FF+16×MAC 共 102 字节）：
   - OpenWrt WAN 口 `192.168.123.174`（dsh 容器内）
   - spark-4 `192.168.123.65`（**与 rtx 同一台 K2P 交换机的 LAN 口**，链路 UP）
3. 各发 5–8 轮、间隔 0.4–1 s，累计观察 60–90 s，**机器均未唤醒**。

## 三、日志证据

```
powercfg /lastwake
  唤醒历史记录计数 - 1
  唤醒历史记录 [0]
    唤醒源计数 - 0          ← 没有记录到设备唤醒源
```

事件日志（Microsoft-Windows-Kernel-Power）：

```
20:22:32 Id=42  系统正在进入睡眠状态。睡眠原因: Application API
20:22:35 Id=107 系统已从睡眠状态恢复。
```

两条关键信息：

- `唤醒源计数 = 0` → 上次唤醒**不是网卡触发的**（是电源按钮）。
- 进入睡眠后 3 秒就记录了"恢复"，说明**睡眠状态本身不稳定**（有设备/定时器在睡眠后立刻把它拉回来），
  这会让"发魔术包 → 观察是否唤醒"的测试非常容易误判：机器其实已经醒了，但网络栈还没恢复，探测窗口错过了。

## 四、结论与下一步（按优先级）

1. **BIOS 是首要嫌疑**（虽然用户未主动改过，但装机/清 CMOS/BIOS 更新都可能重置）：
   - `Advanced → APM Configuration` → **Power On By PCI-E / Resume By PCI-E Device = Enabled**
   - 同页 **ErP Ready / EuP = Disabled**（开启会切断待机供电，WoL 直接失效）
   - 若主板有 `Deep Sleep` / `Suspend to RAM Deep Sleep`，设为允许唤醒
2. **确认睡眠模型**：`powercfg /a` 若显示"待机(S0 低电量待机)"，说明是 Modern Standby（S0ix），
   这种机型对 WoL 支持差异很大，部分 Realtek 网卡在 S0ix 下不响应魔术包，只能在 S4/S5 下工作。
3. **区分"关机唤醒"和"睡眠唤醒"**：改完 BIOS 后分别测：
   - 先试 `关机（真正 S5，非快速启动）` → 发魔术包
   - 再试 `睡眠（S3）` → 发魔术包
   两者能分别定位是 S5 供电问题还是 S3 唤醒问题。

## 五、自助验证脚本（在 rtx 上以管理员 PowerShell 运行）

```powershell
# 1) 看当前是否能被唤醒（设备是否 armed）
powercfg /devicequery wake_armed

# 2) 看上次到底是谁唤醒了机器（唤醒源计数应为 1 且列出网卡）
powercfg /lastwake

# 3) 查看睡眠/唤醒事件（确认是 S3 还是 S0ix）
Get-WinEvent -FilterHashtable @{LogName='System'; ProviderName='Microsoft-Windows-Kernel-Power'} -MaxEvents 30 |
  Where-Object { $_.Id -in 42,107,109 } |
  Select-Object TimeCreated,Id,Message | Format-List

# 4) 只允许幻数据包唤醒（避免被随机网络包误唤醒，同时确保魔术包路径有效）
Set-NetAdapterPowerManagement -Name "以太网" -WakeOnMagicPacket Enabled

# 5) 手动置入睡眠做一次受控测试
#    先记下时间，然后从另一台机器发魔术包，再看第 2 步的 lastwake
rundll32.exe powrprof.dll,SetSuspendState 0,1,0
```

从**同网段的另一台 Linux 机器**发魔术包（把 MAC 换成本机）：

```bash
python3 -c "
import socket
mac='10:ff:e0:3b:a0:57'
pkt=b'\xff'*6+bytes.fromhex(mac.replace(':',''))*16
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)
s.setsockopt(socket.SOL_SOCKET,socket.SO_BROADCAST,1)
for _ in range(5):
    s.sendto(pkt,('192.168.123.255',9))
"
```

判定：

- `lastwake` 显示 `唤醒源计数 - 1` 且列出 Realtek → **包到了、网卡响应了**，问题在系统唤醒策略；
- `lastwake` 仍是 `计数 - 0` → **包没被网卡接收**，问题在 BIOS 待机供电或交换机端口节能。

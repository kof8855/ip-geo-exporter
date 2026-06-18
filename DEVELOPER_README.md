# ip-geo-exporter 开发者指南

> 本指南面向需要在 ip-geo-exporter 基础上做二次开发的开发者或 AI 模型。  
> 涵盖整体架构、数据流、核心数据结构、修改指南。

---

## 目录

1. [项目概述与架构](#1-项目概述与架构)
2. [数据流全链路详解](#2-数据流全链路详解)
3. [代码文件索引](#3-代码文件索引)
4. [核心数据结构](#4-核心数据结构)
5. [关键算法详解](#5-关键算法详解)
6. [扩展指南](#6-扩展指南)
7. [测试与调试](#7-测试与调试)

---

## 1. 项目概述与架构

### 一句话描述

**ip-geo-exporter** 是一个 Go 语言编写的 Prometheus exporter，利用 Linux nftables 的内核级计数器实现逐 IP 流量监控，并结合 GeoLite2-City 数据库做地理信息富化。

### 为什么选择 nftables 而非 pcap？

| 方案 | 原理 | 内存 | 性能 | 依赖 |
|------|------|------|------|------|
| **pcap（bandwhich）** | 每个包从内核拷贝到用户态 | 300MB+ | 高 CPU | libpcap |
| **eBPF TC** | 内核 BPF 映射聚合，增量推送到用户态 | ~30MB | 极低 | 内核 6.6+, clang, kernel-headers |
| **nftables（本方案）** | 内核直接计数，用户态定时读取 | **~30MB** | 极低 | **只需 nft 命令** |

**nftables 方案的核心竞争力：** 不需要任何内核头文件、编译工具链，纯用户态通过 `nft` CLI 操作，任意现代 Linux（内核 3.13+）都可直接运行。

### 技术栈

| 组件 | 版本要求 | 作用 |
|------|---------|------|
| Go | 1.22+ | 编译 |
| Linux kernel | 3.13+ | nftables netfilter |
| nftables userspace | 0.9+ (推荐 1.x) | `nft --json` 读取计数器 |
| GeoLite2-City.mmdb | 最新 | MaxMind GeoIP 数据库 |
| Prometheus client_golang | v1.20 | Prometheus metrics |

---

## 2. 数据流全链路详解

### 完整数据链路

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│  LAYER 1: Linux Kernel nftables (netfilter hooks)                               │
│                                                                                  │
│  每个数据包到达网络协议栈时触发 nftables hook：                                    │
│    input hook (方向: ← 包进入本机)                                              │
│      → 按协议匹配 (tcp/udp/icmp/other)                                          │
│      → update @in_{protocol}_v4/v6 { ip saddr } (添加或更新集合元素)             │
│      → 内核自动为该元素的 counter 累加 packets/bytes                             │
│      → accept (不影响包的正常处理)                                               │
│                                                                                  │
│    output hook (方向: 本机发出包 →)                                              │
│      → 同上，但使用 ip daddr (目的IP)                                            │
│      → update @out_{protocol}_v4/v6 { ip daddr }                                │
│      → 累加 counter → accept                                                    │
│                                                                                  │
│  集合属性：dynamic（自动添加新IP）, timeout 5m（无流量自动删除）, counter（每元素计数）│
│                                                                                  │
│  数据位置: 内核内存 (kmem)                                                       │
│  持久性: 元素<=5min无流量自动消失; 进程退出时自动清理整个 table                    │
└───────────────────────────┬──────────────────────────────────────────────────────┘
                            │ 每 PollInterval (默认 5s) 调用一次
                            │ nft --json list set inet ip_geo_exporter {set_name}
                            ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│  LAYER 2: ip-geo-exporter (Go 用户态进程)                                       │
│                                                                                  │
│  nftables.pollOnce():                                                           │
│    1. 遍历 16 个 set（或按配置少几个）                                           │
│       for each set → exec "nft --json list set ..." → parse JSON                │
│    2. 解析 JSON → []FlowElement{IP, Direction, Protocol, Bytes, Packets}         │
│                                                                                  │
│  tracker.Update():                                                              │
│    3. 对每个 FlowElement，计算 delta                                             │
│       deltaBytes = element.Bytes - shadow[key].lastBytes                         │
│       if key not in shadow: deltaBytes = element.Bytes (新流从当前值开始)         │
│       if element.Bytes < shadow[key].lastBytes: deltaBytes = element.Bytes (重置)│
│    4. 更新 shadow[key] = element.Bytes                                          │
│    5. 输出 []FlowDelta{Key, Bytes, Packets}                                     │
│                                                                                  │
│  geoip.LookupIP():                                                              │
│    6. 对每个有 delta 的 IP：                                                     │
│       if isPrivateIP(ip): return GeoRecord{Country:"未知", Lat:"0.0000", ...}     │
│       if cache.has(ip): return cache[ip] (LRU命中)                               │
│       else: maxminddb.Lookup(ip) → cache[ip] → return (LRU未命中)                 │
│                                                                                  │
│  metrics.AddTraffic():                                                          │
│    7. 如果 deltaBytes > 0: bytesTotal.WithLabelValues(IP,方向,国家,城市,...).Add() │
│       如果 deltaPackets > 0: packetsTotal.WithLabelValues(...).Add()              │
│                                                                                  │
│  数据位置: Go 进程堆内存                                                         │
│  持久性: 进程重启时丢失（从 nftables 当前值重新开始）                              │
└───────────────────────────┬──────────────────────────────────────────────────────┘
                            │ Prometheus 每 scrape_interval 秒来 GET /metrics
                            ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│  LAYER 3: Prometheus Server                                                     │
│                                                                                  │
│  收到 metrics 响应，解析文本格式，提取 counter 值                                │
│  存储到 TSDB（时序数据库，落盘）                                                 │
│  查询时通过 rate() = (val_end - val_start) / time_delta 计算每秒速率              │
│                                                                                  │
│  数据位置: 磁盘 (TSDB)                                                          │
│  持久性: 按保留策略（默认 15 天）                                                │
└───────────────────────────┬──────────────────────────────────────────────────────┘
                            │ PromQL 查询
                            ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│  LAYER 4: Grafana                                                               │
│                                                                                  │
│  Time series 面板: 查询 rate(ip_traffic_bytes_total[5m]) 画曲线                   │
│  Table 面板: 查询 sum by(...)(rate(...)) 展示当前瞬时值                          │
│  Worldmap 面板: 查询 sum by(lat,lng) 在地图上展示                                │
└──────────────────────────────────────────────────────────────────────────────────┘
```

### v0.1.3: 纯被动 Poll（Pure Passive Mode）

**从 v0.1.3 开始**，exporter **完全去掉了后台定时器**。没有任何周期性 poll 在后台运行。

```go
// 唯一的数据采集入口：Prometheus 请求 /metrics 时
mux.Handle(cfg.MetricsPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    pollOnce(context.Background(), mgr, trk, met, geo, ifaceName, activeLabels, &activeLabelsMu)
    promhttp.Handler().ServeHTTP(w, r)
}))
```

**没有定时器、没有后台线程、没有 `sync.Mutex` 竞争。** 有请求才干活，没请求零消耗。

**效果：**

| 场景 | v0.1.0（定时 poll 每5s） | v0.1.3（纯被动） |
|------|------------------------|-----------------|
| 流量产生 → Prometheus 记录 | 最多 ~10s（poll 5s + scrape 5s） | **~5s**（仅等 scrape 周期） |
| 流量结束 → Prometheus 最后记录 | 最多 ~10s | **~12s**（含 TCP 尾巴） |
| Stale IP 残留 | **永远** | 5分钟自动清理 |
| 空闲模式 CPU | 每5s 16 次 nft 命令 → ~2% CPU | **~0%** |
| 代码复杂度 | 需要 timer + mutex + select | **无后台状态** |

**并发安全：**

- `pollOnce` 内部调用的 `tracker.Update()` 已有 `sync.Mutex`
- Prometheus 的 `CounterVec.Add()` 是线程安全的
- `nftables.Manager.Poll()` 执行独立子进程，无共享状态
- **不需要外部的 `pollMu`**——因为只有 /metrics handler 一个调用入口

**副作用：**

- `/metrics` 响应时间 = `~150ms(poll) + HTTP延迟`，在 Prometheus 10s timeout 范围内完全可接受
- 如果 Prometheus 长时间不 scrape（挂了），exporter 的数据不会更新（但 nftables 内核计数不会丢，恢复后一次补全）

### 关键时序（纯被动模式）

```
t=0s     t=3s     t=6s     t=9s     t=12s    t=15s
│        │        │        │        │        │
Prometheus scrape ──→ scrape ──→ scrape ──→ scrape
         │        │        │        │
         ▼        ▼        ▼        ▼
    /metrics handler: pollOnce()  / 返回最新数据
         │        │        │        │
Grafana  刷新     刷新     刷新     刷新
```

- **exporter poll** 默认 5s，可通过 `--poll-interval` 调整
- **Prometheus scrape** 由 Prometheus 配置决定（建议 ≤5s）
- **Grafana refresh** 由仪表盘配置决定（建议 5s）

---

## 3. 代码文件索引

```
ip-geo-exporter/
├── main.go                     ← 入口/编排层
│   作用: CLI参数→nftables setup→启动HTTP→poll循环→信号处理
│   关键函数:
│     main()         — 初始化各组件，启动 HTTP 和 poll 循环
│     pollOnce()     — 单次 poll：读 nft→算 delta→GeoIP→更新 metrics
│     setupLogging() — 配置日志格式和级别
│   信号处理:
│     SIGTERM/SIGINT → Cleanup() → nft delete table → 优雅退出
│
├── internal/
│   ├── config/config.go         ← 配置层
│   │   作用: CLI 参数定义、解析、校验
│   │   关键类型:
│   │     Config       — 所有可配置项的结构体
│   │   关键方法:
│   │     Parse()      — flag.Parse() + ProtocolFilter 解析
│   │     Validate()   — 参数合法性校验
│   │     ProtocolsEnabled() — 返回实际要创建的协议列表
│   │     SetName()    — 生成 nftables set 名（如 "in_tcp_v4"）
│   │
│   ├── nftables/manager.go      ← 内核交互层
│   │   作用: 通过 nft CLI 与内核 nftables 子系统通信
│   │   关键类型:
│   │     Manager      — nftables 生命周期管理器
│   │     FlowElement  — 从内核读取的原始数据
│   │     setDef       — set 定义（name, direction, protocol, ipVer）
│   │   关键方法:
│   │     Setup()      — 生成 nft 规则文本 → nft -f - 执行
│   │     Poll()       — 遍历所有 set → nft --json list set → 解析
│   │     Teardown()   — nft delete table inet ip_geo_exporter
│   │   解析器:
│   │     parseNftSetJSON() — 解析 nft --json 输出的顶层结构
│   │     parseNftElement() — 解析单个元素（兼容3种JSON格式）
│   │   内部函数:
│   │     runNftStdin() — 通过 stdin 向 nft -f - 发送规则文本
│   │     runNft()      — 执行单条 nft 命令
│   │
│   ├── geoip/geoip.go          ← 地理信息层
│   │   作用: GeoLite2-City.mmdb 查询 + LRU 缓存
│   │   关键类型:
│   │     Lookup       — GeoIP 查询器（含缓存）
│   │     GeoRecord    — 地理信息结果
│   │   关键方法:
│   │     New()        — 打开 mmdb 文件，初始化 LRU 缓存
│   │     LookupIP()   — 查询 IP 的地理信息（缓存优先）
│   │     queryGeoIP() — 实际执行 maxminddb.Lookup()
│   │     Stats()      — 返回缓存命中/未命中统计
│   │   辅助函数:
│   │     isPrivateIP() — 判断是否为私网/保留 IP
│   │   缓存淘汰策略:
│   │     简单 FIFO: cacheLru 环形数组，nextSlot 递增，满时淘汰最旧
│   │
│   ├── tracker/tracker.go      ← 增量计算层
│   │   作用: 维护 shadow map，计算前后两次 poll 的增量
│   │   关键类型:
│   │     Tracker      — delta 计算器
│   │     FlowKey      — 流量流标识（IP + 方向 + 协议）
│   │     FlowDelta    — 增量计算结果
│   │     FlowElement  — 输入数据（与 nftables.FlowElement 同结构）
│   │   关键方法:
│   │     Update()     — 输入当前 counters，输出 deltas，更新 shadow
│   │     CleanStale() — 清理已消失的 shadow 条目
│   │     Size()       — 当前追踪的流数量
│   │
│   └── metrics/metrics.go      ← Prometheus 指标层
│       作用: 定义并注册所有 Prometheus 指标
│       关键类型:
│         Exporter      — 所有指标的容器
│         IPFlowLabels  — 指标的标签集
│       关键方法:
│         New()          — 创建并注册所有指标
│         AddTraffic()   — 根据 delta 增加 counter
│         SetTrackedIPs()
│         SetCacheStats()
│         ObservePollDuration()
│         IncPollErrors()
│         SetBuildInfo()
│
├── deploy/
│   └── ip-geo-exporter.service  ← systemd 单元文件
│
├── USER_README.md              ← 用户指南
├── Makefile                    ← 构建命令
└── go.mod / go.sum             ← Go 模块依赖
```

---

## 4. 核心数据结构

### rate() 时间窗口说明

Grafana 的 `rate(metric[$__rate_interval])` 会导致**下载结束后的"拖尾"**现象：

```
实际流量:  ████████████████████░░░░░░░░░░░░░░░░░░░░░░░░
                   下载结束        rate[1m]窗口完全滑出
                   ▼              ▼
rate[1m]看到的: ████████████████╱╲╱╲╱╲░░░░░░░░░░░░░░░░
                   瞬间下降      ~60秒后归零
```

这不是 exporter 的问题，而是 rate() 的数学特性。有以下方案：

| 方案 | PromQL | 拖尾长度 | 曲线平滑度 |
|------|--------|---------|-----------|
| 默认 ($__rate_interval) | `rate(...[$__rate_interval])` | ~60s | 平滑 |
| 短窗口 | `rate(...[15s])` | **~15s** | 略有锯齿 |
| 短窗口+归零过滤 | `rate(...[15s]) > 0` | **~15s，归零立即消失** | 略有锯齿 |

**推荐方案：** 生产环境 Grafana 面板中统一使用 `[15s]` 替代 `[$__rate_interval]`，并加 `> 0` 过滤。


### PromQL 查询大全

所有查询都以 `ip_traffic_bytes_total` 为例，替换 `direction` 即可查上行/下行。

### ─── 实时速率 ───

```promql
# 当前每秒速率（Top 10 IP），推荐 [15s] 减少拖尾，>0 过滤归零IP
topk(10, sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[15s])) > 0)

# 按国家汇总速率
sum by(country) (rate(ip_traffic_bytes_total{direction="download"}[15s]))
```

### ─── 累计总量（从exporter启动至今） ───

```promql
# 单个IP累计下载总量（GB）
ip_traffic_bytes_total{ip="8.8.8.8",direction="download"} / 1073741824

# 按IP排行总流量
topk(10, sum by(ip, country) (ip_traffic_bytes_total{direction="download"}) / 1073741824)

# 按国家汇总总流量
sum by(country) (ip_traffic_bytes_total{direction="download"}) / 1073741824
```

### ─── 增量趋势图（推荐，无拖尾） ───

`increase()` 计算一个时间窗口内的增量，Grafana 用 Time Series 面板显示为"台阶图"，**没有 rate() 的拖尾问题**。

```promql
# 5分钟增量（适合看分钟级流量波动）
increase(ip_traffic_bytes_total{direction="download"}[5m]) / 1073741824

# 30分钟增量（适合看长期趋势）
increase(ip_traffic_bytes_total{direction="download"}[30m]) / 1073741824

# 1小时增量（适合日报/看板）
increase(ip_traffic_bytes_total{direction="download"}[1h]) / 1073741824
```

**Grafana 设置建议：** Time Series 面板 → 单位 `Data → Gibibytes`。

### ─── 过滤特定服务器 ───

如果 Prometheus 配置了多个 job 或多个 target：

```promql
# 只查看某台服务器的流量
increase(ip_traffic_bytes_total{direction="download",instance="你的服务器IP:9100"}[5m]) / 1073741824

# 按 instance 汇总各服务器总流量
sum by(instance) (increase(ip_traffic_bytes_total{direction="download"}[5m])) / 1073741824
```

### ─── Worldmap 全球流量分布 ───

```promql
# 按国家+坐标汇总速率（供 Grafana Worldmap Panel 使用）
sum by(country, latitude, longitude) (rate(ip_traffic_bytes_total{direction="download"}[5m]))
```

### ─── 诊断与调试 ───

```promql
# 原始 Counter 值（不带 rate，验证数据是否正确）
ip_traffic_bytes_total{ip="8.8.8.8",direction="download"}

# 平均包大小
rate(ip_traffic_bytes_total[5m]) / rate(ip_traffic_packets_total[5m])

# 当前追踪的活跃 IP 数
ip_geo_tracked_ips
```

### ─── `rate()` vs `increase()` 对比 ───

| 函数 | 输出 | 适合场景 | 拖尾? |
|------|------|---------|-------|
| `rate(...[$__rate_interval])` | B/s（每秒速率） | 实时曲线图 | ❌ 有，约 1 分钟 |
| `rate(...[15s])` | B/s（每秒速率） | 实时曲线图 | ⚠️ 约 15 秒 |
| `increase(...[5m])` | B（5分钟内总增量） | 台阶/柱状图 | ✅ **无拖尾** |
| `raw counter` | B（累计值） | 原始数据验证 | ✅ **无拖尾** |

**推荐：** 日常用 `increase(...[5m])` 做流量趋势图，没有任何 rate 拖尾问题。


```promql
# ✅ 推荐
topk(10, sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[15s])) > 0)
```

### 4.1 配置结构 (`internal/config/config.go`)

```go
type Config struct {
    // ─── HTTP 服务 ───
    ListenAddress   string        // ":9100"
    MetricsPath     string        // "/metrics"

    // ─── 采集周期 ───
    // 注意：此值直接影响 nft 子进程调用频率
    // 调小 → 实时性高但 CPU 开销增加（每 poll 执行 16 次 nft 命令）
    // 调大 → CPU 降低但 Grafana 曲线变平滑
    PollInterval    time.Duration // 5s

    // ─── GeoIP ───
    GeoIPDB         string        // "/usr/share/GeoIP/GeoLite2-City.mmdb"
    GeoIPLang       string        // "zh-CN"
    GeoIPCacheSize  int           // 100000

    // ─── 协议过滤 ───
    ProtocolFilter  []string      // ["tcp","udp","icmp"] 或 ["all"]
    FilterPrivate   bool          // false
    ExcludeCIDRs    []string      // nil
    IncludeSubnets  []string      // nil

    // ─── nftables 参数 ───
    SetTimeout      time.Duration // 5m
    MaxSetEntries   int           // 100000
    CleanupOnExit   bool          // true

    // ─── IPv6 ───
    EnableIPv6      bool          // true

    // ─── 日志 ───
    LogFormat       string        // "text" 或 "json"
    LogLevel        string        // "info"
}
```

### 4.2 流量数据流结构

```go
// LAYER 1: nftables 读取的原始数据
// nftables/manager.go
type FlowElement struct {
    IP        string // "101.6.15.130"
    Direction string // "upload" / "download"（in→download, out→upload）
    Protocol  string // "tcp"/"udp"/"icmp"/"other"/"all"
    Bytes     uint64 // 内核累积字节数（绝对值）
    Packets   uint64 // 内核累积包数（绝对值）
}

// LAYER 2: Delta 计算
// tracker/tracker.go
type FlowKey struct {
    IP        string // "101.6.15.130"
    Direction string // "upload" / "download"
    Protocol  string // "tcp" / "udp" / "icmp" / "other" / "all"
}

type FlowDelta struct {
    Key     FlowKey // 流标识
    Bytes   uint64  // 增量字节数（非绝对值！）
    Packets uint64  // 增量包数（非绝对值！）
}

// LAYER 2: GeoIP 结果
// geoip/geoip.go
type GeoRecord struct {
    Country      string // "中国" / "美国" / "未知"
    City         string // "北京" / "洛杉矶" / "未知"
    Subdivisions string // "浙江" / "加州" / "未知"
    Latitude     string // "39.9042" / "0.0000"
    Longitude    string // "116.4074" / "0.0000"
}

// LAYER 2: Prometheus 标签
// metrics/metrics.go
type IPFlowLabels struct {
    IP           string // "101.6.15.130"
    Direction    string // "upload" / "download"
    Country      string // "中国"
    City         string // "北京"
    Subdivisions string // "北京"
    Latitude     string // "39.9042"
    Longitude    string // "116.4074"
    Protocol     string // "tcp"
    Iface        string // "all"（v0.1.0 固定值）
}
```

### 4.3 结构体关系图

```
nft CLI output (JSON)
       │
       ▼
nftables.FlowElement{IP, Direction, Protocol, Bytes, Packets}
       │
       ▼
tracker.FlowElement{IP, Direction, Protocol, Bytes, Packets} (同结构)
       │
       ├──→ tracker.Update() → tracker.FlowDelta{Key, Bytes, Packets}
       │        │
       │        ├──→ geoip.LookupIP(IP) → GeoRecord{Country, City, Lat, Lng}
       │        │
       │        └──→ Convert to metrics.IPFlowLabels{IP, Direction, Country, ...}
       │
       └──→ metrics.AddTraffic(labels, deltaBytes, deltaPackets)
                │
                └──→ prometheus.CounterVec.WithLabelValues(...).Add(delta)
```

---

## 5. 关键算法详解

### 5.1 nftables 规则生成算法 (`Setup()`)

```go
func (m *Manager) Setup() error {
    // 1. 先清理可能残留的同名 table
    // 2. 生成 add table inet ip_geo_exporter
    //
    // 3. 遍历所有方向 × 协议 × IP版本的组合：
    //    for each direction in ["in", "out"]:
    //      for each protocol in protocols:
    //        for each ipVer in ["v4", "v6"]:
    //          → add set {name}_{dir}_{proto}_{ver} {
    //              type ipv4_addr/ipv6_addr
    //              flags dynamic,timeout
    //              timeout {SetTimeout}
    //              size {MaxSetEntries}
    //              counter
    //            }
    //
    // 4. 遍历方向 × IP版本：
    //    for each direction:
    //      for each ipVer:
    //        → add chain {dir}_{ver} {
    //            type filter hook {input/output} priority -100; policy accept;
    //          }
    //        for each protocol:
    //          → meta l4proto {tcp/udp/icmp/icmpv6}
    //             update @{set_name} { ip_saddr/daddr } accept
    //        → update @{set_name}_other { ip_saddr/daddr } accept
    //
    // 5. 全部写入 strings.Builder → nft -f - (stdin)
}
```

**重点：`ip saddr` vs `ip daddr` 的选择：**

| Chain hook | 包方向 | 远端IP位置 | set 中存的 key |
|------------|--------|-----------|----------------|
| `input` | 进入本机 | 源IP (`ip saddr`) | 远端IP |
| `output` | 离开本机 | 目的IP (`ip daddr`) | 远端IP |

### 5.2 nft JSON 解析算法 (`parseNftElement()`)

```go
func parseNftElement(raw json.RawMessage) (nftElement, error) {
    // 尝试 3 种 JSON 格式：
    //
    // 格式1 (标准):  {"elem": {"val": "1.2.3.4",
    //                          "counter": {"packets": 100, "bytes": 5000}}}
    //
    // 格式2 (旧版):  ["1.2.3.4",
    //                 {"counter": {"packets": 100, "bytes": 5000}}]
    //
    // 格式3 (兜底):  "1.2.3.4"  (无 counter，仅 IP 文本)
    //
    // 优先尝试格式1，顺序无关
}
```

JSON 结构各字段含义：

| 字段 | 类型 | 含义 | 示例 |
|------|------|------|------|
| `elem.val` | string | IP 地址 | `"101.6.15.130"` |
| `elem.counter.packets` | uint64 | 累计包数 | `834567` |
| `elem.counter.bytes` | uint64 | 累计字节数 | `1194169849` |
| `elem.expires` | string | 剩余存活时间（仅 dynamic set） | `"4m30s"` |

### 5.3 Delta 计算算法 (`Update()`)

```go
func (t *Tracker) Update(elements []FlowElement) []FlowDelta {
    // Shadow map: map[FlowKey]shadowEntry
    //
    // 对每个 element:
    //   key = {element.IP, element.Direction, element.Protocol}
    //
    //   情况A: key 不在 shadow 中（新流）
    //     delta = current          // 首次出现，从当前值开始累加
    //     shadow[key] = current
    //
    //   情况B: key 在 shadow 中（已有流）
    //     delta = current - prev   // 正常增量
    //     shadow[key] = current
    //
    //   情况C: current < prev（计数器异常）
    //     内核 set 被删除重建（cleanup-on-exit=true 后重启）
    //     或 uint64 溢出（理论不可能）
    //     delta = current          // 重新从当前值开始
    //     shadow[key] = current
    //
    //   如果 delta > 0，加入返回值
}
```

### 5.4 私网 IP 判定算法 (`isPrivateIP()`)

```go
func isPrivateIP(ip net.IP) bool {
    // 按优先级检查：
    // 1. Loopback (127.0.0.0/8, ::1)
    // 2. Link-local unicast (169.254.0.0/16, fe80::/10)
    // 3. Link-local multicast (224.0.0.0/24, ff00::/8)
    // 4. RFC1918 (10.0.0.0/8)
    // 5. RFC1918 (172.16.0.0/12)
    // 6. RFC1918 (192.168.0.0/16)
    // 7. CGNAT (100.64.0.0/10)
}
```

**注意：** 如果新增需要过滤的 IP 段（如 `198.18.0.0/15` 基准测试段），在此函数中添加。

### 5.5 GeoIP LRU 缓存算法

```go
type Lookup struct {
    cache    map[string]*GeoRecord  // 主存储
    cacheLru []string               // FIFO 队列，长度 = maxSize
    nextSlot int                    // 下一个写入位置
    maxSize  int                    // 容量上限
}

func (l *Lookup) LookupIP(ipStr string) *GeoRecord {
    // 1. 检查缓存 (RLock)
    //    if hit → 记录 hits++，返回
    //
    // 2. 检查私网IP
    //    if isPrivateIP → 返回 country="未知"
    //
    // 3. 执行 mmdb 查询 (Lock)
    //    misses++
    //    cache[ipStr] = result
    //
    // 4. FIFO 淘汰：
    //    if nextSlot < maxSize:
    //      cacheLru[nextSlot] = ipStr; nextSlot++
    //    else:
    //      oldest = cacheLru[0]
    //      delete(cache, oldest)
    //      copy cacheLru[1:] left by 1
    //      cacheLru[maxSize-1] = ipStr
}
```

**为什么用 FIFO 而不是真正的 LRU？** 简单、性能好。IP 流量呈现"热点集中"特性（少数 IP 占大部分流量），FIFO 足以保留热点。如果热点集超过缓存大小，可改用 `hashicorp/golang-lru`。

### 5.6 nftables set 命名规则

```go
// internal/config/config.go
func (c *Config) SetName(direction, protocol, ipVersion string) string {
    prefix := map[string]string{"in": "in", "out": "out"}[direction]
    if protocol == "all" || protocol == "" {
        return fmt.Sprintf("%s_%s", prefix, ipVersion)     // "in_v4"
    }
    return fmt.Sprintf("%s_%s_%s", prefix, protocol, ipVersion) // "in_tcp_v4"
}
```

### 5.7 主循环算法 (`main.go`)

```go
func main() {
    // ─── 初始化阶段 ───
    cfg := Parse()           // 1. 解析命令行参数
    Validate(cfg)            // 2. 参数校验
    MustRunNftCheck()        // 3. 确认 nft 命令可用
    mgr := NewManager(cfg)   // 4. 创建 nftables 管理器
    mgr.Setup()              // 5. 写入 nftables 规则到内核
    geo := geoip.New(...)    // 6. 加载 GeoIP
    trk := tracker.New()     // 7. 创建增量追踪器
    met := metrics.New()     // 8. 注册 Prometheus 指标

    // ─── HTTP 服务 ───
    // /metrics 使用自定义 handler: Prometheus 来取数时立即 poll，保证数据最新
    mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        pollMu.Lock()
        pollOnce(context.Background(), mgr, trk, met, geo, ifaceName)
        pollMu.Unlock()
        promhttp.Handler().ServeHTTP(w, r)
    }))
    mux.HandleFunc("/health", ok)
    go http.ListenAndServe(cfg.ListenAddress, mux)

    // ─── Poll 循环 ───
    ticker := time.NewTicker(cfg.PollInterval)
    for {
        select {
        case <-ticker.C:
            pollMu.Lock()
            pollOnce(ctx, mgr, trk, met, geo, ifaceName)
            pollMu.Unlock()
        case <-sigCh:
            cleanup(mgr)      // nft delete table
            srv.Shutdown()
            return
        }
    }
}

func pollOnce(mgr, trk, met, geo) {
    start := time.Now()

    // Step 1: 从内核读取计数器
    flows, err := mgr.Poll()          // → []FlowElement

    // Step 2: 计算增量
    elements := toTrackerElements(flows)  // 类型转换
    currentKeys := buildKeys(elements)
    deltas := trk.Update(elements)      // → []FlowDelta
    trk.CleanStale(currentKeys)

    // Step 3: GeoIP 富化 + 更新指标
    for _, d := range deltas {
        geoRec := geo.LookupIP(d.Key.IP)
        met.AddTraffic(toLabels(d.Key, geoRec), d.Bytes, d.Packets)
    }

    // Step 4: 更新内部监控
    met.SetTrackedIPs(trk.Size())
    met.SetCacheStats(geo.Stats())
    met.ObservePollDuration(time.Since(start).Seconds())
}
```

---

## 6. 扩展指南

### 6.1 新增一个 CLI 参数

**步骤：**

1. 在 `internal/config/config.go` 的 `Config` 结构体中添加字段
2. 在 `Parse()` 中添加 `flag.StringVar(...)` 或等效
3. 在 `Validate()` 中添加校验
4. 在使用该参数的地方读取 `cfg.NewField`

**示例：** 添加一个 `--show-local-ports` 参数

```go
// config.go
type Config struct {
    ShowLocalPorts bool  // 新增
}

func Parse() *Config {
    cfg := &Config{}
    flag.BoolVar(&cfg.ShowLocalPorts, "show-local-ports", false,
        "Also record local port numbers")
    // ...
}
```

### 6.2 新增一个 Prometheus 指标

**步骤：**

1. 在 `internal/metrics/metrics.go` 的 `Exporter` 中添加字段
2. 在 `New()` 中使用 `promauto.NewXxxVec` 注册
3. 添加对应的公开方法（如 `SetXxx()`）
4. 在 `main.go` 的 `pollOnce()` 中调用新方法

**示例：** 添加 nftables set 当前元素数

```go
// metrics.go
type Exporter struct {
    setEntries *prometheus.GaugeVec  // 新增
}

func New() *Exporter {
    e := &Exporter{}
    e.setEntries = promauto.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "ip_geo_set_entries",
            Help: "Current number of entries per nftables set",
        },
        []string{"set_name"},
    )
}

func (e *Exporter) SetSetEntries(name string, count int) {
    e.setEntries.WithLabelValues(name).Set(float64(count))
}
```

### 6.3 修改 nftables 规则结构

**步骤：**

1. 修改 `internal/nftables/manager.go` 中的 `Setup()` 方法
2. 如果新增 set，也在 `Poll()` 遍历列表中添加
3. 确保 `Teardown()` 能正确清理（delete table 会清理所有下级）

**常见修改场景：**

```go
// 1. 修改 set 超时时间（已通过 --set-timeout 暴露）
//    在规则生成代码中：timeout {cfg.SetTimeout}

// 2. 修改 set 大小（已通过 --max-set-entries 暴露）
//    在规则生成代码中：size {cfg.MaxSetEntries}

// 3. 新增一个 set（如按端口号拆分）
//    在 direction/protocol/ipVer 三层循环中添加新的维度
//    注意：SetName() 也需要相应修改
```

### 6.4 支持多网卡追踪

**目前限制：** `iface` 标签固定为 `"all"`。

**修改步骤：**

| 步骤 | 修改文件 | 修改内容 |
|------|---------|---------|
| 1 | `nftables/manager.go` | `Setup()` 中的规则生成逻辑：为每个网卡创建独立的 set（如 `in_tcp_v4_eth0`），规则中增加 `iif eth0` / `oif eth0` 匹配 |
| 2 | `nftables/manager.go` | `Poll()` 遍历 set 时，解析 set 名中的网卡标识 |
| 3 | `nftables/manager.go` | `FlowElement` 增加 `Iface` 字段 |
| 4 | `metrics/metrics.go` | `IPFlowLabels.Iface` 使用实际网卡名 |
| 5 | `config/config.go` | 新增 `--interfaces` 参数，指定要监听的网卡列表 |

### 6.5 替换 GeoIP 数据库（如切换到 ipip.net）

**修改步骤：**

1. 定义新的查询接口（统一 `GeoRecord` 输出）
2. 在 `internal/geoip/geoip.go` 中替换 `queryGeoIP()` 的实现
3. 保持 `LookupIP()` 的缓存逻辑不变

```go
func (l *Lookup) queryGeoIP(ip net.IP) *GeoRecord {
    // 替换这里：从 maxminddb.Lookup 改为调用 ipip.net 的 API/库
}
```

---

## 7. 测试与调试

### 单元测试

```bash
# 运行所有测试
go test ./...

# 运行特定包测试
go test ./internal/tracker/
```

### 手动测试

```bash
# 1. 编译
make build

# 2. 运行（前台）
sudo ./ip-geo-exporter --geoip-db=/usr/share/GeoIP/GeoLite2-City.mmdb

# 3. 检查指标
curl -s http://localhost:9100/metrics | head -50

# 4. 查看 nftables 规则
nft list table inet ip_geo_exporter

# 5. 查看某个 set 的计数器
nft list set inet ip_geo_exporter in_tcp_v4
```

### 性能测试

```bash
# 生成大量流量测试 10 万条 set 的性能
# 通过测试不同的 --poll-interval 和 --max-set-entries 组合
```

### 生成测试流量

```bash
# TCP 流量
curl -s https://www.baidu.com > /dev/null

# UDP 流量
nslookup google.com

# ICMP 流量
ping -c 3 8.8.8.8

# IPv6 流量
ping -c 3 -6 2400:3200::1

# 大文件下载（验证计数器精度）
curl -o /dev/null -s --max-time 30 \
  https://mirrors.tuna.tsinghua.edu.cn/ubuntu-releases/24.04/ubuntu-24.04.4-desktop-amd64.iso
```

---

## 附录：依赖关系

```
main.go
  ├── internal/config/config.go
  ├── internal/nftables/manager.go
  │     └── internal/config/config.go
  ├── internal/geoip/geoip.go
  │     └── github.com/oschwald/maxminddb-golang
  ├── internal/tracker/tracker.go
  └── internal/metrics/metrics.go
        └── github.com/prometheus/client_golang

外部依赖:
  nft CLI (系统命令) — 非 Go 依赖，通过 os/exec 调用
  GeoLite2-City.mmdb — 运行时加载
```

**架构约束：** 模块间无循环依赖。数据流向单向：config → nftables/geoip/tracker → metrics → main。

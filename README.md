# ip-geo-exporter

基于 **nftables 内核计数器 + GeoLite2-City** 的逐 IP 流量 Prometheus exporter。

**内存占用：6-30MB**（取决于活跃 IP 数量），远低于 bandwhich 二次开发版的 300MB+。  
**采集模式：纯被动**（无后台定时器，Prometheus scrape 时才执行 nftables 采集）

---

## 目录

1. [工作原理（数据流全景）](#工作原理数据流全景)
2. [nftables 规则结构详解](#nftables-规则结构详解)
3. [内部数据结构详解](#内部数据结构详解)
4. [Prometheus 指标详解](#prometheus-指标详解)
5. [命令行参数详解](#命令行参数详解)
6. [快速安装](#快速安装)
7. [Grafana 配置](#grafana-配置)
8. [构建与二次开发](#构建与二次开发)
9. [许可](#许可)

---

## 工作原理（数据流全景）

```
┌─────────────────────────────────────────────────────────────────────────┐
│  第1层: Linux 内核 nftables                                              │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  inbound (input hook, priority -100)                             │   │
│  │    meta l4proto tcp  → update @in_tcp_v4 { ip saddr }           │   │
│  │    meta l4proto udp  → update @in_udp_v4 { ip saddr }           │   │
│  │    meta l4proto icmp → update @in_icmp_v4 { ip saddr }          │   │
│  │    (no match)        → update @in_other_v4 { ip saddr }         │   │
│  │  ┌─ 每个 @set 是内核动态集合，自动学习 IP，自带 counter ─────────┐  │   │
│  │  │ set in_tcp_v4 {                                             │  │   │
│  │  │   type ipv4_addr; flags dynamic,timeout; timeout 5m; counter│  │   │
│  │  │   elements = {                                              │  │   │
│  │  │     101.6.15.130 counter packets 9999 bytes 999999          │  │   │
│  │  │   }                                                         │  │   │
│  │  │ }                                                           │  │   │
│  │  └─────────────────────────────────────────────────────────────┘  │   │
│  │                                                                     │   │
│  │  outbound (output hook, priority -100)                            │   │
│  │    同上，但用 ip daddr 追踪目的IP                                   │   │
│  │                                                                     │   │
│  │  每个数据包 → 匹配 rule → 更新 set 中的元素计数器 → accept         │   │
│  │  元素 5 分钟无流量自动超时清除（timeout 5m）                        │   │
│  │  最多 10 万个元素（size 100000），超限后 LRU 淘汰                     │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                              │ 每5秒 poll                               │
│                              ▼                                           │
│  第2层: ip-geo-exporter (Go 进程)                                       │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  1. nft --json list set → 解析 elements + counter               │   │
│  │  2. Delta = current_value - shadow_map[flowKey]                  │   │
│  │  3. GeoIP: maxminddb.Lookup(ip) → country/city/lat/lng           │   │
│  │  4. prometheus Counter.Add(delta_bytes, delta_packets)           │   │
│  │  5. 更新 shadow_map[flowKey] = current_value                     │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                              │ GET /metrics                             │
│                              ▼                                           │
│  第3层: Prometheus (TSDB)                                              │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │ 每3~5s scrape exporter 的 /metrics 端点                           │   │
│  │ 存储累积值到磁盘 TSDB（默认保留 15 天）                           │   │
│  │ rate()[5m] = (last_value - first_value) / time_delta             │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                              │ PromQL 查询                              │
│                              ▼                                           │
│  第4层: Grafana (可视化)                                               │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │ Time series 图表: 显示 rate() 随时间变化的曲线                     │   │
│  │ Table 面板: 显示当前瞬时值（或聚合后的 Max/Min/Last）              │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
```

### 核心设计理念

| 设计选择 | 原因 |
|---------|------|
| **nftables 而非 pcap** | 无包拷贝到用户态，内核直接计数，内存降低 10 倍 |
| **Pull 而非 Push** | Prometheus 标准拉模型，exporter 无状态，重启后从 nftables 当前值重新开始 |
| **Counter 而非 Gauge** | 单调递增计数器，rate() 算速率，空档期自动用 (val_delta / time_delta) 平摊 |
| **Poll 即写** | 每次 Prometheus scrape 触发 poll，不预留存，无后台线程 |
| **Counter 而非 Gauge** | 单调递增计数器，rate() 算速率，空档期自动用 (val_delta / time_delta) 平摊 |

---

## nftables 规则结构详解

### 表 (Table)

```
table inet ip_geo_exporter
```

- **family**: `inet` ── 同时处理 IPv4 和 IPv6 流量
- **name**: `ip_geo_exporter` ── 独立命名空间，不影响用户已有的 nftables 规则
- **生命周期**: `Setup()` 时创建，`Teardown()` 或进程退出时 `cleanup-on-exit` 删除
- **重复启动安全**: 启动时先 `delete table` 再 `add table`，保证幂等

### 集合 (Set) 命名规则

```
{direction}_{protocol}_{ip_version}

direction: in  | out
protocol:  tcp | udp | icmp | other | all
ip_ver:    v4  | v6
```

完整列表（16 个 set，当 `--protocol-filter=tcp,udp,icmp` 且 `--enable-ipv6=true` 时）：

| Set 名称 | 方向 | 追踪什么 | type | 说明 |
|----------|------|---------|------|------|
| `in_tcp_v4` | inbound | `ip saddr` | ipv4_addr | 远端源 IP 的 TCP 入站流量 |
| `in_udp_v4` | inbound | `ip saddr` | ipv4_addr | 远端源 IP 的 UDP 入站流量 |
| `in_icmp_v4` | inbound | `ip saddr` | ipv4_addr | 远端源 IP 的 ICMP 入站流量 |
| `in_other_v4` | inbound | `ip saddr` | ipv4_addr | 非 TCP/UDP/ICMP 的入站流量（GRE/ESP 等） |
| `out_tcp_v4` | outbound | `ip daddr` | ipv4_addr | 远端目的 IP 的 TCP 出站流量 |
| `out_udp_v4` | outbound | `ip daddr` | ipv4_addr | 远端目的 IP 的 UDP 出站流量 |
| `out_icmp_v4` | outbound | `ip daddr` | ipv4_addr | 远端目的 IP 的 ICMP 出站流量 |
| `out_other_v4` | outbound | `ip daddr` | ipv4_addr | 非 TCP/UDP/ICMP 的出站流量 |
| `in_tcp_v6` | inbound | `ip6 saddr` | ipv6_addr | IPv6 版本，同上 |
| `in_udp_v6` | inbound | `ip6 saddr` | ipv6_addr | |
| `in_icmp_v6` | inbound | `ip6 saddr` | ipv6_addr | 注意 `icmpv6` 而非 `icmp` |
| `in_other_v6` | inbound | `ip6 saddr` | ipv6_addr | |
| `out_tcp_v6` | outbound | `ip6 daddr` | ipv6_addr | |
| `out_udp_v6` | outbound | `ip6 daddr` | ipv6_addr | |
| `out_icmp_v6` | outbound | `ip6 daddr` | ipv6_addr | |
| `out_other_v6` | outbound | `ip6 daddr` | ipv6_addr | |

**当 `--protocol-filter=all` 时**（不协议拆分）：只创建 4 个 set：`in_v4`, `out_v4`, `in_v6`, `out_v6`

### 集合 (Set) 属性详解

```nftables
set in_tcp_v4 {
    type ipv4_addr           # 元素类型：IPv4 地址
    flags dynamic,timeout    # dynamic: 允许通过 update 动态添加; timeout: 元素可自动过期
    timeout 5m               # 元素无流量 5 分钟后自动删除（内核管理，不需要用户态干预）
    size 100000              # 集合最大元素数（可通 --max-set-entries 调整）
    counter                  # 每个元素附带包计数器和字节计数器（内核自动维护）
}
```

| 属性 | 含义 | 调整参数 |
|------|------|---------|
| `type` | 元素类型，`ipv4_addr`=32位IPv4, `ipv6_addr`=128位IPv6 | 固定，由--enable-ipv6决定 |
| `dynamic` | 允许 nftables 规则动态添加元素 | 固定 |
| `timeout` | 元素静默超时时间 | `--set-timeout`（默认 5m） |
| `size` | 最大元素数 | `--max-set-entries`（默认 100000） |
| `counter` | 每个元素附带 (packets, bytes) 计数器 | 固定 |

### 链 (Chain) 结构

```nftables
# 入站追踪（input hook）
chain in_v4 {
    type filter hook input priority -100; policy accept;
    meta l4proto tcp  update @in_tcp_v4  { ip saddr } accept
    meta l4proto udp  update @in_udp_v4  { ip saddr } accept
    meta l4proto icmp update @in_icmp_v4 { ip saddr } accept
                          update @in_other_v4 { ip saddr } accept
}

# 出站追踪（output hook）
chain out_v4 {
    type filter hook output priority -100; policy accept;
    meta l4proto tcp  update @out_tcp_v4  { ip daddr } accept   # 注意：出站用 daddr
    meta l4proto udp  update @out_udp_v4  { ip daddr } accept
    meta l4proto icmp update @out_icmp_v4 { ip daddr } accept
                          update @out_other_v4 { ip daddr } accept
}
```

| hook | 方向 | saddr/daddr | 对应 Prometheus `direction` |
|------|------|------------|---------------------------|
| `input` | 包进入本机 | `ip saddr`（远端源IP） | `download`（本机接收） |
| `output` | 包从本机发出 | `ip daddr`（远端目的IP） | `upload`（本机发送） |

**关键设计约束：**
- `update @set { expr }` 语句：如果 IP 不在 set 中则添加，同时在元素的 counter 上累加包/字节数
- 每个 rule 后都有 `accept`：匹配到的包立即接受，不再匹配后续规则
- TCP 包只进 `in_tcp_v4`，UDP 只进 `in_udp_v4`，互不干扰
- 最后一条 rule 无 `l4proto` 匹配：捕获所有非 TCP/UDP/ICMP 的流量（`other`）
- `policy accept`：即使没有 rule 匹配，包也被接受（不影响正常网络）

---

## 内部数据结构详解

### `internal/config/config.go` ── 配置结构

```go
type Config struct {
    // HTTP 服务
    ListenAddress   string        // 默认 ":9100"，HTTP 监听地址
    MetricsPath     string        // 默认 "/metrics"，Prometheus 端点路径

    // 采集周期（纯被动模式下无效，保留向后兼容）
    PollInterval    time.Duration // 默认 5s，纯被动模式下此参数不使用

    // GeoIP 数据库
    GeoIPDB         string        // 默认 "/usr/share/GeoIP/GeoLite2-City.mmdb"
    GeoIPLang       string        // 默认 "zh-CN"，国家/城市名的语言代码
    //  支持的值：任意 MaxMind 数据库中的 locale，常见 "zh-CN","en","ja","ru"
    GeoIPCacheSize  int           // 默认 100000，LRU 缓存条目数
    //  影响：每 100K 缓存大约占用 12MB 内存

    // 流量过滤
    ProtocolFilter  []string      // 默认 ["tcp","udp","icmp"]，要追踪的协议
    //  "all" 表示不按协议拆分，所有流量合到一组 set
    FilterPrivate   bool          // 默认 false，是否过滤私网IP
    ExcludeCIDRs    []string      // 额外排除的 CIDR 列表（如 ["10.0.0.0/8"]）
    IncludeSubnets  []string      // 仅在白名单中的子网
    //  如果设置了这个，不在列表中的 IP 不会被追踪（但仍然 accept）

    // nftables 参数
    SetTimeout      time.Duration // 默认 5m，集合元素静默超时
    MaxSetEntries   int           // 默认 100000，集合大小上限
    CleanupOnExit   bool          // 默认 true，退出时删除整个 table
    //  如果 false：进程被 kill -9 后规则残留，需手动 nft delete table

    EnableIPv6      bool          // 默认 true，是否创建 IPv6 规则

    // 日志
    LogFormat       string        // "text" 或 "json"
    LogLevel        string        // "debug","info","warn","error"
}
```

### `internal/nftables/manager.go` ── nftables 层

#### `setDef` ── 集合定义

```go
type setDef struct {
    name      string // 完整 set 名，如 "in_tcp_v4"
    direction string // "in" 或 "out"
    protocol  string // "tcp","udp","icmp","other","all"
    ipVer     string // "v4" 或 "v6"
}
```

#### `FlowElement` ── 单次 poll 的原始数据

```go
type FlowElement struct {
    IP        string // IP 地址文本，如 "101.6.15.130" 或 "2400:c620:32:70::a"
    Direction string // "upload" 或 "download"
    Protocol  string // "tcp","udp","icmp","other","all"
    Bytes     uint64 // nftables 内核计数器的累积字节数（从 set 创建至今）
    Packets   uint64 // nftables 内核计数器的累积包数
}
```

**注意：** `Bytes`/`Packets` 是绝对值，不是增量。delta 计算在 tracker 层完成。

#### `parseNftElement()` ── nft JSON 解析器

解析 `nft --json list set ...` 输出的 JSON 元素。

```json
{
  "nftables": [
    {"metainfo": {"version": "1.0.3"}},
    {
      "set": {
        "name": "in_tcp_v4",
        "elem": [
          {"elem": {"val": "101.6.15.130",
                    "counter": {"packets": 9999, "bytes": 999999},
                    "expires": "4m30s"}}
        ]
      }
    }
  ]
}
```

支持 3 种 JSON 格式（兼容不同 nftables 版本）：
1. `{"elem": {"val": "IP", "counter": {...}}}` ── 标准格式
2. `["IP", {"counter": {...}}]` ── 数组格式（旧版本）
3. `"IP"` ── 纯字符串（无 counter 兜底）

### `internal/tracker/tracker.go` ── 增量计算层

#### `FlowKey` ── 流量流唯一标识

```go
type FlowKey struct {
    IP        string // "101.6.15.130"
    Direction string // "upload" / "download"
    Protocol  string // "tcp" / "udp" / "icmp" / "other" / "all"
}
```

**为什么需要 FlowKey 三元组？** 同一个 IP 可能同时有 TCP 下载和 UDP 查询（如 DNS），需要独立追踪。

#### `FlowDelta` ── 增量结果

```go
type FlowDelta struct {
    Key     FlowKey // 对应的流量流标识
    Bytes   uint64  // 自上次 poll 以来的字节增量
    Packets uint64  // 自上次 poll 以来的包增量
}
```

#### delta 算法

```go
// shadow map: map[FlowKey]shadowEntry
// 每个 poll 周期：
for each element from nftables:
    cur = element.Bytes
    prev = shadow[key].Bytes (如果存在)
    if !exists:
        delta = cur        // 新流，从当前值开始
    else:
        delta = cur - prev // 正常增量
        if cur < prev:     // 处理内核计数器重置
            delta = cur
    
    shadow[key] = cur
    prometheus_counter.Add(delta)
```

**计数器重置检测：** 理论上 uint64 不会溢出（184亿EB），但 nftables set 被删除重建时计数器会归零。检测到 `cur < prev` 时视为重置。

### `internal/geoip/geoip.go` ── 地理信息层

#### `GeoRecord` ── 地理信息

```go
type GeoRecord struct {
    Country      string // 国家名，如 "美国","中国","香港","未知"
    City         string // 城市名，如 "洛杉矶","广州市","杭州","未知"
    Subdivisions string // 省级行政区，如 "加州","广东","浙江","未知"
    Latitude     string // 纬度，保留4位小数，如 "34.0544"，私网IP为 "0.0000"
    Longitude    string // 经度，保留4位小数，如 "-118.244"，私网IP为 "0.0000"
}
```

**私网IP判定规则：**

```go
func isPrivateIP(ip net.IP) bool {
    ip4 := ip.To4()
    // 10.0.0.0/8
    // 172.16.0.0/12
    // 192.168.0.0/16
    // 100.64.0.0/10 (CGNAT)
    // 169.254.0.0/16 (link-local)
    // Loopback (127.0.0.0/8, ::1)
    // Link-local unicast/multicast
}
```

私网 IP 直接返回 `country="未知"`, `lat/lng="0.0000"`，不走 GeoIP 查询。

#### LRU 缓存

```go
type Lookup struct {
    db       *maxminddb.Reader   // mmap 映射的 mmdb 文件
    cache    map[string]*GeoRecord // key: IP 字符串, value: GeoRecord 指针
    cacheLru []string             // FIFO 队列，用于淘汰最旧条目
    maxSize  int                  // 最大缓存数（默认 100000）
    hits     uint64               // 缓存命中次数（用于 /metrics）
    misses   uint64               // 缓存未命中次数（即实际 mmdb 查询次数）
}
```

缓存效率：实际测试中热点 IP 命中率 >99%，每 poll 只有少量新 IP 需要实际查询 mmdb。

### `internal/metrics/metrics.go` ── Prometheus 指标层

#### `IPFlowLabels` ── 流量标签集

```go
type IPFlowLabels struct {
    IP           string // IP 地址（v4 或 v6 文本格式）
    Direction    string // "upload"（本机→远端）或 "download"（远端→本机）
    Country      string // 国家名，中文
    City         string // 城市名，中文
    Subdivisions string // 省/州，中文
    Latitude     string // 纬度浮点文本
    Longitude    string // 经度浮点文本
    Protocol     string // "tcp"/"udp"/"icmp"/"other"/"all"
    Iface        string // 网卡名（v0.1.0 固定为 "all"）
}
```

#### `Exporter` ── 指标容器

```go
type Exporter struct {
    // 主流量计数器
    bytesTotal  *prometheus.CounterVec  // ip_traffic_bytes_total
    pktsTotal   *prometheus.CounterVec  // ip_traffic_packets_total

    // 内部状态
    trackedIPs  *prometheus.GaugeVec    // ip_geo_tracked_ips
    cacheHits   prometheus.Counter       // ip_geo_cache_hits_total
    cacheMisses prometheus.Counter       // ip_geo_cache_misses_total
    cacheSize   prometheus.Gauge         // ip_geo_cache_entries
    pollDur     prometheus.Histogram     // ip_geo_poll_duration_seconds
    pollErrors  prometheus.Counter       // ip_geo_poll_errors_total
    up          prometheus.Gauge         // ip_geo_up
    buildInfo   *prometheus.GaugeVec     // ip_geo_build_info
}
```

### `main.go` ── 编排层

主循环逻辑：

```go
func main() {
    cfg := config.Parse()        // 1. 解析 CLI 参数
    mgr := nftables.New(cfg)     // 2. 初始化 nftables 管理器
    mgr.Setup()                  // 3. 创建 nftables 规则
    geo := geoip.New(...)        // 4. 加载 GeoIP 数据库
    trk := tracker.New()         // 5. 初始化 delta 追踪器
    met := metrics.New()         // 6. 注册 Prometheus 指标

    // 7. 启动 HTTP 服务 (/metrics, /health)
    // 8. 启动定时 poll 循环（每 PollInterval 执行一次 pollOnce）

    // 9. 信号处理：SIGTERM/SIGINT → cleanup() → 删除 nftables table → 退出
}

func pollOnce() {
    flows, err := mgr.Poll()     // a. 读 nftables 所有 set 的 elements
    elements := toTrackerElements(flows)
    deltas := trk.Update(elements) // b. 算 delta
    trk.CleanStale(currentKeys)    // c. 清理超时 IP 的 shadow 条目
    for _, d := range deltas {
        geoRec := geo.LookupIP(d.Key.IP) // d. GeoIP 查询
        met.AddTraffic(labels, d.Bytes, d.Packets) // e. 更新 Prometheus counter
    }
    met.SetTrackedIPs(trk.Size())   // f. 更新内部监控指标
}
```

---

## Prometheus 指标详解

### 主流量指标

#### `ip_traffic_bytes_total`

**类型:** Counter（单调递增，从不减少）

**作用:** 记录每个远端 IP 的传输字节数，是**最核心的指标**。所有流量分析和展示都基于此。

```prometheus
ip_traffic_bytes_total{
    ip="101.6.15.130",
    direction="download",
    country="中国",
    city="北京",
    subdivisions="北京",
    latitude="39.9042",
    longitude="116.4074",
    protocol="tcp",
    iface="all"
} 1194169849
```

| Label | 值示例 | 数据类型 | 是否变量？ | 说明 |
|-------|--------|---------|-----------|------|
| `ip` | `"101.6.15.130"` / `"2400:c620:32:70::a"` | string | ✅ 高基数（每 IP 唯一） | 远端 IP 地址，IPv4 和 IPv6 都用文本格式 |
| `direction` | `"download"` / `"upload"` | string | ❌ 固定 2 个值 | 从本机视角看流量方向：download=接收, upload=发送 |
| `country` | `"中国"` / `"美国"` / `"香港"` / `"未知"` | string | ⚠️ 中基数（按国家数） | GeoIP 中文国家名。私网 IP 为 `"未知"` |
| `city` | `"北京"` / `"洛杉矶"` / `"广州"` / `"未知"` | string | ⚠️ 中基数 | GeoIP 中文城市名。部分 IP 只有国家级别数据时显示 `"未知"` |
| `subdivisions` | `"浙江"` / `"加州"` / `"未知"` | string | ⚠️ 中基数 | 省/州地名 |
| `latitude` | `"39.9042"` / `"0.0000"` | string | ⚠️ 中基数 | 纬度，供 Grafana Worldmap Panel 使用。私网 IP 为 `"0.0000"` |
| `longitude` | `"116.4074"` / `"0.0000"` | string | ⚠️ 中基数 | 经度，同上 |
| `protocol` | `"tcp"` / `"udp"` / `"icmp"` / `"other"` / `"all"` | string | ❌ 固定 3~5 个值 | 传输层协议。`--protocol-filter=all` 时全为 `"all"` |
| `iface` | `"all"` | string | ❌ 固定 | 网卡名（v0.1.0 暂未实现多网卡拆分） |

**标签基数评估（最坏情况）：**

```
ip × direction × protocol = 100,000 × 2 × 4 = 800,000 条时间序列
```

**在 Grafana 中加载这个指标时，必须用 `sum by(...)` 或 `topk()` 聚合，否则会因基数太高导致浏览器崩溃。**

建议查询模式：
```promql
# ✅ 正确：按国家聚合查看
sum by(country) (rate(ip_traffic_bytes_total{direction="download"}[5m]))

# ✅ 正确：只看 Top 20 IP
topk(20, sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[15s])))

# ❌ 错误：不加聚合取所有 IP，会导致 Prometheus 返回海量数据
rate(ip_traffic_bytes_total[5m])
```

#### `ip_traffic_packets_total`

**类型:** Counter

**作用:** 记录每个远端 IP 的包数。通常与 bytes_total 配合使用算平均包大小。

```prometheus
ip_traffic_packets_total{
    ip="101.6.15.130",
    direction="download",
    country="中国",
    city="北京",
    subdivisions="北京",
    protocol="tcp",
    iface="all"
} 834567
```

**注意：** 这个指标**没有** `latitude` 和 `longitude` 标签（经纬度对包数无意义）。

查询示例：
```promql
# 平均包大小（字节/包）
rate(ip_traffic_bytes_total[5m]) / rate(ip_traffic_packets_total[5m])
```

### 内部状态指标

#### `ip_geo_up`

| 字段 | 说明 |
|------|------|
| 类型 | Gauge |
| 含义 | 1 = exporter 正常运行，0 = 未运行 |
| 用途 | Prometheus `up` 类似的健康检查 |

#### `ip_geo_build_info`

| 字段 | 说明 |
|------|------|
| 类型 | Gauge |
| Labels | `version`, `go_version` |
| 含义 | 固定为 1，携带版本信息用于排查 |

#### `ip_geo_tracked_ips`

| 字段 | 说明 |
|------|------|
| 类型 | Gauge |
| Labels | `direction` (`inbound` / `outbound`) |
| 含义 | 当前 tracker shadow map 中活跃的流数量 |
| 用途 | 监控 exporter 内部正在追踪多少 IP。如果接近 `--max-set-entries`（10万），说明 nftables set 可能快满了 |

#### `ip_geo_cache_*` ── GeoIP 缓存统计

| 指标 | 类型 | 含义 |
|------|------|------|
| `ip_geo_cache_entries` | Gauge | LRU 缓存当前条目数 |
| `ip_geo_cache_hits_total` | Counter | 缓存命中次数（无需查 mmdb） |
| `ip_geo_cache_misses_total` | Counter | 缓存未命中次数（实际查了 mmdb） |

命中率 = `hits / (hits + misses)`。正常应在 99% 以上。如果太低，增大 `--geoip-cache-size`。

#### `ip_geo_poll_duration_seconds`

| 字段 | 说明 |
|------|------|
| 类型 | Histogram |
| Buckets | [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0] |
| 含义 | 单次 poll（nftables 读取 + 解析 + delta 计算 + GeoIP 查询 + Prometheus 更新）的总耗时 |
| 用途 | 如果 poll duration 接近 `--poll-interval`，说明 exporter 处理不过来，需增大 poll 间隔 |

#### `ip_geo_poll_errors_total`

| 字段 | 说明 |
|------|------|
| 类型 | Counter |
| 含义 | nftables poll 失败次数。正常情况下应为 0 |

### PromQL 查询模式参考

```promql
# ─── 实时速率 ───
# 当前下行速率（Top 10）
topk(10, sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[15s])))

# 按国家汇总
sum by(country) (rate(ip_traffic_bytes_total{direction="download"}[5m]))

# ─── 历史峰值 ───
# 过去 1 小时内的峰值速率
max_over_time(rate(ip_traffic_bytes_total{direction="download"}[5m])[1h:])

# ─── 流量总量 ───
# 某 IP 总计下载了多少数据
ip_traffic_bytes_total{ip="101.6.15.130", direction="download"}

# ─── 平均包大小 ───
sum(rate(ip_traffic_bytes_total[5m])) / sum(rate(ip_traffic_packets_total[5m]))

# ─── Worldmap 坐标 ───
sum by(country, latitude, longitude) (rate(ip_traffic_bytes_total{direction="download"}[5m]))
```

---

## 命令行参数详解

| 参数 | 类型 | 默认值 | 行为说明 | 使用场景 |
|------|------|--------|---------|---------|
| `--listen-address` | string | `:9100` | HTTP 服务监听地址。格式 `host:port`，空 host=所有接口 | 默认 9100，避免冲突可改 |
| `--metrics-path` | string | `/metrics` | Prometheus metrics 端点路径 | Prometheus 配置中的 `metrics_path` |
| `--poll-interval` | duration | `5s` | 每多少秒读取一次 nftables 并更新指标 | 配合 Prometheus scrape_interval 使用，建议 Prometheus 间隔 ≤ 此值 |
| `--geoip-db` | string | `/usr/share/GeoIP/GeoLite2-City.mmdb` | GeoLite2-City.mmdb 文件路径。文件不存在时启动不报错，所有 IP 显示 country="未知" | - |
| `--geoip-lang` | string | `zh-CN` | MaxMind 数据库中的 locale 代码，控制国家/城市名的语言 | 中文用 `zh-CN`，英文用 `en` |
| `--geoip-cache-size` | int | `100000` | GeoIP LRU 缓最大条目数。每 10 万条 ≈ 12MB 内存 | 活跃 IP 数超过此值时，冷 IP 会频繁 miss |
| `--protocol-filter` | string | `tcp,udp,icmp` | 逗号分隔的协议列表，或 `all`。控制 nftables 创建几组 set | `all` = 不拆分协议，降低基数但丢失协议区分 |
| `--filter-private` | bool | `false` | 是否过滤（不追踪）私网 IP。`true` = 入站/出站规则增加 `ip saddr != { 10.0.0.0/8, ... }` 条件 | 只想看公网流量时启用 |
| `--exclude-cidrs` | string | `""` | 逗号分隔的 CIDR 列表，不追踪这些子网的流量 | 排除监控系统、监控端口等 |
| `--include-subnets` | string | `""` | 逗号分隔的 CIDR 列表，**只**追踪这些子网。空 = 追踪所有 | 只想看特定目标IP时使用 |
| `--set-timeout` | duration | `5m` | nftables 集合元素无流量后的超时删除时间 | 调小可减少内存，调大可保留更久的历史 IP |
| `--max-set-entries` | int | `100000` | nftables 每个 set 的最大元素数 | 高流量服务器需调大 |
| `--cleanup-on-exit` | bool | `true` | 退出时自动 `nft delete table inet ip_geo_exporter` | 设 `false` 可避免被 kill -9 后规则残留 |
| `--enable-ipv6` | bool | `true` | 是否创建 IPv6 的 set/chain/rule | 纯 IPv4 环境设 `false` 可减少 set 数量一半 |
| `--log-format` | string | `text` | 日志输出格式 `text` 或 `json` | 对接日志系统时用 `json` |
| `--log-level` | string | `info` | 日志级别 `debug`/`info`/`warn`/`error` | 调试时用 `debug` 可看到每次 poll 的详细信息 |

---

## 快速安装

### 前置要求

- Linux 内核 3.13+（任意现代 Linux 发行版）
- nftables（`apt install nftables`, `yum install nftables`）
- GeoLite2-City.mmdb 数据库（免费，~64MB）

### 下载 & 安装

```bash
# 下载二进制
wget https://github.com/kof8855/ip-geo-exporter/releases/latest/download/ip-geo-exporter-linux-amd64
chmod +x ip-geo-exporter-linux-amd64
mv ip-geo-exporter-linux-amd64 /usr/local/bin/ip-geo-exporter

# 准备 GeoIP
mkdir -p /usr/share/GeoIP
curl -L -o /usr/share/GeoIP/GeoLite2-City.mmdb https://git.io/GeoLite2-City.mmdb

# systemd 服务（需要 CAP_NET_ADMIN，不需要 root）
cat > /etc/systemd/system/ip-geo-exporter.service << 'EOF'
[Unit]
Description=ip-geo-exporter
After=network.target nftables.service

[Service]
Type=simple
ExecStart=/usr/local/bin/ip-geo-exporter \
    --listen-address=:9100 \
    --geoip-db=/usr/share/GeoIP/GeoLite2-City.mmdb \
    --poll-interval=5s
Restart=always
CapabilityBoundingSet=CAP_NET_ADMIN
AmbientCapabilities=CAP_NET_ADMIN
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now ip-geo-exporter
```

### Prometheus 配置

```yaml
scrape_configs:
  - job_name: 'ip-geo-exporter'
    scrape_interval: 5s
    scrape_timeout: 3s
    static_configs:
      - targets: ['192.168.168.16:9100']
```

### Docker 部署

```yaml
services:
  ip-geo-exporter:
    build: .
    network_mode: host
    cap_add:
      - NET_ADMIN
    volumes:
      - ./GeoLite2-City.mmdb:/usr/share/GeoIP/GeoLite2-City.mmdb:ro
    command:
      - --listen-address=:9100
      - --poll-interval=5s
    restart: unless-stopped
```

---


## 📊 PromQL 查询大全

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


## Grafana 配置

### 仪表盘：Table 面板

展示当前速率 Top 20：

```promql
# 下行
topk(20, label_join(
  sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[15s])),
  "Name", "-", "country", "ip"
))
```

### 仪表盘：Time Series 面板

展示流量趋势曲线：

```promql
topk(10, sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[15s])))
```

### 仪表盘：Worldmap 面板

按坐标展示全球流量分布：

```promql
sum by (country, latitude, longitude) (rate(ip_traffic_bytes_total{direction="download"}[5m]))
```

---

## 构建与二次开发

### 目录速查

| 文件/目录 | 职责 | 新增功能时先看这里 |
|-----------|------|------------------|
| `main.go` | 编排层：启动、poll 循环、信号处理 | 修改流程时 |
| `internal/config/config.go` | CLI 参数定义 + 校验 | 新增参数时 |
| `internal/nftables/manager.go` | nftables 通信：Setup/Poll/Teardown | 修改内核规则时 |
| `internal/geoip/geoip.go` | GeoIP 查询 + LRU 缓存 | 换 GeoIP 库或加 ASN 时 |
| `internal/tracker/tracker.go` | Delta 计算 + 计数器重置检测 | 修改增量算法时 |
| `internal/metrics/metrics.go` | Prometheus 指标注册 | 增删指标标签时 |
| `deploy/ip-geo-exporter.service` | systemd 单元 | 改部署参数时 |

### 构建命令

```bash
make build          # 编译当前平台
make build-linux-amd64  # 交叉编译 x86_64
make build-linux-arm64  # 交叉编译 ARM64
make install        # 编译 + 安装到 systemd
```

### 如何添加一个新的指标

1. 在 `internal/metrics/metrics.go` 的 `Exporter` 结构体中添加字段
2. 在 `New()` 中注册（`promauto.NewCounterVec`...）
3. 添加对应的 Setter 方法（如 `SetXxx()`）
4. 在 `main.go` 的 `pollOnce()` 中调用新方法

### 如何支持多网卡追踪

目前 `iface` 标签固定为 `"all"`。改为多网卡需要在：
1. `internal/nftables/manager.go`：为每个网卡创建独立的 set（如 `in_tcp_v4_eth0`）
2. nftables 规则中绑定 `oif`/`iif` 条件
3. `FlowElement` 增加 `Iface` 字段
4. `metrics.IPFlowLabels.Iface` 使用实际网卡名

---

## 常见问题

### Q: 为什么总字节数比 curl 报告的略大？

A: 因为 nftables 在 IP 层计数，包含 TCP/IP 协议头（以太网14B + IP头20B + TCP头20B ≈ 54B/包）。实测差异约 0.6%，完全正常。

### Q: 为什么公网 IP 显示 country="未知"？

A: 两种可能：
1. GeoLite2-City.mmdb 没有这个 IP 的数据（免费版不覆盖所有 IP）
2. IP 属于特殊地址段（如 198.18.0.0/15 基准测试段）

解决方案：换付费版 MaxMind 数据库或用 ipip.net 的库。

### Q: Prometheus 挂了半小时，恢复后会怎样？

A: Prometheus 恢复后 scrape 到最新累积值，rate() 计算 = (新值 - 旧值) / 间隔时间，得到的是"半小时平均速率"。7:30-7:59 之间的数据在 Grafana 上显示为缺口。

### Q: exporter 重启会丢数据吗？

A: 会丢 **Prometheus 层面的连续性**（内存 Counter 归零），但 nftables 内核计数器还在。重启后 exporter 从 nftables 当前值重新开始，Prometheus 的 rate() 会出现一个"跳变"（从旧值跳到新值），持续一个 scrape 周期后恢复正常。

### Q: 为什么 --filter-private 默认是 false？

A: 因为用户需要看到所有流量以便排查。如果只想看公网流量，设 `--filter-private=true`。

---

## 许可

GPL-3.0

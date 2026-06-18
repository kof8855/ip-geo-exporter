# ip-geo-exporter 用户指南

基于 **nftables 内核计数器** 的逐 IP 流量监控工具，配合 Prometheus + Grafana 展示每 IP 的上下行流量及地理位置。

> **适用人群：** 运维人员、系统管理员、网络工程师  
> **核心优势：** 内存占用仅 6~30MB（对比 bandwhich 的 300MB+），单二进制部署，无需任何依赖

---

## 📋 它能做什么？

- **监控每 IP 流量**——知道谁在跟你的服务器通信、传了多少数据
- **显示 IP 地理位置**——自动识别 IP 属于哪个国家/城市（中国-北京、美国-洛杉矶、香港...）
- **区分协议类型**——TCP 下载、UDP 查询、ICMP ping 分开统计
- **支持 IPv4 + IPv6**——IPv6 地址也能追踪
- **5 秒刷新**——配合 Grafana 看实时曲线
- **内存仅 ~30MB**——和 bandwhich 的 300MB+ 说再见

---

## 🚀 5 分钟快速部署

### 第 1 步：下载二进制

```bash
wget -O /usr/local/bin/ip-geo-exporter \
  https://github.com/kof8855/ip-geo-exporter/releases/latest/download/ip-geo-exporter-linux-amd64
chmod +x /usr/local/bin/ip-geo-exporter
```

### 第 2 步：准备 GeoIP 数据库（免费，64MB）

```bash
mkdir -p /usr/share/GeoIP
curl -L -o /usr/share/GeoIP/GeoLite2-City.mmdb https://git.io/GeoLite2-City.mmdb
```

### 第 3 步：启动

```bash
# 快速试一下（Ctrl+C 退出后自动清理）
sudo ./ip-geo-exporter --geoip-db=/usr/share/GeoIP/GeoLite2-City.mmdb
```

### 第 4 步：配 Prometheus

在 Prometheus 配置文件里加：

```yaml
- job_name: 'ip-geo-exporter'
  scrape_interval: 5s
  static_configs:
    - targets: ['你的服务器IP:9100']
```

### 第 5 步：看 Grafana

打开 Grafana → 添加 Prometheus 数据源 → 导入提供好的仪表盘 JSON，或者自己建面板：

```promql
# 下行流量 Top 20 排行
topk(20, label_join(
  sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[$__rate_interval])),
  "Name", "-", "country", "ip"
))
```

---

## 📊 所有指标大白话解释

这个 exporter 暴露的所有指标，按类别分组讲解。

### 🟢 核心指标（你最需要关注的）

#### `ip_traffic_bytes_total`

**含义：** 每个 IP 地址给你服务器传了多少字节（累计值，只增不减）  
**类型：** Counter（计数器）

```prometheus
# 例子：8.8.8.8 给你服务器发了 1000 字节（ICMP ping 流量）
ip_traffic_bytes_total{ip="8.8.8.8",direction="download",country="美国",
  city="未知",subdivisions="未知",latitude="37.7510",longitude="-97.8220",
  protocol="icmp",iface="all"} 1000
```

**每个标签的意思：**

| 标签 | 大白话 | 例子 |
|------|--------|------|
| `ip` | 跟谁在通信（对方的 IP） | `"8.8.8.8"`、`"2400:c620:32:70::a"` |
| `direction` | 数据往哪流 | `"download"`=下载（对方→你），`"upload"`=上传（你→对方） |
| `country` | 对方 IP 在哪个国家 | `"美国"`、`"中国"`、`"香港"`、`"未知"`（查不到就是未知） |
| `city` | 对方 IP 在哪个城市 | `"洛杉矶"`、`"杭州"`、`"未知"` |
| `subdivisions` | 对方 IP 在哪个省/州 | `"加州"`、`"广东"`、`"未知"` |
| `latitude` | 纬度（给地图用的） | `"34.0544"`，查不到或内网 IP 就是 `"0.0000"` |
| `longitude` | 经度（给地图用的） | `"-118.244"`，查不到或内网 IP 就是 `"0.0000"` |
| `protocol` | 用什么协议通信的 | `"tcp"`、`"udp"`、`"icmp"`、`"other"`（其他） |
| `iface` | 从哪个网卡走的 | 当前固定 `"all"`（还没按网卡拆分） |

**怎么用：**
```promql
# 当前下载速度（每秒多少字节）
rate(ip_traffic_bytes_total{direction="download"}[30s])

# 按国家汇总看看哪个国家流量最大
sum by(country) (rate(ip_traffic_bytes_total{direction="download"}[5m]))
```

#### `ip_traffic_packets_total`

**含义：** 每个 IP 给你服务器传了多少个数据包（累计值，只增不减）  
**类型：** Counter（计数器）

跟 `ip_traffic_bytes_total` 的标签完全一样，只是从"字节"换成了"包数"。

```prometheus
ip_traffic_packets_total{ip="8.8.8.8",direction="download",country="美国",
  city="未知",subdivisions="未知",protocol="icmp",iface="all"} 10
```

**怎么用：**
```promql
# 平均每个包多大（字节/包）= 字节数 ÷ 包数
rate(ip_traffic_bytes_total[5m]) / rate(ip_traffic_packets_total[5m])
# 如果结果 = 1460，说明你在下载大文件（MTU 满的）
# 如果结果 = 60~100，说明是小包交互（SSH、心跳、DNS查询）
```

---

### 🔵 健康检查指标（看 exporter 自己是否正常）

#### `ip_geo_up`

**含义：** exporter 是否活着  
**类型：** Gauge（开关）

```prometheus
ip_geo_up 1    # 1 = 活着，0 = 挂了
```

#### `ip_geo_build_info`

**含义：** 当前运行的版本信息  
**类型：** Gauge

```prometheus
ip_geo_build_info{version="0.1.0",go_version="go1.22.10"} 1    # 固定为 1
```

#### `ip_geo_tracked_ips`

**含义：** 当前在追踪多少个 IP（按方向分）  
**类型：** Gauge

```prometheus
ip_geo_tracked_ips{direction="inbound"}  1532    # 当前有 1532 个 IP 在给你发数据
ip_geo_tracked_ips{direction="outbound"} 1248    # 当前有 1248 个 IP 你在给它们发数据
```

**正常范围：** 几到几千都正常。如果接近 100000（`--max-set-entries` 默认值），说明快满了，需要调大参数。

#### `ip_geo_poll_errors_total`

**含义：** 读取 nftables 时出错的次数  
**类型：** Counter

```prometheus
ip_geo_poll_errors_total 0    # 0 = 没问题，大于 0 = 有问题
```

**正常值：** 始终为 0。如果 >0，检查 nft 命令是否可用。

#### `ip_geo_poll_duration_seconds`

**含义：** 每次采集花多长时间  
**类型：** Histogram（分布统计）

```prometheus
# 一堆数据，不用全看懂，看汇总就行：
ip_geo_poll_duration_seconds_sum      # 总共花了多少秒
ip_geo_poll_duration_seconds_count    # 采集了多少次
ip_geo_poll_duration_seconds_bucket{le="0.1"}  # 多少次在 0.1 秒以内完成
ip_geo_poll_duration_seconds_bucket{le="0.5"}  # 多少次在 0.5 秒以内完成
```

**正常值：** 平均几十到几百毫秒。如果超过 `--poll-interval`（默认 5s），说明服务器忙不过来，需要增大采集间隔。

**怎么用：**
```promql
# 平均每次采集耗时
rate(ip_geo_poll_duration_seconds_sum[5m]) / rate(ip_geo_poll_duration_seconds_count[5m])
```

#### `ip_geo_cache_entries`

**含义：** GeoIP 缓存里存了多少个 IP 的地理位置  
**类型：** Gauge

```prometheus
ip_geo_cache_entries 48500  # 缓存了 48500 个 IP 的地理信息
```

#### `ip_geo_cache_hits_total` / `ip_geo_cache_misses_total`

**含义：** 查地理位置时，缓存命中了多少次 / 没命中多少次  
**类型：** Counter

```prometheus
ip_geo_cache_hits_total   150000    # 15 万次直接读缓存（快）
ip_geo_cache_misses_total 320       # 320 次实际查了数据库（慢）
```

命中率应该 99% 以上。如果命中率低，说明 `--geoip-cache-size` 设太小了。

---

### 🟡 进程指标（exporter 自己占了多少资源）

#### `process_resident_memory_bytes`

**含义：** exporter 占了多少物理内存  
**类型：** Gauge

```prometheus
process_resident_memory_bytes 3.0e7  # ≈30MB ← 你就找这个数！
```

**正常值：** 6~30MB。如果超过 100MB，说明追踪的 IP 太多了（正常不会）。

#### `process_cpu_seconds_total`

**含义：** exporter 消耗了多少 CPU 时间  
**类型：** Counter

```prometheus
process_cpu_seconds_total 2.5  # 启动以来总共用了 2.5 秒 CPU
```

**怎么用：**
```promql
rate(process_cpu_seconds_total[5m])  # CPU 使用率（每秒多少秒CPU时间）
```

#### `process_open_fds`

**含义：** exporter 打开了多少个文件  
**类型：** Gauge

```prometheus
process_open_fds 12  # 开了 12 个文件句柄
```

**正常值：** 个位数到十几。如果几百上千，有文件泄漏。

#### `process_network_receive_bytes_total`

**含义：** Prometheus 来取数据时，网络接收了多少字节  
**类型：** Counter

```prometheus
process_network_receive_bytes_total 500000  # 总计收了 500KB
```

#### `process_network_transmit_bytes_total`

**含义：** Prometheus 来取数据时，网络发送了多少字节（即 /metrics 响应体大小）  
**类型：** Counter

```prometheus
process_network_transmit_bytes_total 8000000  # 总计发了 8MB（metrics 数据）
```

#### `process_start_time_seconds`

**含义：** exporter 是什么时候启动的  
**类型：** Gauge

```prometheus
process_start_time_seconds 1781699662  # 时间戳，可转成日期
```

#### `process_virtual_memory_bytes`

**含义：** exporter 占了多少虚拟内存（一般比物理内存大，正常）  
**类型：** Gauge

```prometheus
process_virtual_memory_bytes 1.2e9  # ≈1.2GB ← 别慌！这是虚拟内存不是实际占用的
```

#### `process_max_fds`

**含义：** 操作系统允许 exporter 最多打开多少个文件（ulimit 限制）  
**类型：** Gauge

---

### 🟠 Prometheus HTTP 指标（看 Prometheus 取数本身）

#### `promhttp_metric_handler_requests_total`

**含义：** Prometheus 来取了多少次数据，每次成功还是失败  
**类型：** Counter

```prometheus
promhttp_metric_handler_requests_total{code="200"} 850   # 成功了 850 次
promhttp_metric_handler_requests_total{code="500"} 0     # 失败了 0 次
promhttp_metric_handler_requests_total{code="503"} 0     # 拒绝了 0 次
```

#### `promhttp_metric_handler_requests_in_flight`

**含义：** 当前正在有几个 Prometheus 在同时取数据  
**类型：** Gauge

```prometheus
promhttp_metric_handler_requests_in_flight 1  # 当前有 1 个 Prometheus 在拉取
```

---

### 🔴 Go 运行时的指标（不懂就别管，专家用的）

这些是 Go 语言自己暴露的，说明程序内部状态。一般不用看，除非内存异常时排查。

| 指标 | 大白话 | 什么时候需要看 |
|------|--------|--------------|
| `go_goroutines` | 当前有多少个"并发任务"在跑 | 如果几千上万，有协程泄漏 |
| `go_gc_duration_seconds` | Go 的垃圾回收花了多长时间 | 如果经常 >1s，内存压力大 |
| `go_memstats_heap_alloc_bytes` | 堆上当前在用多少内存 | 比 RSS 小就正常 |
| `go_memstats_heap_sys_bytes` | 从系统申请了多少堆内存 | 比 RSS 大一些也正常 |
| `go_memstats_next_gc_bytes` | 内存用到多少时触发垃圾回收 | 默认是当前堆的 2 倍 |
| `go_memstats_alloc_bytes_total` | 程序启动以来总共分配了多少内存（累计） | 看程序活跃度 |
| `go_info` | Go 版本信息 | 排查兼容性问题 |
| `go_sched_gomaxprocs_threads` | 用了几核 CPU | 默认 = CPU 核数 |
| `go_threads` | 开了多少个操作系统线程 | 如果几千，异常 |
| 所有 `go_memstats_*` | Go 内存管理器的详细统计数据 | 基本用不上 |

---

## ⚙️ 常用调优

| 场景 | 命令 |
|------|------|
| 只监控公网 IP | `--filter-private=true` |
| 只看某些 IP 段 | `--include-subnets=101.0.0.0/8` |
| 不区分 TCP/UDP | `--protocol-filter=all` |
| 用英文国家名 | `--geoip-lang=en` |
| 加快刷新 | `--poll-interval=3s` |
| 减负（少扔 CPU） | `--poll-interval=10s` |

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


## ❓ 常见问题

**Q: 为什么总字节数比 curl 报告的略大？**  
A: 因为 nftables 在网络层计数，包含 TCP/IP 协议头（每包约 54 字节额外开销）。实测大约多 0.6%，正常现象。

**Q: 为什么有些 IP 显示 country="未知"？**  
A: 免费版 GeoLite2 数据库不覆盖所有 IP，或者那个 IP 是内网/特殊地址段。

**Q: Prometheus 挂了半小时再恢复，Grafana 上有什么影响？**  
A: 恢复后能得到半小时的"平均速率"，但 7:30~7:59 之间 Grafana 图上会出现一段缺口（没有数据点）。

**Q: 流量发生的时间和 Prometheus 记录的时间为什么有几秒差？**  
A: 这是 **Prometheus 拉模型固有的**——数据只在 Prometheus 来 scrape 时才采集。当前 exporter 是**纯被动模式**，没有后台定时器。延迟 ≈ 等下一次 Prometheus scrape 的时间（平均 1.5s，最长 3s）+ TCP 握手时间。如果使用 `scrape_interval: 1s`，延迟可压缩到 ~2s。

**Q: 为什么下载结束后的 Grafana 图上还有"拖尾"（速率逐渐下降）？**  
A: 这是 **Prometheus rate() 的计算方式导致的**，不是 exporter 的问题。`rate(metric[1m])` 计算的是"过去 1 分钟的平均速率"。下载结束后，这个窗口仍然包含下载期间的数据，所以要等窗口完全滑出（最长 1 分钟）才会归零。

**快速修复：** 把 Grafana 查询中的 `[$__rate_interval]` 改为 `[15s]`，并加 `> 0` 过滤零值：

```promql
# 优化前（有 ~1 分钟拖尾）
topk(10, sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[$__rate_interval])))

# 优化后（拖尾缩短到 ~15s，归零后立即消失）
topk(10, sum by(ip, country) (rate(ip_traffic_bytes_total{direction="download"}[15s])) > 0)
```

**怎样验证是不是 rate 的问题？** 直接用原始 Counter 值（不加 rate）：

```promql
ip_traffic_bytes_total{ip="183.60.240.59",direction="download"}
```

这个值在下载结束后保持不变，不会"拖尾"。如果它不变，那 rate() 趋近于 0 就是正确的数学结果。


**Q: 怎么确认纯被动模式在工作？**  
A: 观察 `ip_geo_poll_duration_seconds_count`——这个计数器每次 Prometheus scrape 时 +1。如果 3s 内 count 增加了 1，说明工作正常。不会出现"5s内+2"的情况（那是旧版混合模式的特征）。

**Q: exporter 重启了，会丢数据吗？**  
A: Prometheus 的 rate() 会跳变一下（大约 1 个采集周期），然后恢复正常。nftables 内核里计的数不会丢。

**Q: 内存 30MB 算是正常吗？**  
A: 正常。bandwhich 要 300MB+，这个是它的十分之一不到。

**Q: 这些 go_ 开头的指标需要关注吗？**  
A: 不用。它们是 Go 语言运行时自带的，除非 exporter 出异常（比如内存突然暴涨），才需要看一眼。

---

## 🧪 验证是否正常工作

```bash
# 1. 确认服务运行
curl -s http://localhost:9100/metrics | grep ip_geo_up
# 应该返回: ip_geo_up 1

# 2. 造点流量试试
ping -c 1 8.8.8.8

# 3. 等 5 秒后检查
curl -s http://localhost:9100/metrics | grep ip_traffic_bytes | head -5
# 应该能看到 8.8.8.8 的记录
```

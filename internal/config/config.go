package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── 默认值 ──────────────────────────────────────────────────────────────────────
const (
	defaultListenAddress  = ":9100"
	defaultMetricsPath    = "/metrics"
	defaultPollInterval   = 5 * time.Second
	defaultGeoIPDB        = "./GeoLite2-City.mmdb"
	defaultGeoIPLang      = "zh-CN"
	defaultGeoIPCacheSize = 100000
	defaultProtocolFilter = "tcp,udp,icmp"
	defaultSetTimeout     = 5 * time.Minute
	defaultMaxSetEntries  = 100000
	defaultCleanupOnExit  = true
	defaultEnableIPv6     = true
	defaultLogFormat      = "text"
	defaultLogLevel       = "info"
)

// ─── YAML 映射结构（字符串类型便于 yaml.v3 解析） ──────────────────────────────────
type configYAML struct {
	ListenAddress   string `yaml:"listen_address"`
	MetricsPath     string `yaml:"metrics_path"`
	PollInterval    string `yaml:"poll_interval"`    // e.g. "5s"
	GeoIPDB         string `yaml:"geoip_db"`
	GeoIPLang       string `yaml:"geoip_lang"`
	GeoIPCacheSize  int    `yaml:"geoip_cache_size"`
	ProtocolFilter  string `yaml:"protocol_filter"` // e.g. "tcp,udp,icmp" or "all"
	FilterPrivate   *bool  `yaml:"filter_private"`
	ExcludeCIDRs    string `yaml:"exclude_cidrs"`   // comma-separated
	IncludeSubnets  string `yaml:"include_subnets"` // comma-separated
	SetTimeout      string `yaml:"set_timeout"`     // e.g. "5m"
	MaxSetEntries   int    `yaml:"max_set_entries"`
	CleanupOnExit   *bool  `yaml:"cleanup_on_exit"`
	EnableIPv6      *bool  `yaml:"enable_ipv6"`
	LogFormat       string `yaml:"log_format"`
	LogLevel        string `yaml:"log_level"`
}

// ─── 主配置结构 ────────────────────────────────────────────────────────────────────
type Config struct {
	ListenAddress   string
	MetricsPath     string
	PollInterval    time.Duration

	GeoIPDB         string
	GeoIPLang       string
	GeoIPCacheSize  int

	ProtocolFilter  []string // "tcp","udp","icmp" or ["all"]
	FilterPrivate   bool
	ExcludeCIDRs    []string
	IncludeSubnets  []string

	SetTimeout      time.Duration
	MaxSetEntries   int
	CleanupOnExit   bool

	EnableIPv6      bool

	LogFormat       string
	LogLevel        string
}

func (c *Config) ProtocolsEnabled() []string {
	if len(c.ProtocolFilter) == 1 && c.ProtocolFilter[0] == "all" {
		return []string{"all"}
	}
	return c.ProtocolFilter
}

func (c *Config) Validate() error {
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll-interval must be positive, got %v", c.PollInterval)
	}
	if c.MaxSetEntries <= 0 {
		return fmt.Errorf("max-set-entries must be positive, got %d", c.MaxSetEntries)
	}
	if c.SetTimeout <= 0 {
		return fmt.Errorf("set-timeout must be positive, got %v", c.SetTimeout)
	}
	validProtos := map[string]bool{"tcp": true, "udp": true, "icmp": true, "all": true}
	seen := make(map[string]bool)
	for _, p := range c.ProtocolFilter {
		if !validProtos[p] {
			return fmt.Errorf("invalid protocol filter %q, must be one of: tcp, udp, icmp, all", p)
		}
		if seen[p] {
			return fmt.Errorf("duplicate protocol filter: %s", p)
		}
		seen[p] = true
	}
	if len(c.ProtocolFilter) == 0 {
		return errors.New("at least one protocol filter required (use \"all\" for no split)")
	}
	if len(c.ProtocolFilter) > 1 && seen["all"] {
		return errors.New("cannot combine \"all\" with specific protocols")
	}
	return nil
}

func (c *Config) SetName(direction, protocol, ipVersion string) string {
	prefix := map[string]string{
		"in":  "in",
		"out": "out",
	}[direction]
	if prefix == "" {
		prefix = direction
	}
	if protocol == "all" || protocol == "" {
		return fmt.Sprintf("%s_%s", prefix, ipVersion)
	}
	return fmt.Sprintf("%s_%s_%s", prefix, protocol, ipVersion)
}

// ─── 默认配置 ────────────────────────────────────────────────────────────────────
func defaultConfig() *Config {
	return &Config{
		ListenAddress:   defaultListenAddress,
		MetricsPath:     defaultMetricsPath,
		PollInterval:    defaultPollInterval,
		GeoIPDB:         defaultGeoIPDB,
		GeoIPLang:       defaultGeoIPLang,
		GeoIPCacheSize:  defaultGeoIPCacheSize,
		ProtocolFilter:  []string{},
		FilterPrivate:   false,
		ExcludeCIDRs:    nil,
		IncludeSubnets:  nil,
		SetTimeout:      defaultSetTimeout,
		MaxSetEntries:   defaultMaxSetEntries,
		CleanupOnExit:   defaultCleanupOnExit,
		EnableIPv6:      defaultEnableIPv6,
		LogFormat:       defaultLogFormat,
		LogLevel:        defaultLogLevel,
	}
}

// ─── YAML 合并 ────────────────────────────────────────────────────────────────────
// mergeYAML 将 YAML 文件中非零值合并到 cfg 中。
func mergeYAML(cfg *Config, y *configYAML) error {
	if y.ListenAddress != "" {
		cfg.ListenAddress = y.ListenAddress
	}
	if y.MetricsPath != "" {
		cfg.MetricsPath = y.MetricsPath
	}
	if y.PollInterval != "" {
		d, err := time.ParseDuration(y.PollInterval)
		if err != nil {
			return fmt.Errorf("invalid poll_interval %q: %w", y.PollInterval, err)
		}
		cfg.PollInterval = d
	}
	if y.GeoIPDB != "" {
		cfg.GeoIPDB = y.GeoIPDB
	}
	if y.GeoIPLang != "" {
		cfg.GeoIPLang = y.GeoIPLang
	}
	if y.GeoIPCacheSize > 0 {
		cfg.GeoIPCacheSize = y.GeoIPCacheSize
	}
	if y.ProtocolFilter != "" {
		raw := strings.TrimSpace(y.ProtocolFilter)
		if raw == "all" {
			cfg.ProtocolFilter = []string{"all"}
		} else {
			parts := strings.Split(raw, ",")
			cfg.ProtocolFilter = nil
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					cfg.ProtocolFilter = append(cfg.ProtocolFilter, p)
				}
			}
		}
	}
	if y.FilterPrivate != nil {
		cfg.FilterPrivate = *y.FilterPrivate
	}
	if y.ExcludeCIDRs != "" {
		parts := strings.Split(y.ExcludeCIDRs, ",")
		cfg.ExcludeCIDRs = nil
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.ExcludeCIDRs = append(cfg.ExcludeCIDRs, p)
			}
		}
	}
	if y.IncludeSubnets != "" {
		parts := strings.Split(y.IncludeSubnets, ",")
		cfg.IncludeSubnets = nil
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.IncludeSubnets = append(cfg.IncludeSubnets, p)
			}
		}
	}
	if y.SetTimeout != "" {
		d, err := time.ParseDuration(y.SetTimeout)
		if err != nil {
			return fmt.Errorf("invalid set_timeout %q: %w", y.SetTimeout, err)
		}
		cfg.SetTimeout = d
	}
	if y.MaxSetEntries > 0 {
		cfg.MaxSetEntries = y.MaxSetEntries
	}
	if y.CleanupOnExit != nil {
		cfg.CleanupOnExit = *y.CleanupOnExit
	}
	if y.EnableIPv6 != nil {
		cfg.EnableIPv6 = *y.EnableIPv6
	}
	if y.LogFormat != "" {
		cfg.LogFormat = y.LogFormat
	}
	if y.LogLevel != "" {
		cfg.LogLevel = y.LogLevel
	}
	return nil
}

// ─── CLI 参数解析辅助 ──────────────────────────────────────────────────────────────
// findAndRemoveConfigFlag 从 args 中扫描 --config / -config 并返回配置路径 + 过滤后的剩余参数。
func findAndRemoveConfigFlag(args []string) (configPath string, remaining []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config" || arg == "-config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++ // skip value
			}
		case strings.HasPrefix(arg, "--config="):
			configPath = arg[len("--config="):]
		case strings.HasPrefix(arg, "-config="):
			configPath = arg[len("-config="):]
		default:
			remaining = append(remaining, arg)
		}
	}
	return
}

// ─── 主入口 ───────────────────────────────────────────────────────────────────────
// Parse 解析配置，优先级：CLI 参数 > YAML 配置文件 > 内置默认值。
func Parse() *Config {
	cfg := defaultConfig()

	// ① 从 os.Args 中提取 --config 路径
	configPath, filteredArgs := findAndRemoveConfigFlag(os.Args[1:])

	// ② 如果指定了配置文件，加载并合并
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: cannot read config file %s: %v\n", configPath, err)
		} else {
			var yamlCfg configYAML
			if err := yaml.Unmarshal(data, &yamlCfg); err != nil {
				fmt.Fprintf(os.Stderr, "[config] warning: cannot parse config file %s: %v\n", configPath, err)
			} else {
				if err := mergeYAML(cfg, &yamlCfg); err != nil {
					fmt.Fprintf(os.Stderr, "[config] warning: config file merge error: %v\n", err)
				}
			}
		}
	}

	// ③ 如果 YAML 没设 ProtocolFilter，初始化为默认
	if len(cfg.ProtocolFilter) == 0 {
		cfg.ProtocolFilter = parseProtocolFilter(defaultProtocolFilter)
	}

	// ④ 注册 CLI 参数（使用 cfg 当前值作为默认值，使其可被命令行覆盖）
	flag.StringVar(&cfg.ListenAddress, "listen-address", cfg.ListenAddress, "HTTP 监听地址 (默认: "+defaultListenAddress+")")
	flag.StringVar(&cfg.MetricsPath, "metrics-path", cfg.MetricsPath, "Prometheus metrics 路径 (默认: "+defaultMetricsPath+")")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "nftables 采集间隔 (默认: "+defaultPollInterval.String()+")")

	flag.StringVar(&cfg.GeoIPDB, "geoip-db", cfg.GeoIPDB, "GeoLite2-City.mmdb 路径 (默认: "+defaultGeoIPDB+")")
	flag.StringVar(&cfg.GeoIPLang, "geoip-lang", cfg.GeoIPLang, "地理名称语言 (默认: "+defaultGeoIPLang+")")
	flag.IntVar(&cfg.GeoIPCacheSize, "geoip-cache-size", cfg.GeoIPCacheSize, "GeoIP LRU 缓存大小 (默认: "+fmt.Sprint(defaultGeoIPCacheSize)+")")

	protoDefault := strings.Join(cfg.ProtocolFilter, ",")
	protoFlag := flag.String("protocol-filter", protoDefault, "协议拆分 tcp,udp,icmp 或 all (默认: "+defaultProtocolFilter+")")

	flag.BoolVar(&cfg.FilterPrivate, "filter-private", cfg.FilterPrivate, "过滤 RFC1918 内网 IP (默认: false)")

	var excludeDefault, includeDefault string
	if len(cfg.ExcludeCIDRs) > 0 {
		excludeDefault = strings.Join(cfg.ExcludeCIDRs, ",")
	}
	if len(cfg.IncludeSubnets) > 0 {
		includeDefault = strings.Join(cfg.IncludeSubnets, ",")
	}
	excludeStr := flag.String("exclude-cidrs", excludeDefault, "额外排除的 CIDR 网段，逗号分隔")
	includeStr := flag.String("include-subnets", includeDefault, "仅监控这些子网，逗号分隔 (空=全部)")

	flag.DurationVar(&cfg.SetTimeout, "set-timeout", cfg.SetTimeout, "nftables 集合元素超时 (默认: "+defaultSetTimeout.String()+")")
	flag.IntVar(&cfg.MaxSetEntries, "max-set-entries", cfg.MaxSetEntries, "nftables 集合最大条目数 (默认: "+fmt.Sprint(defaultMaxSetEntries)+")")
	flag.BoolVar(&cfg.CleanupOnExit, "cleanup-on-exit", cfg.CleanupOnExit, "退出时自动删除 nftables 规则 (默认: "+fmt.Sprint(defaultCleanupOnExit)+")")

	flag.BoolVar(&cfg.EnableIPv6, "enable-ipv6", cfg.EnableIPv6, "启用 IPv6 追踪 (默认: "+fmt.Sprint(defaultEnableIPv6)+")")

	flag.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "日志格式 text 或 json (默认: "+defaultLogFormat+")")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "日志级别 debug/info/warn/error (默认: "+defaultLogLevel+")")

	// ⑤ 解析 CLI 参数（已过滤 --config）
	flag.CommandLine.Parse(filteredArgs)

	// ⑥ 协议过滤器特殊处理（因为 flag 是 string 类型但 cfg 是 []string）
	raw := strings.TrimSpace(*protoFlag)
	if raw == "all" {
		cfg.ProtocolFilter = []string{"all"}
	} else {
		cfg.ProtocolFilter = parseProtocolFilter(raw)
	}

	// ⑦ 解析 CIDR
	if *excludeStr != "" {
		cfg.ExcludeCIDRs = splitTrim(*excludeStr, ",")
	}
	if *includeStr != "" {
		cfg.IncludeSubnets = splitTrim(*includeStr, ",")
	}

	return cfg
}

// ─── 小工具 ───────────────────────────────────────────────────────────────────────
func parseProtocolFilter(raw string) []string {
	if raw == "all" {
		return []string{"all"}
	}
	return splitTrim(raw, ",")
}

func splitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

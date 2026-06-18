package config

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
)

type Config struct {
	ListenAddress   string
	MetricsPath     string
	PollInterval    time.Duration

	GeoIPDB         string
	GeoIPLang       string
	GeoIPCacheSize  int

	ProtocolFilter  []string // "tcp","udp","icmp" or "all"
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
	if c.GeoIPDB == "" {
		return errors.New("geoip-db path is required")
	}
	return nil
}

func (c *Config) SetName(direction, protocol, ipVersion string) string {
	// Returns set name like: out_tcp_v4, in_udp_v6, in_v4 (no split)
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

func Parse() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.ListenAddress, "listen-address", ":9100", "HTTP listen address")
	flag.StringVar(&cfg.MetricsPath, "metrics-path", "/metrics", "Prometheus metrics path")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", 5*time.Second, "nftables poll interval")

	flag.StringVar(&cfg.GeoIPDB, "geoip-db", "/usr/share/GeoIP/GeoLite2-City.mmdb", "Path to GeoLite2-City.mmdb")
	flag.StringVar(&cfg.GeoIPLang, "geoip-lang", "zh-CN", "GeoIP locale for country/city names")
	flag.IntVar(&cfg.GeoIPCacheSize, "geoip-cache-size", 100000, "GeoIP LRU cache size")

	protoFilter := flag.String("protocol-filter", "tcp,udp,icmp", "Protocol split: comma-separated (tcp,udp,icmp) or \"all\" to disable")
	flag.BoolVar(&cfg.FilterPrivate, "filter-private", false, "Filter RFC1918 private IPs (default: show all)")
	excludeStr := flag.String("exclude-cidrs", "", "Extra CIDRs to exclude (comma-separated, e.g. 10.0.0.0/8)")
	includeStr := flag.String("include-subnets", "", "Only monitor these subnets (comma-separated, empty=all)")

	flag.DurationVar(&cfg.SetTimeout, "set-timeout", 5*time.Minute, "nftables set element timeout")
	flag.IntVar(&cfg.MaxSetEntries, "max-set-entries", 100000, "nftables set max entries")
	flag.BoolVar(&cfg.CleanupOnExit, "cleanup-on-exit", true, "Delete nftables table on exit")

	flag.BoolVar(&cfg.EnableIPv6, "enable-ipv6", true, "Enable IPv6 tracking")

	flag.StringVar(&cfg.LogFormat, "log-format", "text", "Log format: text or json")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "Log level: debug, info, warn, error")

	flag.Parse()

	// Parse protocol filter
	raw := strings.TrimSpace(*protoFilter)
	if raw == "all" {
		cfg.ProtocolFilter = []string{"all"}
	} else {
		parts := strings.Split(raw, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cfg.ProtocolFilter = append(cfg.ProtocolFilter, p)
			}
		}
	}

	// Parse CIDR lists
	if *excludeStr != "" {
		cfg.ExcludeCIDRs = strings.Split(*excludeStr, ",")
		for i := range cfg.ExcludeCIDRs {
			cfg.ExcludeCIDRs[i] = strings.TrimSpace(cfg.ExcludeCIDRs[i])
		}
	}
	if *includeStr != "" {
		cfg.IncludeSubnets = strings.Split(*includeStr, ",")
		for i := range cfg.IncludeSubnets {
			cfg.IncludeSubnets[i] = strings.TrimSpace(cfg.IncludeSubnets[i])
		}
	}

	return cfg
}

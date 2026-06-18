package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kof8855/ip-geo-exporter/internal/config"
	"github.com/kof8855/ip-geo-exporter/internal/geoip"
	"github.com/kof8855/ip-geo-exporter/internal/metrics"
	"github.com/kof8855/ip-geo-exporter/internal/nftables"
	"github.com/kof8855/ip-geo-exporter/internal/tracker"
)

const version = "0.1.3"

func main() {
	cfg := config.Parse()

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	setupLogging(cfg.LogFormat, cfg.LogLevel)
	slog.Info("starting ip-geo-exporter", "version", version)

	// Check nftables availability
	nftables.MustRunNftCheck()

	// Initialize nftables manager
	mgr := nftables.New(cfg)
	if err := mgr.Setup(); err != nil {
		slog.Error("nftables setup failed", "error", err)
		os.Exit(1)
	}
	slog.Info("nftables rules installed")

	// Initialize GeoIP
	var geo *geoip.Lookup
	if cfg.GeoIPDB != "" {
		var err error
		geo, err = geoip.New(cfg.GeoIPDB, cfg.GeoIPLang, cfg.GeoIPCacheSize)
		if err != nil {
			slog.Warn("GeoIP database not loaded, all locations will be unknown", "path", cfg.GeoIPDB, "error", err)
			geo = nil
		} else {
			defer geo.Close()
			slog.Info("GeoIP database loaded", "path", cfg.GeoIPDB, "lang", cfg.GeoIPLang)
		}
	} else {
		slog.Warn("no GeoIP database specified, all locations will be unknown")
	}

	// Initialize tracker + metrics
	trk := tracker.New()
	met := metrics.New()
	goVersion := strings.TrimPrefix(runtime.Version(), "go")
	if bi, ok := debug.ReadBuildInfo(); ok {
		goVersion = bi.GoVersion
	}
	met.SetBuildInfo(version, goVersion)

	// activeLabels remembers the full label set for each flow, needed to
	// delete stale Prometheus counters when nftables times out the IP.
	activeLabels := make(map[tracker.FlowKey]metrics.IPFlowLabels)
	var activeLabelsMu sync.RWMutex

	ifaceName := "all"

	// ─── /metrics handler: 每次 Prometheus scrape 触发一次 poll ─────
	mux := http.NewServeMux()
	mux.Handle(cfg.MetricsPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollOnce(context.Background(), mgr, trk, met, geo, ifaceName, activeLabels, &activeLabelsMu)
		promhttp.Handler().ServeHTTP(w, r)
	}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: cfg.ListenAddress, Handler: mux}
	go func() {
		slog.Info("HTTP server listening", "address", cfg.ListenAddress, "path", cfg.MetricsPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	// 初始一次 poll（预热 shadow map，启动后马上有数据）
	pollOnce(context.Background(), mgr, trk, met, geo, ifaceName, activeLabels, &activeLabelsMu)
	slog.Info("exporter ready — waiting for Prometheus scrapes (passive mode)")

	// ─── 等待信号 ────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutdown signal received")
	if cfg.CleanupOnExit {
		slog.Info("cleaning up nftables rules")
		if err := mgr.Teardown(); err != nil {
			slog.Warn("nftables teardown had errors", "error", err)
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
	slog.Info("shutdown complete")
}

func pollOnce(ctx context.Context, mgr *nftables.Manager, trk *tracker.Tracker, met *metrics.Exporter, geo *geoip.Lookup, ifaceName string, activeLabels map[tracker.FlowKey]metrics.IPFlowLabels, labelsMu *sync.RWMutex) {
	start := time.Now()

	flows, err := mgr.Poll()
	if err != nil {
		slog.Error("nftables poll failed", "error", err)
		met.IncPollErrors()
		met.ObservePollDuration(time.Since(start).Seconds())
		return
	}

	// 构建当前 keys + tracker elements
	elements := make([]tracker.FlowElement, len(flows))
	currentKeys := make(map[tracker.FlowKey]bool, len(flows))
	for i, f := range flows {
		elements[i] = tracker.FlowElement{IP: f.IP, Direction: f.Direction, Protocol: f.Protocol, Bytes: f.Bytes, Packets: f.Packets}
		currentKeys[tracker.FlowKey{IP: f.IP, Direction: f.Direction, Protocol: f.Protocol}] = true
	}

	// 计算增量
	deltas := trk.Update(elements)

	// 清理 stale：nftables 已超时删除的 IP → 同时删除 Prometheus 计数器
	staleKeys := trk.CleanStale(currentKeys)
	if len(staleKeys) > 0 {
		labelsMu.Lock()
		var staleLabels []metrics.IPFlowLabels
		for _, k := range staleKeys {
			if ls, ok := activeLabels[k]; ok {
				staleLabels = append(staleLabels, ls)
				delete(activeLabels, k)
			}
		}
		labelsMu.Unlock()
		if len(staleLabels) > 0 {
			met.DeleteFlows(staleLabels)
			slog.Debug("deleted stale counters", "count", len(staleLabels))
		}
	}

	// GeoIP 富化 + 更新指标
	for _, d := range deltas {
		if d.Bytes == 0 && d.Packets == 0 {
			continue
		}
		geoRec := &geoip.GeoRecord{Country: "未知", City: "未知", Subdivisions: "未知", Latitude: "0.0000", Longitude: "0.0000"}
		if geo != nil {
			geoRec = geo.LookupIP(d.Key.IP)
		}
		labels := metrics.IPFlowLabels{
			IP: d.Key.IP, Direction: d.Key.Direction,
			Country: geoRec.Country, City: geoRec.City, Subdivisions: geoRec.Subdivisions,
			Latitude: geoRec.Latitude, Longitude: geoRec.Longitude,
			Protocol: d.Key.Protocol, Iface: ifaceName,
		}
		met.AddTraffic(labels, d.Bytes, d.Packets)

		labelsMu.Lock()
		activeLabels[d.Key] = labels
		labelsMu.Unlock()
	}

	// 内部状态
	met.SetTrackedIPs(trk.Size(), trk.Size())
	if geo != nil {
		hits, misses, entries := geo.Stats()
		met.SetCacheStats(hits, misses, entries)
	}
	met.ObservePollDuration(time.Since(start).Seconds())
}

func setupLogging(format, level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}

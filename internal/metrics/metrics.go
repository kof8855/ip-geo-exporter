package metrics

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Exporter holds all Prometheus metrics for ip-geo-exporter.
type Exporter struct {
	bytesTotal  *prometheus.CounterVec
	pktsTotal   *prometheus.CounterVec

	trackedIPs  *prometheus.GaugeVec
	cacheHits   prometheus.Counter
	cacheMisses prometheus.Counter
	cacheSize   prometheus.Gauge
	pollDur     prometheus.Histogram
	pollErrors  prometheus.Counter
	up          prometheus.Gauge
	buildInfo   *prometheus.GaugeVec
}

// New creates and registers all Prometheus metrics.
func New() *Exporter {
	e := &Exporter{}

	e.bytesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ip_traffic_bytes_total",
			Help: "Total bytes transferred to/from remote address",
		},
		[]string{"ip", "direction", "country", "city", "subdivisions", "latitude", "longitude", "protocol", "iface"},
	)

	e.pktsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ip_traffic_packets_total",
			Help: "Total packets transferred to/from remote address",
		},
		[]string{"ip", "direction", "country", "city", "subdivisions", "protocol", "iface"},
	)

	e.trackedIPs = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ip_geo_tracked_ips",
			Help: "Current number of tracked IPs by direction",
		},
		[]string{"direction"},
	)

	e.cacheHits = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "ip_geo_cache_hits_total",
			Help: "Total GeoIP cache hits",
		},
	)

	e.cacheMisses = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "ip_geo_cache_misses_total",
			Help: "Total GeoIP cache misses",
		},
	)

	e.cacheSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "ip_geo_cache_entries",
			Help: "Current number of entries in GeoIP cache",
		},
	)

	e.pollDur = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "ip_geo_poll_duration_seconds",
			Help:    "Histogram of nftables poll duration",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
		},
	)

	e.pollErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "ip_geo_poll_errors_total",
			Help: "Total poll errors",
		},
	)

	e.up = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "ip_geo_up",
			Help: "Whether the exporter is running",
		},
	)

	e.buildInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ip_geo_build_info",
			Help: "Build information",
		},
		[]string{"version", "go_version"},
	)

	e.up.Set(1)

	slog.Info("Prometheus metrics registered")
	return e
}

// IPFlowLabels holds the label values for a traffic record.
type IPFlowLabels struct {
	IP           string
	Direction    string
	Country      string
	City         string
	Subdivisions string
	Latitude     string
	Longitude    string
	Protocol     string
	Iface        string
}

// AddTraffic increments bytes and packets counters for a flow.
func (e *Exporter) AddTraffic(labels IPFlowLabels, bytesDelta, packetsDelta uint64) {
	if bytesDelta > 0 {
		e.bytesTotal.WithLabelValues(
			labels.IP, labels.Direction, labels.Country, labels.City,
			labels.Subdivisions, labels.Latitude, labels.Longitude,
			labels.Protocol, labels.Iface,
		).Add(float64(bytesDelta))
	}
	if packetsDelta > 0 {
		e.pktsTotal.WithLabelValues(
			labels.IP, labels.Direction, labels.Country, labels.City,
			labels.Subdivisions, labels.Protocol, labels.Iface,
		).Add(float64(packetsDelta))
	}
}

// DeleteFlows removes the Prometheus counters for stale flows (IPs that have
// timed out from nftables and no longer have traffic). This prevents stale
// counter values from persisting in /metrics and wasting Prometheus storage.
func (e *Exporter) DeleteFlows(labels []IPFlowLabels) {
	for _, l := range labels {
		e.bytesTotal.Delete(prometheus.Labels{
			"ip": l.IP, "direction": l.Direction, "country": l.Country,
			"city": l.City, "subdivisions": l.Subdivisions,
			"latitude": l.Latitude, "longitude": l.Longitude,
			"protocol": l.Protocol, "iface": l.Iface,
		})
		e.pktsTotal.Delete(prometheus.Labels{
			"ip": l.IP, "direction": l.Direction, "country": l.Country,
			"city": l.City, "subdivisions": l.Subdivisions,
			"protocol": l.Protocol, "iface": l.Iface,
		})
	}
}

// SetTrackedIPs updates the tracked IP counts.
func (e *Exporter) SetTrackedIPs(inbound, outbound int) {
	e.trackedIPs.WithLabelValues("inbound").Set(float64(inbound))
	e.trackedIPs.WithLabelValues("outbound").Set(float64(outbound))
}

// SetCacheStats updates GeoIP cache statistics.
func (e *Exporter) SetCacheStats(hits, misses uint64, entries int) {
	e.cacheHits.Add(float64(hits))
	e.cacheMisses.Add(float64(misses))
	e.cacheSize.Set(float64(entries))
}

// ObservePollDuration records poll duration.
func (e *Exporter) ObservePollDuration(durSeconds float64) {
	e.pollDur.Observe(durSeconds)
}

// IncPollErrors increments the poll error counter.
func (e *Exporter) IncPollErrors() {
	e.pollErrors.Inc()
}

// SetBuildInfo sets the build info metric.
func (e *Exporter) SetBuildInfo(version, goVersion string) {
	e.buildInfo.WithLabelValues(version, goVersion).Set(1)
}

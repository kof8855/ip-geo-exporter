package geoip

import (
	"fmt"
	"net"
	"sync"

	"github.com/oschwald/maxminddb-golang"
)

// GeoRecord holds geographic information for an IP address.
type GeoRecord struct {
	Country      string // e.g. "美国", "中国", "香港"
	City         string // e.g. "洛杉矶", "广州市" or "未知"
	Subdivisions string // e.g. "加州", "广东" or "未知"
	Latitude     string // e.g. "34.0544"
	Longitude    string // e.g. "-118.244"
}

// Lookup handles GeoIP queries with an LRU cache.
type Lookup struct {
	db       *maxminddb.Reader
	lang     string
	dbPath   string

	mu       sync.RWMutex
	cache    map[string]*GeoRecord
	cacheLru []string // simple FIFO tracking for cache eviction
	maxSize  int
	nextSlot int

	hits   uint64
	misses uint64
}

// New opens a GeoIP database and returns a Lookup.
func New(dbPath string, lang string, cacheSize int) (*Lookup, error) {
	db, err := maxminddb.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open maxminddb %s: %w", dbPath, err)
	}

	if cacheSize <= 0 {
		cacheSize = 100000
	}

	return &Lookup{
		db:       db,
		lang:     lang,
		dbPath:   dbPath,
		cache:    make(map[string]*GeoRecord, cacheSize),
		cacheLru: make([]string, cacheSize),
		maxSize:  cacheSize,
	}, nil
}

// Close closes the GeoIP database.
func (l *Lookup) Close() {
	if l.db != nil {
		l.db.Close()
	}
}

// LookupIP returns GeoIP information for the given IP address.
func (l *Lookup) LookupIP(ipStr string) *GeoRecord {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return &GeoRecord{
			Country:      "未知",
			City:         "未知",
			Subdivisions: "未知",
			Latitude:     "0.0000",
			Longitude:    "0.0000",
		}
	}

	// Check private/ranges that GeoIP won't handle
	if isPrivateIP(ip) {
		return &GeoRecord{
			Country:      "未知",
			City:         "未知",
			Subdivisions: "未知",
			Latitude:     "0.0000",
			Longitude:    "0.0000",
		}
	}

	// Check cache
	{
		l.mu.RLock()
		rec, ok := l.cache[ipStr]
		l.mu.RUnlock()
		if ok {
			l.mu.Lock()
			l.hits++
			l.mu.Unlock()
			return rec
		}
	}

	l.mu.Lock()
	l.misses++
	l.mu.Unlock()

	// Query GeoIP
	rec := l.queryGeoIP(ip)

	// Store in cache
	l.mu.Lock()
	l.cache[ipStr] = rec
	// Simple FIFO eviction
	if l.nextSlot < l.maxSize {
		l.cacheLru[l.nextSlot] = ipStr
		l.nextSlot++
	} else {
		// Evict oldest
		oldest := l.cacheLru[0]
		delete(l.cache, oldest)
		copy(l.cacheLru, l.cacheLru[1:])
		l.cacheLru[l.maxSize-1] = ipStr
	}
	l.mu.Unlock()

	return rec
}

// Stats returns cache hit/miss counts.
func (l *Lookup) Stats() (hits, misses uint64, entries int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.hits, l.misses, len(l.cache)
}

func (l *Lookup) queryGeoIP(ip net.IP) *GeoRecord {
	var record struct {
		Country struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"country"`
		City struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"city"`
		Subdivisions []struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"subdivisions"`
		Location struct {
			Latitude  float64 `maxminddb:"latitude"`
			Longitude float64 `maxminddb:"longitude"`
		} `maxminddb:"location"`
	}

	err := l.db.Lookup(ip, &record)
	if err != nil {
		return &GeoRecord{
			Country:      "未知",
			City:         "未知",
			Subdivisions: "未知",
			Latitude:     "0.0000",
			Longitude:    "0.0000",
		}
	}

	result := &GeoRecord{}

	// Country
	if name, ok := record.Country.Names[l.lang]; ok {
		result.Country = name
	} else if name, ok := record.Country.Names["en"]; ok {
		result.Country = name
	} else {
		result.Country = "未知"
	}

	// City
	if name, ok := record.City.Names[l.lang]; ok {
		result.City = name
	} else if name, ok := record.City.Names["en"]; ok {
		result.City = name
	} else {
		result.City = "未知"
	}

	// Subdivisions (state/province)
	if len(record.Subdivisions) > 0 {
		if name, ok := record.Subdivisions[0].Names[l.lang]; ok {
			result.Subdivisions = name
		} else if name, ok := record.Subdivisions[0].Names["en"]; ok {
			result.Subdivisions = name
		} else {
			result.Subdivisions = "未知"
		}
	} else {
		result.Subdivisions = "未知"
	}

	// Coordinates
	result.Latitude = fmt.Sprintf("%.4f", record.Location.Latitude)
	result.Longitude = fmt.Sprintf("%.4f", record.Location.Longitude)

	return result
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// RFC1918
		if ip4[0] == 10 {
			return true
		}
		if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return true
		}
		if ip4[0] == 192 && ip4[1] == 168 {
			return true
		}
		// RFC6598 (CGNAT)
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// RFC3927 (link-local)
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	}
	return false
}

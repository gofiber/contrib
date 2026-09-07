package monitor

import (
	"strconv"
	"time"
)

type snapshot struct {
	CollectedAt time.Time       `json:"collected_at"`
	Collection  collectionStats `json:"collection"`
	Process     processStats    `json:"process"`
	Runtime     runtimeStats    `json:"runtime"`
	System      systemStats     `json:"system"`
	HTTP        httpStats       `json:"http"`
	PID         legacyPIDStats  `json:"pid"`
	OS          legacyOSStats   `json:"os"`
}

type collectionStats struct {
	Partial bool     `json:"partial"`
	Errors  []string `json:"errors"`
}

type processStats struct {
	CPUPercent      *float64 `json:"cpu_percent"`
	RSSBytes        *uint64  `json:"rss_bytes"`
	Threads         *int32   `json:"threads"`
	OpenDescriptors *int32   `json:"open_descriptors"`
	TCPConnections  *int     `json:"tcp_connections"`
	UptimeSeconds   uint64   `json:"uptime_seconds"`
}

type runtimeStats struct {
	Goroutines            int     `json:"goroutines"`
	HeapAllocBytes        uint64  `json:"heap_alloc_bytes"`
	HeapSysBytes          uint64  `json:"heap_sys_bytes"`
	HeapInuseBytes        uint64  `json:"heap_inuse_bytes"`
	HeapIdleBytes         uint64  `json:"heap_idle_bytes"`
	HeapReleasedBytes     uint64  `json:"heap_released_bytes"`
	HeapObjects           uint64  `json:"heap_objects"`
	NextGCBytes           uint64  `json:"next_gc_bytes"`
	Mallocs               uint64  `json:"mallocs"`
	Frees                 uint64  `json:"frees"`
	GCCount               uint64  `json:"gc_count"`
	GCPauseMetricsEnabled bool    `json:"gc_pause_metrics_enabled"`
	GCPauseLastNS         *uint64 `json:"gc_pause_last_ns"`
	GCPauseWindowNS       *uint64 `json:"gc_pause_window_ns"`
	GCPauseTotalNS        *uint64 `json:"gc_pause_total_ns"`
	GCCPUFraction         float64 `json:"gc_cpu_fraction"`
	GOMAXPROCS            int     `json:"gomaxprocs"`
}

type systemStats struct {
	CPUPercent           *float64 `json:"cpu_percent"`
	MemoryUsedPercent    *float64 `json:"memory_used_percent"`
	MemoryUsedBytes      *uint64  `json:"memory_used_bytes"`
	MemoryTotalBytes     *uint64  `json:"memory_total_bytes"`
	MemoryAvailableBytes *uint64  `json:"memory_available_bytes"`
	DiskUsedPercent      *float64 `json:"disk_used_percent"`
	DiskUsedBytes        *uint64  `json:"disk_used_bytes"`
	DiskTotalBytes       *uint64  `json:"disk_total_bytes"`
	DiskFreeBytes        *uint64  `json:"disk_free_bytes"`
	DiskFSType           *string  `json:"disk_fstype"`
	Load1                *float64 `json:"load1"`
	Load5                *float64 `json:"load5"`
	Load15               *float64 `json:"load15"`
	NetworkReceiveBPS    *float64 `json:"network_receive_bps"`
	NetworkSendBPS       *float64 `json:"network_send_bps"`
	TCPConnections       *int     `json:"tcp_connections"`
}

type httpStats struct {
	Requests uint64          `json:"requests"`
	InFlight uint64          `json:"in_flight"`
	RPS      *float64        `json:"rps"`
	Status   httpStatusStats `json:"status"`
	Rates    httpRateStats   `json:"rates"`
	Latency  latencyStats    `json:"latency"`
}

type httpStatusStats struct {
	Status1xx uint64 `json:"1xx"`
	Status2xx uint64 `json:"2xx"`
	Status3xx uint64 `json:"3xx"`
	Status4xx uint64 `json:"4xx"`
	Status5xx uint64 `json:"5xx"`
}

type httpRateStats struct {
	Status4xx *float64 `json:"4xx"`
	Status5xx *float64 `json:"5xx"`
}

type latencyStats struct {
	P50NS *uint64 `json:"p50_ns"`
	P95NS *uint64 `json:"p95_ns"`
	P99NS *uint64 `json:"p99_ns"`
}

type legacyPIDStats struct {
	CPU        float64 `json:"cpu"`
	RAM        uint64  `json:"ram"`
	Conns      int     `json:"conns"`
	Goroutines int     `json:"goroutines"`
	Requests   string  `json:"requests"`
	Uptime     float64 `json:"uptime"`
}

type legacyOSStats struct {
	CPU      float64 `json:"cpu"`
	RAM      uint64  `json:"ram"`
	TotalRAM uint64  `json:"total_ram"`
	LoadAvg  float64 `json:"load_avg"`
	Conns    int     `json:"conns"`
}

// withLegacyViews derives the compatibility payload from the primary snapshot.
// Legacy numeric fields intentionally retain their original non-null JSON types.
func (s snapshot) withLegacyViews() snapshot {
	s.PID = legacyPIDStats{
		CPU:        valueOrZero(s.Process.CPUPercent),
		RAM:        valueOrZero(s.Process.RSSBytes),
		Conns:      valueOrZero(s.Process.TCPConnections),
		Goroutines: s.Runtime.Goroutines,
		Requests:   strconv.FormatUint(s.HTTP.Requests, 10),
		Uptime:     float64(s.Process.UptimeSeconds),
	}
	s.OS = legacyOSStats{
		CPU:      valueOrZero(s.System.CPUPercent),
		RAM:      valueOrZero(s.System.MemoryUsedBytes),
		TotalRAM: valueOrZero(s.System.MemoryTotalBytes),
		LoadAvg:  valueOrZero(s.System.Load1),
		Conns:    valueOrZero(s.System.TCPConnections),
	}
	return s
}

type cacheEntry struct {
	snapshot snapshot
	cachedAt time.Time
}

// currentSnapshot publishes immutable cache entries and uses a double-checked
// lock so concurrent cache misses perform at most one collection. The lock also
// serializes collector baselines and the destructive histogram window reset.
func (m *middleware) currentSnapshot(now time.Time) snapshot {
	if entry := m.cache.Load(); cacheFresh(entry, now, m.refresh) {
		return entry.snapshot
	}

	m.collectMu.Lock()
	defer m.collectMu.Unlock()

	if entry := m.cache.Load(); cacheFresh(entry, now, m.refresh) {
		return entry.snapshot
	}

	current := m.collectFn(now)
	m.cache.Store(&cacheEntry{snapshot: current, cachedAt: now})
	return current
}

func cacheFresh(entry *cacheEntry, now time.Time, ttl time.Duration) bool {
	if entry == nil {
		return false
	}
	age := now.Sub(entry.cachedAt)
	// A negative age means this caller captured now before a concurrent caller
	// published a newer entry. Treating it as fresh prevents duplicate collection
	// and keeps window baselines from moving backwards.
	return age < ttl
}

func (m *middleware) collectSnapshot(now time.Time) snapshot {
	return m.collector.collect(m, now)
}

func valuePointer[T any](value T) *T {
	return &value
}

func valueOrZero[T any](value *T) (zero T) {
	if value != nil {
		return *value
	}
	return zero
}

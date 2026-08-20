package stats

import "time"

type snapshot struct {
	CollectedAt time.Time       `json:"collected_at"`
	Collection  collectionStats `json:"collection"`
	Process     processStats    `json:"process"`
	Runtime     runtimeStats    `json:"runtime"`
	System      systemStats     `json:"system"`
	HTTP        httpStats       `json:"http"`
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
	UptimeSeconds   uint64   `json:"uptime_seconds"`
}

type runtimeStats struct {
	Goroutines     int    `json:"goroutines"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapObjects    uint64 `json:"heap_objects"`
	GCCount        uint32 `json:"gc_count"`
	GCPauseLastNS  uint64 `json:"gc_pause_last_ns"`
}

type systemStats struct {
	CPUPercent        *float64 `json:"cpu_percent"`
	MemoryUsedPercent *float64 `json:"memory_used_percent"`
	MemoryUsedBytes   *uint64  `json:"memory_used_bytes"`
	MemoryTotalBytes  *uint64  `json:"memory_total_bytes"`
	DiskUsedPercent   *float64 `json:"disk_used_percent"`
	DiskUsedBytes     *uint64  `json:"disk_used_bytes"`
	DiskTotalBytes    *uint64  `json:"disk_total_bytes"`
	Load1             *float64 `json:"load1"`
	NetworkReceiveBPS *float64 `json:"network_receive_bps"`
	NetworkSendBPS    *float64 `json:"network_send_bps"`
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

type cacheEntry struct {
	snapshot snapshot
	cachedAt time.Time
}

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
	return age >= 0 && age < ttl
}

func (m *middleware) collectSnapshot(now time.Time) snapshot {
	return m.collector.collect(m, now)
}

func valuePointer[T any](value T) *T {
	return &value
}

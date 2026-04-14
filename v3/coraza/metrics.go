// Package coraza includes lightweight metrics and lifecycle snapshots for Engine instances.
package coraza

import (
	"math"
	"sync/atomic"
	"time"
)

const metricsEWMAAlpha = 0.2

// MetricsCollector records lightweight request metrics for a Coraza Engine.
type MetricsCollector interface {
	ObserveRequest(duration time.Duration, blocked bool)
	GetMetrics() *MetricsSnapshot
	Reset()
}

// MetricsSnapshot represents the current request metrics for an Engine.
type MetricsSnapshot struct {
	// TotalRequests is the number of requests observed by the middleware.
	TotalRequests uint64 `json:"total_requests"`
	// BlockedRequests is the number of requests interrupted by the WAF.
	BlockedRequests uint64 `json:"blocked_requests"`
	// BlockRate is the cumulative ratio of blocked requests to total requests.
	BlockRate float64 `json:"block_rate"`
	// RecentLatencyMs is the EWMA of recent middleware latency in milliseconds.
	RecentLatencyMs float64 `json:"recent_latency_ms"`
	// RecentBlockRate is the EWMA of recent blocked-request outcomes.
	RecentBlockRate float64 `json:"recent_block_rate"`
	// Timestamp is when the snapshot was generated.
	Timestamp time.Time `json:"timestamp"`
}

// EngineSnapshot represents lifecycle and configuration state for an Engine.
type EngineSnapshot struct {
	// Initialized reports whether the Engine currently holds a usable WAF instance.
	Initialized bool `json:"initialized"`
	// SupportsOptions reports whether the current WAF supports Coraza experimental options.
	SupportsOptions bool `json:"supports_options"`
	// ConfigFiles lists the directive files for the active configuration.
	ConfigFiles []string `json:"config_files"`
	// LastAttemptConfigFiles lists the directive files from the most recent init attempt.
	LastAttemptConfigFiles []string `json:"last_attempt_config_files"`
	// LastInitError contains the most recent initialization error, if any.
	LastInitError string `json:"last_init_error,omitempty"`
	// LastLoadedAt is the timestamp of the most recent successful initialization or reload.
	LastLoadedAt time.Time `json:"last_loaded_at"`
	// InitSuccessTotal is the number of successful init calls.
	InitSuccessTotal uint64 `json:"init_success_total"`
	// InitFailureTotal is the number of failed init calls.
	InitFailureTotal uint64 `json:"init_failure_total"`
	// ReloadSuccessTotal is the number of successful reload calls.
	ReloadSuccessTotal uint64 `json:"reload_success_total"`
	// ReloadFailureTotal is the number of failed reload calls.
	ReloadFailureTotal uint64 `json:"reload_failure_total"`
	// ReloadCount is the total number of successful reload transitions.
	ReloadCount uint64 `json:"reload_count"`
}

// MetricsReport combines request metrics with Engine lifecycle information.
type MetricsReport struct {
	// Requests is the request metrics snapshot.
	Requests MetricsSnapshot `json:"requests"`
	// Engine is the Engine lifecycle snapshot.
	Engine EngineSnapshot `json:"engine"`
}

type defaultMetricsCollector struct {
	totalRequests   atomic.Uint64
	blockedRequests atomic.Uint64

	recentLatencyBits        atomic.Uint64
	recentBlockRateBits      atomic.Uint64
	recentLatencyInitialized atomic.Uint32
	recentBlockInitialized   atomic.Uint32
}

// NewDefaultMetricsCollector creates the built-in in-memory metrics collector.
func NewDefaultMetricsCollector() MetricsCollector {
	return &defaultMetricsCollector{}
}

func (m *defaultMetricsCollector) ObserveRequest(duration time.Duration, blocked bool) {
	m.totalRequests.Add(1)
	if blocked {
		m.blockedRequests.Add(1)
	}

	m.updateRecentBlockRate(blocked)
	if duration >= 0 {
		m.updateRecentLatency(duration)
	}
}

func (m *defaultMetricsCollector) GetMetrics() *MetricsSnapshot {
	totalReqs := m.totalRequests.Load()
	blockedReqs := m.blockedRequests.Load()

	var blockRate float64
	if totalReqs > 0 {
		blockRate = float64(blockedReqs) / float64(totalReqs)
	}

	return &MetricsSnapshot{
		TotalRequests:   totalReqs,
		BlockedRequests: blockedReqs,
		BlockRate:       blockRate,
		RecentLatencyMs: m.loadRecentLatencyMs(),
		RecentBlockRate: m.loadRecentBlockRate(),
		Timestamp:       time.Now(),
	}
}

func (m *defaultMetricsCollector) Reset() {
	m.totalRequests.Store(0)
	m.blockedRequests.Store(0)
	m.recentLatencyBits.Store(0)
	m.recentBlockRateBits.Store(0)
	m.recentLatencyInitialized.Store(0)
	m.recentBlockInitialized.Store(0)
}

func (m *defaultMetricsCollector) updateRecentLatency(duration time.Duration) {
	sample := float64(duration.Nanoseconds()) / 1e6
	m.updateEWMA(sample, &m.recentLatencyBits, &m.recentLatencyInitialized)
}

func (m *defaultMetricsCollector) updateRecentBlockRate(blocked bool) {
	sample := 0.0
	if blocked {
		sample = 1.0
	}
	m.updateEWMA(sample, &m.recentBlockRateBits, &m.recentBlockInitialized)
}

func (m *defaultMetricsCollector) updateEWMA(sample float64, bits *atomic.Uint64, initialized *atomic.Uint32) {
	if initialized.Load() == 0 {
		if initialized.CompareAndSwap(0, 1) {
			bits.Store(math.Float64bits(sample))
			return
		}
	}

	for {
		currentBits := bits.Load()
		current := math.Float64frombits(currentBits)
		next := current + metricsEWMAAlpha*(sample-current)
		if bits.CompareAndSwap(currentBits, math.Float64bits(next)) {
			return
		}
	}
}

func (m *defaultMetricsCollector) loadRecentLatencyMs() float64 {
	if m.recentLatencyInitialized.Load() == 0 {
		return 0
	}

	return math.Float64frombits(m.recentLatencyBits.Load())
}

func (m *defaultMetricsCollector) loadRecentBlockRate() float64 {
	if m.recentBlockInitialized.Load() == 0 {
		return 0
	}

	return math.Float64frombits(m.recentBlockRateBits.Load())
}

// MetricsSnapshot returns a copy of the Engine's current request metrics.
func (e *Engine) MetricsSnapshot() MetricsSnapshot {
	collector := e.Metrics()
	if collector == nil {
		return MetricsSnapshot{Timestamp: time.Now()}
	}

	snapshot := collector.GetMetrics()
	if snapshot == nil {
		return MetricsSnapshot{Timestamp: time.Now()}
	}

	return *snapshot
}

// Snapshot returns lifecycle and configuration state for the Engine.
func (e *Engine) Snapshot() EngineSnapshot {
	return e.observabilitySnapshot()
}

// Report returns both the request metrics and lifecycle snapshot for the Engine.
func (e *Engine) Report() MetricsReport {
	return MetricsReport{
		Requests: e.MetricsSnapshot(),
		Engine:   e.Snapshot(),
	}
}

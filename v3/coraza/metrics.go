// Package coraza includes lightweight metrics and lifecycle snapshots for Engine instances.
package coraza

import (
	"sync"
	"sync/atomic"
	"time"
)

// MetricsCollector records lightweight request metrics for a Coraza Engine.
type MetricsCollector interface {
	RecordRequest()
	RecordBlock()
	RecordLatency(duration time.Duration)
	GetMetrics() *MetricsSnapshot
	Reset()
}

// MetricsSnapshot represents the current request metrics for an Engine.
type MetricsSnapshot struct {
	// TotalRequests is the number of requests observed by the middleware.
	TotalRequests uint64 `json:"total_requests"`
	// BlockedRequests is the number of requests interrupted by the WAF.
	BlockedRequests uint64 `json:"blocked_requests"`
	// AvgLatencyMs is the average middleware latency in milliseconds.
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	// BlockRate is the ratio of blocked requests to total requests.
	BlockRate float64 `json:"block_rate"`
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

	latencyMutex sync.RWMutex
	latencySum   time.Duration
	latencyCount uint64
}

// NewDefaultMetricsCollector creates the built-in in-memory metrics collector.
func NewDefaultMetricsCollector() MetricsCollector {
	return &defaultMetricsCollector{}
}

func (m *defaultMetricsCollector) RecordRequest() {
	m.totalRequests.Add(1)
}

func (m *defaultMetricsCollector) RecordBlock() {
	m.blockedRequests.Add(1)
}

func (m *defaultMetricsCollector) RecordLatency(duration time.Duration) {
	m.latencyMutex.Lock()
	defer m.latencyMutex.Unlock()

	m.latencySum += duration
	m.latencyCount++
}

func (m *defaultMetricsCollector) GetMetrics() *MetricsSnapshot {
	totalReqs := m.totalRequests.Load()
	blockedReqs := m.blockedRequests.Load()

	m.latencyMutex.RLock()
	var avgLatencyMs float64
	if m.latencyCount > 0 {
		avgLatencyMs = float64(m.latencySum.Nanoseconds()) / float64(m.latencyCount) / 1e6
	}
	m.latencyMutex.RUnlock()

	var blockRate float64
	if totalReqs > 0 {
		blockRate = float64(blockedReqs) / float64(totalReqs)
	}

	return &MetricsSnapshot{
		TotalRequests:   totalReqs,
		BlockedRequests: blockedReqs,
		AvgLatencyMs:    avgLatencyMs,
		BlockRate:       blockRate,
		Timestamp:       time.Now(),
	}
}

func (m *defaultMetricsCollector) Reset() {
	m.totalRequests.Store(0)
	m.blockedRequests.Store(0)

	m.latencyMutex.Lock()
	defer m.latencyMutex.Unlock()
	m.latencySum = 0
	m.latencyCount = 0
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

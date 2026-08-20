package stats

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotCacheTTL(t *testing.T) {
	m, err := newMiddleware(Config{Refresh: time.Second})
	mustNoError(t, err)
	var calls atomic.Int64
	m.collectFn = func(now time.Time) snapshot {
		calls.Add(1)
		return snapshot{CollectedAt: now}
	}

	started := time.Unix(100, 0)
	first := m.currentSnapshot(started)
	second := m.currentSnapshot(started.Add(999 * time.Millisecond))
	third := m.currentSnapshot(started.Add(time.Second))
	mustEqual(t, first.CollectedAt, second.CollectedAt)
	mustNotEqual(t, first.CollectedAt, third.CollectedAt)
	mustEqual(t, int64(2), calls.Load())
}

func TestConcurrentCacheMissCollectsOnce(t *testing.T) {
	m, err := newMiddleware()
	mustNoError(t, err)
	var calls atomic.Int64
	m.collectFn = func(now time.Time) snapshot {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return snapshot{CollectedAt: now}
	}

	const workers = 64
	var group sync.WaitGroup
	now := time.Unix(100, 0)
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			_ = m.currentSnapshot(now)
		}()
	}
	group.Wait()
	mustEqual(t, int64(1), calls.Load())
}

func TestFirstAndSecondSnapshotWindowMetrics(t *testing.T) {
	m, err := newMiddleware()
	mustNoError(t, err)
	now := time.Now()
	m.requests.Add(10)
	m.status2.Add(10)
	m.latency.observeSharded(uint64(time.Millisecond), 0)

	first := m.collectSnapshot(now)
	mustNil(t, first.HTTP.RPS)
	mustNil(t, first.HTTP.Rates.Status4xx)
	mustNil(t, first.HTTP.Rates.Status5xx)
	mustNil(t, first.HTTP.Latency.P50NS)
	mustNil(t, first.Process.CPUPercent)
	mustNil(t, first.System.CPUPercent)
	mustNil(t, first.System.NetworkReceiveBPS)

	m.requests.Add(4)
	m.status2.Add(3)
	m.status4.Add(1)
	m.latency.observeSharded(uint64(time.Millisecond), 1)
	second := m.collectSnapshot(now.Add(2 * time.Second))
	mustNotNil(t, second.HTTP.RPS)
	mustInDelta(t, 2, *second.HTTP.RPS, 0.001)
	mustNotNil(t, second.HTTP.Rates.Status4xx)
	mustInDelta(t, 0.25, *second.HTTP.Rates.Status4xx, 0.001)
	mustNotNil(t, second.HTTP.Latency.P50NS)
}

func TestCacheHitDoesNotResetHistogram(t *testing.T) {
	m, err := newMiddleware(Config{Refresh: time.Second})
	mustNoError(t, err)
	now := time.Now()
	_ = m.currentSnapshot(now)
	m.latency.observeSharded(uint64(time.Millisecond), 0)
	_ = m.currentSnapshot(now.Add(500 * time.Millisecond))
	mustEqual(t, uint64(1), m.latency.snapshotAndReset().count)
}

func TestMiddlewareInstancesAreIsolated(t *testing.T) {
	first, err := newMiddleware()
	mustNoError(t, err)
	second, err := newMiddleware()
	mustNoError(t, err)
	first.requests.Add(3)
	first.status2.Add(3)

	firstSnapshot := first.collectSnapshot(time.Now())
	secondSnapshot := second.collectSnapshot(time.Now())
	mustEqual(t, uint64(3), firstSnapshot.HTTP.Requests)
	mustZero(t, secondSnapshot.HTTP.Requests)
}

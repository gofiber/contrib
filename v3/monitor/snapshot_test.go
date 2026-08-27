package monitor

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotCacheTTL(t *testing.T) {
	m, err := newMiddleware(Config{Refresh: time.Second})
	require.NoError(t, err)
	var calls atomic.Int64
	m.collectFn = func(now time.Time) snapshot {
		calls.Add(1)
		return snapshot{CollectedAt: now}
	}

	started := time.Unix(100, 0)
	first := m.currentSnapshot(started)
	second := m.currentSnapshot(started.Add(999 * time.Millisecond))
	third := m.currentSnapshot(started.Add(time.Second))
	assert.Equal(t, first.CollectedAt, second.CollectedAt)
	assert.NotEqual(t, first.CollectedAt, third.CollectedAt)
	assert.Equal(t, int64(2), calls.Load())
}

func TestConcurrentCacheMissCollectsOnce(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
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
	assert.Equal(t, int64(1), calls.Load())
}

func TestNewerCacheWinsWhenCallerTimestampIsOlder(t *testing.T) {
	m, err := newMiddleware()
	require.NoError(t, err)
	var calls atomic.Int64
	m.collectFn = func(now time.Time) snapshot {
		calls.Add(1)
		return snapshot{CollectedAt: now}
	}

	newer := time.Unix(101, 0)
	m.cache.Store(&cacheEntry{
		snapshot: snapshot{CollectedAt: newer},
		cachedAt: newer,
	})

	current := m.currentSnapshot(time.Unix(100, 0))
	assert.Equal(t, newer, current.CollectedAt)
	assert.Zero(t, calls.Load())
	assert.Equal(t, newer, m.cache.Load().cachedAt)
}

func TestFirstAndSecondSnapshotWindowMetrics(t *testing.T) {
	m, err := newMiddleware(Config{EnableGCPauseMetrics: true})
	require.NoError(t, err)
	now := time.Now()
	m.requests.Add(10)
	m.status1.Add(2)
	m.status2.Add(10)
	m.latency.observeSharded(uint64(time.Millisecond), 0)

	first := m.collectSnapshot(now)
	assert.Nil(t, first.HTTP.RPS)
	assert.Nil(t, first.HTTP.Rates.Status4xx)
	assert.Nil(t, first.HTTP.Rates.Status5xx)
	assert.Nil(t, first.HTTP.Latency.P50NS)
	assert.Equal(t, uint64(2), first.HTTP.Status.Status1xx)
	assert.Nil(t, first.Runtime.GCPauseWindowNS)
	assert.Nil(t, first.Process.CPUPercent)
	assert.Nil(t, first.System.CPUPercent)
	assert.Nil(t, first.System.NetworkReceiveBPS)

	m.requests.Add(4)
	m.status2.Add(3)
	m.status4.Add(1)
	m.latency.observeSharded(uint64(time.Millisecond), 1)
	second := m.collectSnapshot(now.Add(2 * time.Second))
	require.NotNil(t, second.HTTP.RPS)
	assert.InDelta(t, 2, *second.HTTP.RPS, 0.001)
	require.NotNil(t, second.HTTP.Rates.Status4xx)
	assert.InDelta(t, 0.25, *second.HTTP.Rates.Status4xx, 0.001)
	require.NotNil(t, second.HTTP.Latency.P50NS)
	require.NotNil(t, second.Runtime.GCPauseWindowNS)
}

func TestCacheHitDoesNotResetHistogram(t *testing.T) {
	m, err := newMiddleware(Config{Refresh: time.Second})
	require.NoError(t, err)
	now := time.Now()
	_ = m.currentSnapshot(now)
	m.latency.observeSharded(uint64(time.Millisecond), 0)
	_ = m.currentSnapshot(now.Add(500 * time.Millisecond))
	assert.Equal(t, uint64(1), m.latency.snapshotAndReset().count)
}

func TestMiddlewareInstancesAreIsolated(t *testing.T) {
	first, err := newMiddleware()
	require.NoError(t, err)
	second, err := newMiddleware()
	require.NoError(t, err)
	first.requests.Add(3)
	first.status2.Add(3)

	firstSnapshot := first.collectSnapshot(time.Now())
	secondSnapshot := second.collectSnapshot(time.Now())
	assert.Equal(t, uint64(3), firstSnapshot.HTTP.Requests)
	assert.Zero(t, secondSnapshot.HTTP.Requests)
}

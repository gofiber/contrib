package stats

import (
	"sync"
	"testing"
	"time"
)

func TestLatencyHistogramPercentilesAndReset(t *testing.T) {
	var histogram latencyHistogram
	values := []time.Duration{time.Millisecond, 2 * time.Millisecond, 7 * time.Millisecond, 30 * time.Millisecond, 6 * time.Second}
	for index, value := range values {
		histogram.observeSharded(uint64(value), uint64(index))
	}

	window := histogram.snapshotAndReset()
	p50, ok := window.percentile(50)
	mustTrue(t, ok)
	mustEqual(t, uint64(10*time.Millisecond), p50)
	p95, ok := window.percentile(95)
	mustTrue(t, ok)
	mustEqual(t, uint64(6*time.Second), p95)
	_, ok = histogram.snapshotAndReset().percentile(50)
	mustFalse(t, ok)
}

func TestLatencyHistogramConcurrentObserve(t *testing.T) {
	var histogram latencyHistogram
	const workers = 64
	const observations = 200
	var group sync.WaitGroup
	for worker := uint64(0); worker < workers; worker++ {
		group.Add(1)
		go func(shard uint64) {
			defer group.Done()
			for range observations {
				histogram.observeSharded(uint64(time.Millisecond), shard)
			}
		}(worker)
	}
	group.Wait()
	mustEqual(t, uint64(workers*observations), histogram.snapshotAndReset().count)
}

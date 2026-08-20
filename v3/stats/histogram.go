package stats

import (
	"sync/atomic"
	"time"
)

// latencyBoundsNS are inclusive upper bounds. The last bucket is also the
// reported saturation value for observations above 60 seconds; keeping
// percentiles bucket-only avoids a separately reset maximum crossing windows.
var latencyBoundsNS = [...]uint64{
	uint64(time.Millisecond),
	5 * uint64(time.Millisecond),
	10 * uint64(time.Millisecond),
	25 * uint64(time.Millisecond),
	50 * uint64(time.Millisecond),
	100 * uint64(time.Millisecond),
	250 * uint64(time.Millisecond),
	500 * uint64(time.Millisecond),
	uint64(time.Second),
	2500 * uint64(time.Millisecond),
	5 * uint64(time.Second),
	10 * uint64(time.Second),
	30 * uint64(time.Second),
	60 * uint64(time.Second),
}

const latencyHistogramShards = 32

// latencyHistogram shards counters by the monotonic request sequence to reduce
// cache-line contention without storing route, method, or other cardinality.
type latencyHistogram struct {
	buckets [latencyHistogramShards][len(latencyBoundsNS) + 1]atomic.Uint64
}

type latencyWindow struct {
	buckets [len(latencyBoundsNS) + 1]uint64
	count   uint64
}

func (h *latencyHistogram) observeSharded(ns, shard uint64) {
	bucket := len(latencyBoundsNS)
	for index, bound := range latencyBoundsNS {
		if ns <= bound {
			bucket = index
			break
		}
	}
	h.buckets[shard&(latencyHistogramShards-1)][bucket].Add(1)
}

// snapshotAndReset advances the latency window only when a fresh snapshot is
// collected. Atomic swaps avoid blocking request observation; a request racing
// the reset may land in either adjacent window, but its count is not lost.
func (h *latencyHistogram) snapshotAndReset() latencyWindow {
	var window latencyWindow
	for shard := range h.buckets {
		for bucket := range h.buckets[shard] {
			count := h.buckets[shard][bucket].Swap(0)
			window.buckets[bucket] += count
			window.count += count
		}
	}
	return window
}

// percentile returns the containing bucket's upper bound rather than an
// interpolated duration. Values in the overflow bucket saturate at the final
// configured bound.
func (w latencyWindow) percentile(percent uint64) (uint64, bool) {
	if w.count == 0 || percent == 0 {
		return 0, false
	}

	rank := (w.count*percent + 99) / 100
	var cumulative uint64
	for index, count := range w.buckets {
		cumulative += count
		if cumulative < rank {
			continue
		}
		if index < len(latencyBoundsNS) {
			return latencyBoundsNS[index], true
		}
		return latencyBoundsNS[len(latencyBoundsNS)-1], true
	}
	return latencyBoundsNS[len(latencyBoundsNS)-1], true
}

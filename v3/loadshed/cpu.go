package loadshed

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/cpu"
)

// LoadCriteria interface for different types of load metrics.
type LoadCriteria interface {
	Metric(ctx context.Context) (float64, error)
	ShouldShed(metric float64) bool
}

// CPULoadCriteria for using CPU as a load metric.
type CPULoadCriteria struct {
	LowerThreshold float64
	UpperThreshold float64
	Interval       time.Duration
	Getter         CPUPercentGetter

	once   sync.Once
	cached atomic.Uint64
	cancel context.CancelFunc
}

func (c *CPULoadCriteria) startSampler() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel

	interval := c.Interval
	if interval <= 0 {
		interval = time.Second
	}

	go func() {
		for {
			start := time.Now()

			percentages, err := c.Getter.PercentWithContext(ctx, interval, false)
			if err == nil && len(percentages) > 0 {
				c.cached.Store(math.Float64bits(percentages[0]))
			} else {
				// Fail open on sampling errors or empty results: treat CPU as
				// idle so the middleware never sheds based on stale high values.
				c.cached.Store(math.Float64bits(0))
			}

			// Ensure we never busy-spin even if the getter returns instantly.
			elapsed := time.Since(start)
			if elapsed < interval {
				select {
				case <-ctx.Done():
					return
				case <-time.After(interval - elapsed):
				}
			} else {
				// Check for stop between iterations.
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}
	}()
}

// Stop terminates the background CPU sampler goroutine.
// It is safe to call Stop concurrently and multiple times (context.CancelFunc
// is idempotent). Stop also interrupts any in-progress CPU sampling call,
// so shutdown is responsive regardless of the configured Interval.
// If called before the sampler has started, no goroutine is ever launched.
func (c *CPULoadCriteria) Stop() {
	c.once.Do(func() {
		// If Stop is called before Metric/New, create a cancel func without
		// starting the sampler goroutine. sync.Once guarantees that either
		// this func or startSampler runs, never both.
		_, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
	})
	c.cancel()
}

// Metric returns the most recently sampled CPU usage percentage.
// On the first call it starts a background goroutine that continuously
// samples CPU usage at the configured Interval, so individual requests
// are never blocked waiting for a CPU measurement.
// Before the first sample completes, it returns 0 (allowing requests through).
// On sampling errors the cached value is reset to 0, preserving fail-open
// behaviour: ShouldShed(0) is always false, so requests are allowed through.
func (c *CPULoadCriteria) Metric(_ context.Context) (float64, error) {
	c.once.Do(c.startSampler)
	return math.Float64frombits(c.cached.Load()), nil
}

func (c *CPULoadCriteria) ShouldShed(metric float64) bool {
	if metric > c.UpperThreshold*100 {
		return true
	} else if metric > c.LowerThreshold*100 {
		rejectionProbability := (metric - c.LowerThreshold*100) / (c.UpperThreshold - c.LowerThreshold)
		// #nosec G404
		return rand.Float64()*100 < rejectionProbability
	}
	return false
}

type CPUPercentGetter interface {
	PercentWithContext(ctx context.Context, interval time.Duration, percpu bool) ([]float64, error)
}

type DefaultCPUPercentGetter struct{}

func (*DefaultCPUPercentGetter) PercentWithContext(ctx context.Context, interval time.Duration, percpu bool) ([]float64, error) {
	return cpu.PercentWithContext(ctx, interval, percpu)
}

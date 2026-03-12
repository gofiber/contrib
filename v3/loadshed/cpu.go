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

// minSamplerSleep is the minimum pause between sampler iterations to prevent
// busy-spin when a custom getter returns instantly or the interval is tiny.
const minSamplerSleep = 100 * time.Millisecond

func (c *CPULoadCriteria) startSampler() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel

	interval := c.Interval
	if interval <= 0 {
		interval = time.Second
	}

	go func() {
		timer := time.NewTimer(interval)
		defer timer.Stop()

		for {
			start := time.Now()

			c.sample(ctx, interval)

			// Calculate how long to sleep before the next sample.
			elapsed := time.Since(start)
			sleep := interval - elapsed
			if sleep < minSamplerSleep {
				sleep = minSamplerSleep
			}

			timer.Reset(sleep)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
	}()
}

// sample performs a single CPU measurement with panic recovery.
// If the getter panics, it recovers and fails open (cached → 0).
func (c *CPULoadCriteria) sample(ctx context.Context, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			// Fail open: treat CPU as idle so the middleware never sheds
			// based on a panicking getter.
			c.cached.Store(math.Float64bits(0))
		}
	}()

	percentages, err := c.Getter.PercentWithContext(ctx, interval, false)
	if err == nil && len(percentages) > 0 {
		c.cached.Store(math.Float64bits(percentages[0]))
	} else {
		// Fail open on sampling errors or empty results: treat CPU as
		// idle so the middleware never sheds based on stale high values.
		c.cached.Store(math.Float64bits(0))
	}
}

// Stop terminates the background CPU sampler goroutine.
// It is safe to call Stop concurrently and multiple times (context.CancelFunc
// is idempotent). Stop also interrupts any in-progress CPU sampling call,
// so shutdown is responsive regardless of the configured Interval.
// If called before the sampler has started, no goroutine is ever launched.
func (c *CPULoadCriteria) Stop() {
	c.once.Do(func() {
		// If Stop is called before Metric/New, set a no-op cancel without
		// starting the sampler goroutine. sync.Once guarantees that either
		// this func or startSampler runs, never both.
		c.cancel = func() {}
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
func (c *CPULoadCriteria) Metric(ctx context.Context) (float64, error) {
	c.once.Do(c.startSampler)
	if err := ctx.Err(); err != nil {
		// Fail open: return a zero metric so requests are allowed through,
		// but surface the context error for observability.
		return 0, err
	}
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

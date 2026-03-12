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
	done   chan struct{}
}

func (c *CPULoadCriteria) startSampler() {
	c.done = make(chan struct{})

	interval := c.Interval
	if interval <= 0 {
		interval = time.Second
	}

	go func() {
		for {
			start := time.Now()

			percentages, err := c.Getter.PercentWithContext(context.Background(), interval, false)
			if err == nil && len(percentages) > 0 {
				c.cached.Store(math.Float64bits(percentages[0]))
			}

			// Ensure we never busy-spin even if the getter returns instantly.
			elapsed := time.Since(start)
			if elapsed < interval {
				select {
				case <-c.done:
					return
				case <-time.After(interval - elapsed):
				}
			} else {
				// Check for stop between iterations.
				select {
				case <-c.done:
					return
				default:
				}
			}
		}
	}()
}

// Stop terminates the background CPU sampler goroutine.
// It is safe to call Stop multiple times or before the sampler has started.
func (c *CPULoadCriteria) Stop() {
	if c.done != nil {
		select {
		case <-c.done:
			// already closed
		default:
			close(c.done)
		}
	}
}

// Metric returns the most recently sampled CPU usage percentage.
// On the first call it starts a background goroutine that continuously
// samples CPU usage at the configured Interval, so individual requests
// are never blocked waiting for a CPU measurement.
// Before the first sample completes, it returns 0 (allowing requests through).
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

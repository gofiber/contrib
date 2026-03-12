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
}

func (c *CPULoadCriteria) startSampler() {
	// The sampler goroutine runs for the lifetime of the server.
	// This is intentional: it continuously updates the cached CPU metric
	// so that Metric() can return immediately without blocking.
	go func() {
		for {
			percentages, err := c.Getter.PercentWithContext(context.Background(), c.Interval, false)
			if err == nil && len(percentages) > 0 {
				c.cached.Store(math.Float64bits(percentages[0]))
			}
		}
	}()
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

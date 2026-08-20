package stats

import (
	"os"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gopsnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type collector struct {
	proc      *process.Process
	numCPU    int
	startedAt time.Time

	processCPUSeen  bool
	processCPUTime  float64
	processCPUAt    time.Time
	systemCPUSeen   bool
	systemCPUTimes  cpu.TimesStat
	networkSeen     bool
	networkReceived uint64
	networkSent     uint64
	networkAt       time.Time

	httpSeen      bool
	httpAt        time.Time
	lastRequests  uint64
	lastCompleted uint64
	lastStatus4xx uint64
	lastStatus5xx uint64
}

func newCollector(now time.Time) collector {
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		proc = nil
	}
	numCPU := runtime.NumCPU()
	if numCPU < 1 {
		numCPU = 1
	}
	return collector{proc: proc, numCPU: numCPU, startedAt: now}
}

func (c *collector) collect(m *middleware, now time.Time) snapshot {
	errors := make([]string, 0)
	processValues, processErrors := c.collectProcess(now)
	errors = appendCollectionErrors(errors, processErrors...)
	systemValues, systemErrors := c.collectSystem(now)
	errors = appendCollectionErrors(errors, systemErrors...)

	return snapshot{
		CollectedAt: now.UTC(),
		Collection: collectionStats{
			Partial: len(errors) > 0,
			Errors:  errors,
		},
		Process: processValues,
		Runtime: collectRuntime(),
		System:  systemValues,
		HTTP:    c.collectHTTP(m, now),
	}
}

func (c *collector) collectProcess(now time.Time) (processStats, []string) {
	uptime := now.Sub(c.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	stats := processStats{UptimeSeconds: uint64(uptime / time.Second)}
	if c.proc == nil {
		return stats, []string{"process.cpu", "process.memory", "process.threads", "process.descriptors"}
	}

	errors := make([]string, 0)
	if times, err := c.proc.Times(); err == nil && times != nil {
		currentCPUTime := times.User + times.System
		if c.processCPUSeen {
			if value, ok := processCPUPercent(c.processCPUTime, currentCPUTime, now.Sub(c.processCPUAt), c.numCPU); ok {
				stats.CPUPercent = valuePointer(value)
			}
		}
		c.processCPUSeen = true
		c.processCPUTime = currentCPUTime
		c.processCPUAt = now
	} else {
		errors = append(errors, "process.cpu")
	}
	if info, err := c.proc.MemoryInfo(); err == nil && info != nil {
		stats.RSSBytes = valuePointer(info.RSS)
	} else {
		errors = append(errors, "process.memory")
	}
	if threads, err := c.proc.NumThreads(); err == nil {
		stats.Threads = valuePointer(threads)
	} else {
		errors = append(errors, "process.threads")
	}
	if descriptors, err := c.proc.NumFDs(); err == nil {
		stats.OpenDescriptors = valuePointer(descriptors)
	} else {
		errors = append(errors, "process.descriptors")
	}
	return stats, errors
}

func collectRuntime() runtimeStats {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return runtimeStats{
		Goroutines:     runtime.NumGoroutine(),
		HeapAllocBytes: memory.HeapAlloc,
		HeapObjects:    memory.HeapObjects,
		GCCount:        memory.NumGC,
		GCPauseLastNS:  lastGCPauseNS(memory),
	}
}

func lastGCPauseNS(memory runtime.MemStats) uint64 {
	if memory.NumGC == 0 {
		return 0
	}
	index := (memory.NumGC + uint32(len(memory.PauseNs)) - 1) % uint32(len(memory.PauseNs))
	return memory.PauseNs[index]
}

func (c *collector) collectSystem(now time.Time) (systemStats, []string) {
	var stats systemStats
	errors := make([]string, 0)

	if values, err := cpu.Times(false); err == nil && len(values) > 0 {
		current := values[0]
		if c.systemCPUSeen {
			if value, ok := systemCPUPercent(c.systemCPUTimes, current); ok {
				stats.CPUPercent = valuePointer(value)
			}
		}
		c.systemCPUSeen = true
		c.systemCPUTimes = current
	} else {
		errors = append(errors, "system.cpu")
	}
	if memory, err := mem.VirtualMemory(); err == nil && memory != nil {
		stats.MemoryUsedPercent = valuePointer(memory.UsedPercent)
		stats.MemoryUsedBytes = valuePointer(memory.Used)
		stats.MemoryTotalBytes = valuePointer(memory.Total)
	} else {
		errors = append(errors, "system.memory")
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		if usage, usageErr := disk.Usage(workingDirectory); usageErr == nil && usage != nil {
			stats.DiskUsedPercent = valuePointer(usage.UsedPercent)
			stats.DiskUsedBytes = valuePointer(usage.Used)
			stats.DiskTotalBytes = valuePointer(usage.Total)
		} else {
			errors = append(errors, "system.disk")
		}
	} else {
		errors = append(errors, "system.disk")
	}
	if runtime.GOOS != "windows" {
		if average, err := load.Avg(); err == nil && average != nil {
			stats.Load1 = valuePointer(average.Load1)
		} else {
			errors = append(errors, "system.load")
		}
	}
	if values, err := gopsnet.IOCounters(false); err == nil && len(values) > 0 {
		current := values[0]
		if c.networkSeen {
			stats.NetworkReceiveBPS, stats.NetworkSendBPS = networkRates(
				c.networkReceived,
				c.networkSent,
				current.BytesRecv,
				current.BytesSent,
				now.Sub(c.networkAt),
			)
		}
		c.networkSeen = true
		c.networkReceived = current.BytesRecv
		c.networkSent = current.BytesSent
		c.networkAt = now
	} else {
		errors = append(errors, "system.network")
	}
	return stats, errors
}

func (c *collector) collectHTTP(m *middleware, now time.Time) httpStats {
	status := httpStatusStats{
		Status2xx: m.status2.Load(),
		Status3xx: m.status3.Load(),
		Status4xx: m.status4.Load(),
		Status5xx: m.status5.Load(),
	}
	status1xx := m.status1.Load()
	completed := status1xx + status.Status2xx + status.Status3xx + status.Status4xx + status.Status5xx
	requests := m.requests.Load()
	window := m.latency.snapshotAndReset()

	stats := httpStats{
		Requests: requests,
		InFlight: m.inFlight.Load(),
		Status:   status,
	}
	if c.httpSeen {
		elapsed := now.Sub(c.httpAt)
		if elapsed > 0 {
			rps := float64(counterDelta(requests, c.lastRequests)) / elapsed.Seconds()
			stats.RPS = valuePointer(rps)
		}
		completedDelta := counterDelta(completed, c.lastCompleted)
		if completedDelta > 0 {
			status4xxRate := float64(counterDelta(status.Status4xx, c.lastStatus4xx)) / float64(completedDelta)
			status5xxRate := float64(counterDelta(status.Status5xx, c.lastStatus5xx)) / float64(completedDelta)
			stats.Rates.Status4xx = valuePointer(status4xxRate)
			stats.Rates.Status5xx = valuePointer(status5xxRate)
		}
		if p50, ok := window.percentile(50); ok {
			stats.Latency.P50NS = valuePointer(p50)
		}
		if p95, ok := window.percentile(95); ok {
			stats.Latency.P95NS = valuePointer(p95)
		}
		if p99, ok := window.percentile(99); ok {
			stats.Latency.P99NS = valuePointer(p99)
		}
	}

	c.httpSeen = true
	c.httpAt = now
	c.lastRequests = requests
	c.lastCompleted = completed
	c.lastStatus4xx = status.Status4xx
	c.lastStatus5xx = status.Status5xx
	return stats
}

func appendCollectionErrors(existing []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		seen := false
		for _, current := range existing {
			if current == value {
				seen = true
				break
			}
		}
		if !seen {
			existing = append(existing, value)
		}
	}
	return existing
}

func processCPUPercent(previous, current float64, elapsed time.Duration, numCPU int) (float64, bool) {
	if current < previous || elapsed <= 0 || numCPU < 1 {
		return 0, false
	}
	percent := (current - previous) / elapsed.Seconds() / float64(numCPU) * 100
	return clampPercent(percent), true
}

func systemCPUPercent(previous, current cpu.TimesStat) (float64, bool) {
	previousTotal, previousBusy := cpuTotalAndBusy(previous)
	currentTotal, currentBusy := cpuTotalAndBusy(current)
	totalDelta := currentTotal - previousTotal
	busyDelta := currentBusy - previousBusy
	if totalDelta <= 0 || busyDelta < 0 {
		return 0, false
	}
	return clampPercent(busyDelta / totalDelta * 100), true
}

func cpuTotalAndBusy(value cpu.TimesStat) (float64, float64) {
	total := value.User + value.System + value.Idle + value.Nice + value.Iowait + value.Irq + value.Softirq + value.Steal + value.Guest + value.GuestNice
	if runtime.GOOS == "linux" {
		total -= value.Guest + value.GuestNice
	}
	return total, total - value.Idle - value.Iowait
}

func networkRates(previousReceived, previousSent, currentReceived, currentSent uint64, elapsed time.Duration) (*float64, *float64) {
	if elapsed <= 0 || currentReceived < previousReceived || currentSent < previousSent {
		return nil, nil
	}
	receive := float64(currentReceived-previousReceived) / elapsed.Seconds()
	send := float64(currentSent-previousSent) / elapsed.Seconds()
	return valuePointer(receive), valuePointer(send)
}

func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

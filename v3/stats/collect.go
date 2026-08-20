package stats

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	runtimemetrics "runtime/metrics"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gopsnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

const (
	runtimeMetricHeapObjectsBytes = iota
	runtimeMetricHeapFreeBytes
	runtimeMetricHeapReleasedBytes
	runtimeMetricHeapUnusedBytes
	runtimeMetricHeapObjects
	runtimeMetricHeapGoalBytes
	runtimeMetricHeapAllocs
	runtimeMetricHeapFrees
	runtimeMetricHeapTinyAllocs
	runtimeMetricGCCycles
	runtimeMetricGCCPUSeconds
	runtimeMetricTotalCPUSeconds
	runtimeMetricCount
)

// runtimeMetricNames shares the index constants above with runtimeSamples.
// Keep their order aligned when adding a metric; runtime/metrics reports an
// unknown name as KindBad, which the scalar helpers degrade to zero.
var runtimeMetricNames = [...]string{
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/heap/unused:bytes",
	"/gc/heap/objects:objects",
	"/gc/heap/goal:bytes",
	"/gc/heap/allocs:objects",
	"/gc/heap/frees:objects",
	"/gc/heap/tiny/allocs:objects",
	"/gc/cycles/total:gc-cycles",
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/total:cpu-seconds",
}

// collector owns all cross-snapshot baselines. It is mutable and must only be
// called while middleware.collectMu is held; requests update the source HTTP
// counters independently through atomics.
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
	runtimeSamples  [runtimeMetricCount]runtimemetrics.Sample
	gcPauseEnabled  bool
	gcPauseSeen     bool
	gcPauseTotal    uint64

	httpSeen      bool
	httpAt        time.Time
	lastRequests  uint64
	lastCompleted uint64
	lastStatus4xx uint64
	lastStatus5xx uint64
}

func newCollector(now time.Time, enableGCPauseMetrics bool) collector {
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		proc = nil
	}
	numCPU := runtime.NumCPU()
	if numCPU < 1 {
		numCPU = 1
	}
	current := collector{
		proc:           proc,
		numCPU:         numCPU,
		startedAt:      now,
		gcPauseEnabled: enableGCPauseMetrics,
	}
	for index, name := range runtimeMetricNames {
		current.runtimeSamples[index].Name = name
	}
	return current
}

// collect returns partial data with stable error identifiers. Raw operating
// system errors are not copied into snapshots, preventing disclosure of
// filesystem paths, device names, or platform-specific details.
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
		Runtime: c.collectRuntime(),
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

// collectRuntime uses runtime/metrics for the default heap and GC counters.
// Exact pause totals remain an explicit opt-in because ReadMemStats takes a
// stop-the-world snapshot; the first enabled window only establishes a pause
// baseline and therefore remains null.
func (c *collector) collectRuntime() runtimeStats {
	runtimemetrics.Read(c.runtimeSamples[:])
	values := runtimeMetricValues{
		heapObjectsBytes:  runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapObjectsBytes),
		heapFreeBytes:     runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapFreeBytes),
		heapReleasedBytes: runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapReleasedBytes),
		heapUnusedBytes:   runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapUnusedBytes),
		heapObjects:       runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapObjects),
		heapGoalBytes:     runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapGoalBytes),
		heapAllocs:        runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapAllocs),
		heapFrees:         runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapFrees),
		heapTinyAllocs:    runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricHeapTinyAllocs),
		gcCycles:          runtimeMetricUint64(c.runtimeSamples[:], runtimeMetricGCCycles),
		gcCPUSeconds:      runtimeMetricFloat64(c.runtimeSamples[:], runtimeMetricGCCPUSeconds),
		totalCPUSeconds:   runtimeMetricFloat64(c.runtimeSamples[:], runtimeMetricTotalCPUSeconds),
	}
	stats := runtimeStatsFromMetricValues(values)
	stats.Goroutines = runtime.NumGoroutine()
	stats.GOMAXPROCS = runtime.GOMAXPROCS(0)
	stats.GCPauseMetricsEnabled = c.gcPauseEnabled
	if !c.gcPauseEnabled {
		return stats
	}

	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	applyGCPauseMetrics(&stats, memory, c.gcPauseSeen, c.gcPauseTotal)
	c.gcPauseSeen = true
	c.gcPauseTotal = memory.PauseTotalNs
	return stats
}

type runtimeMetricValues struct {
	heapObjectsBytes  uint64
	heapFreeBytes     uint64
	heapReleasedBytes uint64
	heapUnusedBytes   uint64
	heapObjects       uint64
	heapGoalBytes     uint64
	heapAllocs        uint64
	heapFrees         uint64
	heapTinyAllocs    uint64
	gcCycles          uint64
	gcCPUSeconds      float64
	totalCPUSeconds   float64
}

func runtimeStatsFromMetricValues(values runtimeMetricValues) runtimeStats {
	// These sums mirror runtime.MemStats: in-use spans contain objects plus
	// unused slots, while idle spans are free or already released to the OS.
	heapInuse := values.heapObjectsBytes + values.heapUnusedBytes
	heapIdle := values.heapFreeBytes + values.heapReleasedBytes
	stats := runtimeStats{
		HeapAllocBytes:    values.heapObjectsBytes,
		HeapSysBytes:      heapInuse + heapIdle,
		HeapInuseBytes:    heapInuse,
		HeapIdleBytes:     heapIdle,
		HeapReleasedBytes: values.heapReleasedBytes,
		HeapObjects:       values.heapObjects,
		NextGCBytes:       values.heapGoalBytes,
		Mallocs:           values.heapAllocs + values.heapTinyAllocs,
		Frees:             values.heapFrees + values.heapTinyAllocs,
		GCCount:           values.gcCycles,
	}
	if values.totalCPUSeconds > 0 && !math.IsNaN(values.gcCPUSeconds) && !math.IsNaN(values.totalCPUSeconds) {
		fraction := values.gcCPUSeconds / values.totalCPUSeconds
		if fraction < 0 {
			fraction = 0
		} else if fraction > 1 {
			fraction = 1
		}
		stats.GCCPUFraction = fraction
	}
	return stats
}

func applyGCPauseMetrics(stats *runtimeStats, memory runtime.MemStats, pauseSeen bool, previousPauseTotal uint64) {
	stats.GCPauseLastNS = valuePointer(lastGCPauseNS(memory))
	stats.GCPauseTotalNS = valuePointer(memory.PauseTotalNs)
	if pauseSeen && memory.PauseTotalNs >= previousPauseTotal {
		stats.GCPauseWindowNS = valuePointer(memory.PauseTotalNs - previousPauseTotal)
	}
}

func runtimeMetricUint64(samples []runtimemetrics.Sample, index int) uint64 {
	if samples[index].Value.Kind() != runtimemetrics.KindUint64 {
		return 0
	}
	return samples[index].Value.Uint64()
}

func runtimeMetricFloat64(samples []runtimemetrics.Sample, index int) float64 {
	if samples[index].Value.Kind() != runtimemetrics.KindFloat64 {
		return 0
	}
	return samples[index].Value.Float64()
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
		stats.MemoryAvailableBytes = valuePointer(memory.Available)
	} else {
		errors = append(errors, "system.memory")
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		if usage, usageErr := disk.Usage(workingDirectory); usageErr == nil && usage != nil {
			stats.DiskUsedPercent = valuePointer(usage.UsedPercent)
			stats.DiskUsedBytes = valuePointer(usage.Used)
			stats.DiskTotalBytes = valuePointer(usage.Total)
			stats.DiskFreeBytes = valuePointer(usage.Free)
			filesystemType := usage.Fstype
			if filesystemType == "" {
				// Some platforms return usable partitions together with non-fatal
				// warnings (for example, an unavailable removable drive on Windows).
				// Keep the safe filesystem metadata and ignore those warnings here.
				partitions, _ := disk.Partitions(false)
				filesystemType = filesystemTypeForPath(workingDirectory, partitions)
			}
			if filesystemType != "" {
				stats.DiskFSType = valuePointer(filesystemType)
			}
		} else {
			errors = append(errors, "system.disk")
		}
	} else {
		errors = append(errors, "system.disk")
	}
	// gopsutil's Windows load implementation starts its own sampling goroutine.
	// Stats has no background collectors, so load averages are unsupported there
	// rather than silently changing the middleware lifecycle.
	if runtime.GOOS != "windows" {
		if average, err := load.Avg(); err == nil && average != nil {
			stats.Load1 = valuePointer(average.Load1)
			stats.Load5 = valuePointer(average.Load5)
			stats.Load15 = valuePointer(average.Load15)
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

// filesystemTypeForPath selects the most specific containing mount. Only the
// filesystem type leaves the collector; mountpoints and devices are never
// copied into the snapshot.
func filesystemTypeForPath(path string, partitions []disk.PartitionStat) string {
	bestLength := -1
	filesystemType := ""
	for _, partition := range partitions {
		if partition.Mountpoint == "" || partition.Fstype == "" {
			continue
		}
		mountpoint := filepath.Clean(partition.Mountpoint)
		if volume := filepath.VolumeName(partition.Mountpoint); volume != "" && partition.Mountpoint == volume {
			mountpoint = volume + string(os.PathSeparator)
		}
		relative, err := filepath.Rel(mountpoint, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			continue
		}
		if len(mountpoint) > bestLength {
			bestLength = len(mountpoint)
			filesystemType = partition.Fstype
		}
	}
	return filesystemType
}

// collectHTTP advances every window baseline together. It is called only for a
// real cache refresh, so cache hits neither reset latency buckets nor shorten
// rate windows.
func (c *collector) collectHTTP(m *middleware, now time.Time) httpStats {
	status := httpStatusStats{
		Status1xx: m.status1.Load(),
		Status2xx: m.status2.Load(),
		Status3xx: m.status3.Load(),
		Status4xx: m.status4.Load(),
		Status5xx: m.status5.Load(),
	}
	completed := status.Status1xx + status.Status2xx + status.Status3xx + status.Status4xx + status.Status5xx
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

package monitor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
)

func TestProcessCPUPercent(t *testing.T) {
	value, ok := processCPUPercent(10, 11, time.Second, 2)
	mustTrue(t, ok)
	mustInDelta(t, 50, value, 0.001)
	value, ok = processCPUPercent(10, 20, time.Second, 1)
	mustTrue(t, ok)
	mustEqual(t, float64(100), value)
	_, ok = processCPUPercent(10, 9, time.Second, 1)
	mustFalse(t, ok)
}

func TestSystemCPUPercent(t *testing.T) {
	previous := cpu.TimesStat{User: 10, System: 5, Idle: 85}
	current := cpu.TimesStat{User: 20, System: 10, Idle: 170}
	value, ok := systemCPUPercent(previous, current)
	mustTrue(t, ok)
	mustInDelta(t, 15, value, 0.001)
}

func TestNetworkRates(t *testing.T) {
	receive, send := networkRates(100, 200, 300, 500, 2*time.Second)
	mustNotNil(t, receive)
	mustNotNil(t, send)
	mustInDelta(t, 100, *receive, 0.001)
	mustInDelta(t, 150, *send, 0.001)
	receive, send = networkRates(300, 500, 100, 200, time.Second)
	mustNil(t, receive)
	mustNil(t, send)
}

func TestMissingProcessUsesStableErrors(t *testing.T) {
	collector := newCollector(time.Now(), false)
	collector.proc = nil
	_, errors := collector.collectProcess(time.Now())
	mustEqual(t, []string{"process.cpu", "process.memory", "process.threads", "process.descriptors"}, errors)
	for _, value := range errors {
		mustNotContain(t, value, `C:\`)
		mustNotContain(t, value, "/proc/")
	}
}

func TestWindowsLoadIsUnsupportedWithoutPartial(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific behavior")
	}
	collector := newCollector(time.Now(), false)
	stats, errors := collector.collectSystem(time.Now())
	mustNil(t, stats.Load1)
	mustNil(t, stats.Load5)
	mustNil(t, stats.Load15)
	if slices.Contains(errors, "system.load") {
		t.Fatal("Windows load should be unsupported without a collection error")
	}
}

func TestRuntimeStatsFromMetricValues(t *testing.T) {
	stats := runtimeStatsFromMetricValues(runtimeMetricValues{
		heapObjectsBytes:  1,
		heapUnusedBytes:   2,
		heapFreeBytes:     4,
		heapReleasedBytes: 5,
		heapObjects:       6,
		heapGoalBytes:     7,
		heapAllocs:        8,
		heapFrees:         9,
		heapTinyAllocs:    10,
		gcCycles:          11,
		gcCPUSeconds:      2,
		totalCPUSeconds:   8,
	})

	first := stats
	mustEqual(t, uint64(1), first.HeapAllocBytes)
	mustEqual(t, uint64(12), first.HeapSysBytes)
	mustEqual(t, uint64(3), first.HeapInuseBytes)
	mustEqual(t, uint64(9), first.HeapIdleBytes)
	mustEqual(t, uint64(5), first.HeapReleasedBytes)
	mustEqual(t, uint64(6), first.HeapObjects)
	mustEqual(t, uint64(7), first.NextGCBytes)
	mustEqual(t, uint64(18), first.Mallocs)
	mustEqual(t, uint64(19), first.Frees)
	mustEqual(t, uint64(11), first.GCCount)
	mustEqual(t, 0.25, first.GCCPUFraction)
	mustNil(t, first.GCPauseLastNS)
	mustNil(t, first.GCPauseTotalNS)
	mustNil(t, first.GCPauseWindowNS)
}

func TestApplyGCPauseMetrics(t *testing.T) {
	memory := runtime.MemStats{NumGC: 1, PauseTotalNs: 120}
	memory.PauseNs[0] = 11
	var first runtimeStats
	applyGCPauseMetrics(&first, memory, false, 0)
	mustNotNil(t, first.GCPauseLastNS)
	mustNotNil(t, first.GCPauseTotalNS)
	mustEqual(t, uint64(11), *first.GCPauseLastNS)
	mustEqual(t, uint64(120), *first.GCPauseTotalNS)
	mustNil(t, first.GCPauseWindowNS)

	memory.PauseTotalNs = 155
	var second runtimeStats
	applyGCPauseMetrics(&second, memory, true, *first.GCPauseTotalNS)
	mustNotNil(t, second.GCPauseWindowNS)
	mustEqual(t, uint64(35), *second.GCPauseWindowNS)

	memory.PauseTotalNs = 10
	var reset runtimeStats
	applyGCPauseMetrics(&reset, memory, true, *second.GCPauseTotalNS)
	mustNil(t, reset.GCPauseWindowNS)
}

func TestGCPauseCollectionIsOptIn(t *testing.T) {
	disabled := newCollector(time.Now(), false)
	disabledStats := disabled.collectRuntime()
	mustFalse(t, disabledStats.GCPauseMetricsEnabled)
	mustNil(t, disabledStats.GCPauseLastNS)

	enabled := newCollector(time.Now(), true)
	enabledStats := enabled.collectRuntime()
	mustTrue(t, enabledStats.GCPauseMetricsEnabled)
	mustNotNil(t, enabledStats.GCPauseLastNS)
	mustNotNil(t, enabledStats.GCPauseTotalNS)
}

func TestSystemSnapshotDoesNotExposeFilesystemIdentity(t *testing.T) {
	collector := newCollector(time.Now(), false)
	stats, _ := collector.collectSystem(time.Now())
	encoded, err := json.Marshal(stats)
	mustNoError(t, err)
	value := string(encoded)
	mustNotContain(t, value, `"path"`)
	mustNotContain(t, value, `"device"`)
	if workingDirectory, err := os.Getwd(); err == nil {
		mustNotContain(t, value, workingDirectory)
	}
}

func TestFilesystemTypeForPathUsesMostSpecificMount(t *testing.T) {
	base := t.TempDir()
	applicationPath := filepath.Join(base, "service", "data")
	partitions := []disk.PartitionStat{
		{Mountpoint: filepath.Dir(base), Fstype: "parentfs"},
		{Mountpoint: base, Fstype: "applicationfs"},
	}
	mustEqual(t, "applicationfs", filesystemTypeForPath(applicationPath, partitions))
	mustEqual(t, "", filesystemTypeForPath(applicationPath, nil))
	if volume := filepath.VolumeName(applicationPath); volume != "" {
		mustEqual(t, "volumefs", filesystemTypeForPath(applicationPath, []disk.PartitionStat{{Mountpoint: volume, Fstype: "volumefs"}}))
	}
}

func TestAppendCollectionErrorsDeduplicates(t *testing.T) {
	actual := appendCollectionErrors([]string{"system.cpu"}, "system.memory", "system.cpu", "")
	mustEqual(t, []string{"system.cpu", "system.memory"}, actual)
}

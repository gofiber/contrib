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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessCPUPercent(t *testing.T) {
	value, ok := processCPUPercent(10, 11, time.Second, 2)
	assert.True(t, ok)
	assert.InDelta(t, 50, value, 0.001)
	value, ok = processCPUPercent(10, 20, time.Second, 1)
	assert.True(t, ok)
	assert.Equal(t, float64(100), value)
	_, ok = processCPUPercent(10, 9, time.Second, 1)
	assert.False(t, ok)
}

func TestSystemCPUPercent(t *testing.T) {
	previous := cpu.TimesStat{User: 10, System: 5, Idle: 85}
	current := cpu.TimesStat{User: 20, System: 10, Idle: 170}
	value, ok := systemCPUPercent(previous, current)
	assert.True(t, ok)
	assert.InDelta(t, 15, value, 0.001)
}

func TestNetworkRates(t *testing.T) {
	receive, send := networkRates(100, 200, 300, 500, 2*time.Second)
	require.NotNil(t, receive)
	require.NotNil(t, send)
	assert.InDelta(t, 100, *receive, 0.001)
	assert.InDelta(t, 150, *send, 0.001)
	receive, send = networkRates(300, 500, 100, 200, time.Second)
	assert.Nil(t, receive)
	assert.Nil(t, send)
}

func TestMissingProcessUsesStableErrors(t *testing.T) {
	collector := newCollector(time.Now(), false)
	collector.proc = nil
	_, errors := collector.collectProcess(time.Now())
	assert.Equal(t, []string{"process.cpu", "process.memory", "process.threads", "process.descriptors"}, errors)
	for _, value := range errors {
		assert.NotContains(t, value, `C:\`)
		assert.NotContains(t, value, "/proc/")
	}
}

func TestWindowsLoadIsUnsupportedWithoutPartial(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific behavior")
	}
	collector := newCollector(time.Now(), false)
	stats, errors := collector.collectSystem(time.Now())
	assert.Nil(t, stats.Load1)
	assert.Nil(t, stats.Load5)
	assert.Nil(t, stats.Load15)
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
	assert.Equal(t, uint64(1), first.HeapAllocBytes)
	assert.Equal(t, uint64(12), first.HeapSysBytes)
	assert.Equal(t, uint64(3), first.HeapInuseBytes)
	assert.Equal(t, uint64(9), first.HeapIdleBytes)
	assert.Equal(t, uint64(5), first.HeapReleasedBytes)
	assert.Equal(t, uint64(6), first.HeapObjects)
	assert.Equal(t, uint64(7), first.NextGCBytes)
	assert.Equal(t, uint64(18), first.Mallocs)
	assert.Equal(t, uint64(19), first.Frees)
	assert.Equal(t, uint64(11), first.GCCount)
	assert.Equal(t, 0.25, first.GCCPUFraction)
	assert.Nil(t, first.GCPauseLastNS)
	assert.Nil(t, first.GCPauseTotalNS)
	assert.Nil(t, first.GCPauseWindowNS)
}

func TestApplyGCPauseMetrics(t *testing.T) {
	memory := runtime.MemStats{NumGC: 1, PauseTotalNs: 120}
	memory.PauseNs[0] = 11
	var first runtimeStats
	applyGCPauseMetrics(&first, memory, false, 0)
	require.NotNil(t, first.GCPauseLastNS)
	require.NotNil(t, first.GCPauseTotalNS)
	assert.Equal(t, uint64(11), *first.GCPauseLastNS)
	assert.Equal(t, uint64(120), *first.GCPauseTotalNS)
	assert.Nil(t, first.GCPauseWindowNS)

	memory.PauseTotalNs = 155
	var second runtimeStats
	applyGCPauseMetrics(&second, memory, true, *first.GCPauseTotalNS)
	require.NotNil(t, second.GCPauseWindowNS)
	assert.Equal(t, uint64(35), *second.GCPauseWindowNS)

	memory.PauseTotalNs = 10
	var reset runtimeStats
	applyGCPauseMetrics(&reset, memory, true, *second.GCPauseTotalNS)
	assert.Nil(t, reset.GCPauseWindowNS)
}

func TestApplyGCPauseMetricsWithoutGC(t *testing.T) {
	memory := runtime.MemStats{}

	var first runtimeStats
	applyGCPauseMetrics(&first, memory, false, 0)
	assert.Nil(t, first.GCPauseLastNS)
	require.NotNil(t, first.GCPauseTotalNS)
	assert.Zero(t, *first.GCPauseTotalNS)
	assert.Nil(t, first.GCPauseWindowNS)
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"gc_pause_last_ns":null`)

	var next runtimeStats
	applyGCPauseMetrics(&next, memory, true, 0)
	assert.Nil(t, next.GCPauseLastNS)
	require.NotNil(t, next.GCPauseTotalNS)
	assert.Zero(t, *next.GCPauseTotalNS)
	require.NotNil(t, next.GCPauseWindowNS)
	assert.Zero(t, *next.GCPauseWindowNS)
}

func TestGCPauseCollectionIsOptIn(t *testing.T) {
	disabled := newCollector(time.Now(), false)
	disabledStats := disabled.collectRuntime()
	assert.False(t, disabledStats.GCPauseMetricsEnabled)
	assert.Nil(t, disabledStats.GCPauseLastNS)

	enabled := newCollector(time.Now(), true)
	enabledStats := enabled.collectRuntime()
	assert.True(t, enabledStats.GCPauseMetricsEnabled)
	assert.NotNil(t, enabledStats.GCPauseTotalNS)
}

func TestSystemSnapshotDoesNotExposeFilesystemIdentity(t *testing.T) {
	collector := newCollector(time.Now(), false)
	stats, _ := collector.collectSystem(time.Now())
	encoded, err := json.Marshal(stats)
	require.NoError(t, err)
	value := string(encoded)
	assert.NotContains(t, value, `"path"`)
	assert.NotContains(t, value, `"device"`)
	if workingDirectory, err := os.Getwd(); err == nil {
		assert.NotContains(t, value, workingDirectory)
	}
}

func TestFilesystemTypeForPathUsesMostSpecificMount(t *testing.T) {
	base := t.TempDir()
	applicationPath := filepath.Join(base, "service", "data")
	partitions := []disk.PartitionStat{
		{Mountpoint: filepath.Dir(base), Fstype: "parentfs"},
		{Mountpoint: base, Fstype: "applicationfs"},
	}
	assert.Equal(t, "applicationfs", filesystemTypeForPath(applicationPath, partitions))
	assert.Equal(t, "", filesystemTypeForPath(applicationPath, nil))
	if volume := filepath.VolumeName(applicationPath); volume != "" {
		assert.Equal(t, "volumefs", filesystemTypeForPath(applicationPath, []disk.PartitionStat{{Mountpoint: volume, Fstype: "volumefs"}}))
	}
}

func TestAppendCollectionErrorsDeduplicates(t *testing.T) {
	actual := appendCollectionErrors([]string{"system.cpu"}, "system.memory", "system.cpu", "")
	assert.Equal(t, []string{"system.cpu", "system.memory"}, actual)
}

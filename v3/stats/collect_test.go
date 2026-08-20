package stats

import (
	"runtime"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/stretchr/testify/require"
)

func TestProcessCPUPercent(t *testing.T) {
	value, ok := processCPUPercent(10, 11, time.Second, 2)
	require.True(t, ok)
	require.InDelta(t, 50, value, 0.001)
	value, ok = processCPUPercent(10, 20, time.Second, 1)
	require.True(t, ok)
	require.Equal(t, float64(100), value)
	_, ok = processCPUPercent(10, 9, time.Second, 1)
	require.False(t, ok)
}

func TestSystemCPUPercent(t *testing.T) {
	previous := cpu.TimesStat{User: 10, System: 5, Idle: 85}
	current := cpu.TimesStat{User: 20, System: 10, Idle: 170}
	value, ok := systemCPUPercent(previous, current)
	require.True(t, ok)
	require.InDelta(t, 15, value, 0.001)
}

func TestNetworkRates(t *testing.T) {
	receive, send := networkRates(100, 200, 300, 500, 2*time.Second)
	require.NotNil(t, receive)
	require.NotNil(t, send)
	require.InDelta(t, 100, *receive, 0.001)
	require.InDelta(t, 150, *send, 0.001)
	receive, send = networkRates(300, 500, 100, 200, time.Second)
	require.Nil(t, receive)
	require.Nil(t, send)
}

func TestMissingProcessUsesStableErrors(t *testing.T) {
	collector := newCollector(time.Now())
	collector.proc = nil
	_, errors := collector.collectProcess(time.Now())
	require.Equal(t, []string{"process.cpu", "process.memory", "process.threads", "process.descriptors"}, errors)
	for _, value := range errors {
		require.NotContains(t, value, `C:\`)
		require.NotContains(t, value, "/proc/")
	}
}

func TestWindowsLoadIsUnsupportedWithoutPartial(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific behavior")
	}
	collector := newCollector(time.Now())
	stats, errors := collector.collectSystem(time.Now())
	require.Nil(t, stats.Load1)
	require.NotContains(t, errors, "system.load")
}

func TestAppendCollectionErrorsDeduplicates(t *testing.T) {
	actual := appendCollectionErrors([]string{"system.cpu"}, "system.memory", "system.cpu", "")
	require.Equal(t, []string{"system.cpu", "system.memory"}, actual)
}

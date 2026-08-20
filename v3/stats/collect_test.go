package stats

import (
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
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
	collector := newCollector(time.Now())
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
	collector := newCollector(time.Now())
	stats, errors := collector.collectSystem(time.Now())
	mustNil(t, stats.Load1)
	if slices.Contains(errors, "system.load") {
		t.Fatal("Windows load should be unsupported without a collection error")
	}
}

func TestAppendCollectionErrorsDeduplicates(t *testing.T) {
	actual := appendCollectionErrors([]string{"system.cpu"}, "system.memory", "system.cpu", "")
	mustEqual(t, []string{"system.cpu", "system.memory"}, actual)
}

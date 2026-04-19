package monitor

import (
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type stats struct {
	PID statsPID `json:"pid"`
	OS  statsOS  `json:"os"`
}

type statsPID struct {
	CPU        float64 `json:"cpu"`
	RAM        uint64  `json:"ram"`
	Conns      int     `json:"conns"`
	Goroutines int     `json:"goroutines"`
	// Requests is encoded as a string to preserve precision in JavaScript,
	// which cannot exactly represent uint64 values above 2^53-1.
	Requests string  `json:"requests"`
	Uptime   float64 `json:"uptime"`
}

type statsOS struct {
	CPU      float64 `json:"cpu"`
	RAM      uint64  `json:"ram"`
	TotalRAM uint64  `json:"total_ram"`
	LoadAvg  float64 `json:"load_avg"`
	Conns    int     `json:"conns"`
}

var (
	monitPIDCPU        atomic.Value
	monitPIDRAM        atomic.Value
	monitPIDConns      atomic.Value
	monitPIDGoroutines atomic.Value

	monitOSCPU      atomic.Value
	monitOSRAM      atomic.Value
	monitOSTotalRAM atomic.Value
	monitOSLoadAvg  atomic.Value
	monitOSConns    atomic.Value

	monitTotalRequests atomic.Uint64
)

var (
	once             sync.Once
	processStartTime time.Time
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Start routine to update statistics
	once.Do(func() {
		// Initialize atomic.Values with typed zero defaults so that Load() before
		// the first updateStatistics completes does not panic with
		// "sync/atomic: Load of uninitialized Value".
		monitPIDCPU.Store(float64(0))
		monitPIDRAM.Store(uint64(0))
		monitPIDConns.Store(int(0))
		monitPIDGoroutines.Store(int(0))
		monitOSCPU.Store(float64(0))
		monitOSRAM.Store(uint64(0))
		monitOSTotalRAM.Store(uint64(0))
		monitOSLoadAvg.Store(float64(0))
		monitOSConns.Store(int(0))
		// p may be nil on permission/platform errors; updateStatistics handles nil gracefully.
		p, _ := process.NewProcess(int32(os.Getpid()))

		// Use the actual process start time from gopsutil when available;
		// fall back to middleware init time if the process handle is unavailable.
		processStartTime = time.Now()
		if p != nil {
			if createMs, err := p.CreateTime(); err == nil {
				processStartTime = time.UnixMilli(createMs)
			}
		}

		numcpu := runtime.NumCPU()
		updateStatistics(p, numcpu)

		go func() {
			for {
				time.Sleep(cfg.Refresh)

				updateStatistics(p, numcpu)
			}
		}()
	})

	// Return new handler
	return func(c fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		// Increment the absolute request counter
		monitTotalRequests.Add(1)

		if c.Method() != fiber.MethodGet {
			return fiber.ErrMethodNotAllowed
		}
		if c.Get(fiber.HeaderAccept) == fiber.MIMEApplicationJSON || cfg.APIOnly {
			snapshot := stats{}
			//nolint:errcheck // Ignore the type-assertion errors
			snapshot.PID.CPU, _ = monitPIDCPU.Load().(float64)
			snapshot.PID.RAM, _ = monitPIDRAM.Load().(uint64)
			snapshot.PID.Conns, _ = monitPIDConns.Load().(int)
			snapshot.PID.Goroutines, _ = monitPIDGoroutines.Load().(int)
			snapshot.PID.Requests = strconv.FormatUint(monitTotalRequests.Load(), 10)
			snapshot.PID.Uptime = time.Since(processStartTime).Seconds()

			snapshot.OS.CPU, _ = monitOSCPU.Load().(float64)
			snapshot.OS.RAM, _ = monitOSRAM.Load().(uint64)
			snapshot.OS.TotalRAM, _ = monitOSTotalRAM.Load().(uint64)
			snapshot.OS.LoadAvg, _ = monitOSLoadAvg.Load().(float64)
			snapshot.OS.Conns, _ = monitOSConns.Load().(int)
			return c.Status(fiber.StatusOK).JSON(&snapshot)
		}
		c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
		return c.Status(fiber.StatusOK).SendString(cfg.index)
	}
}

func updateStatistics(p *process.Process, numcpu int) {
	// Process-dependent metrics — skipped when p is nil (e.g. NewProcess failed).
	if p != nil {
		if pidCPU, err := p.Percent(0); err == nil {
			monitPIDCPU.Store(pidCPU / float64(numcpu))
		}

		if pidRAM, err := p.MemoryInfo(); err == nil && pidRAM != nil {
			monitPIDRAM.Store(pidRAM.RSS)
		}

		if pidConns, err := net.ConnectionsPid("tcp", p.Pid); err == nil {
			monitPIDConns.Store(len(pidConns))
		}
	}

	// Process-independent metrics — always updated.
	if osCPU, err := cpu.Percent(0, false); err == nil && len(osCPU) > 0 {
		monitOSCPU.Store(osCPU[0])
	}

	if osRAM, err := mem.VirtualMemory(); err == nil && osRAM != nil {
		monitOSRAM.Store(osRAM.Used)
		monitOSTotalRAM.Store(osRAM.Total)
	}

	if loadAvg, err := load.Avg(); err == nil && loadAvg != nil {
		monitOSLoadAvg.Store(loadAvg.Load1)
	}

	monitPIDGoroutines.Store(runtime.NumGoroutine())

	if osConns, err := net.Connections("tcp"); err == nil {
		monitOSConns.Store(len(osConns))
	}
}

package fgprof

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/felixge/fgprof"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
)

const (
	// maxSeconds is the longest profile whose duration still fits a time.Duration.
	maxSeconds = math.MaxInt64 / int64(time.Second)
	// defaultSeconds is what fgprof.Handler profiles without a seconds parameter.
	defaultSeconds = 30
)

// profileGuard admits a bounded number of bounded profiles and recovers what net/http would:
// fgprof divides by a zero sample rate when a profile ends early, and the adaptor re-panics that.
type profileGuard struct {
	next       http.Handler
	maxSeconds int64
	running    chan struct{}
}

func (g *profileGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read like fgprof.Handler does, so both see the same value; it rejects what does not parse.
	seconds := int64(defaultSeconds)
	if s := r.URL.Query().Get("seconds"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			seconds = n
		}
	}
	if seconds > g.maxSeconds {
		http.Error(w, "bad seconds: above "+strconv.FormatInt(g.maxSeconds, 10), http.StatusBadRequest)
		return
	}

	select {
	case g.running <- struct{}{}:
		defer func() { <-g.running }()
	default:
		http.Error(w, "too many profiles running", http.StatusServiceUnavailable)
		return
	}

	defer func() {
		if p := recover(); p != nil {
			log.Errorf("fgprof: handler panicked: %v", p)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	}()
	g.next.ServeHTTP(w, r)
}

func New(conf ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(conf...)

	fgProfPath := cfg.Prefix + "/debug/fgprof"

	// Each profile holds a record per goroutine and adds its own, so memory grows with n*n.
	var fgprofHandler = adaptor.HTTPHandler(&profileGuard{
		next:       fgprof.Handler(),
		maxSeconds: min(int64(cfg.MaxSeconds), maxSeconds),
		running:    make(chan struct{}, cfg.MaxConcurrent),
	})

	// Return new handler
	return func(c fiber.Ctx) error {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		if c.Path() == fgProfPath {
			return fgprofHandler(c)
		}
		return c.Next()
	}
}

package monitor

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
)

const (
	defaultTitle       = "Fiber Monitor"
	defaultDescription = "Live process, runtime, system, and HTTP metrics for this Fiber service."
	defaultFooter      = "Powered by github.com/gofiber/contrib/v3/monitor."
	defaultRefresh     = 3 * time.Second
	minimumRefresh     = time.Second

	legacyDefaultFontURL    = `https://fonts.googleapis.com/css2?family=Roboto:wght@400;900&display=swap`
	legacyDefaultChartJSURL = `https://cdn.jsdelivr.net/npm/chart.js@2.9/dist/Chart.bundle.min.js`
)

// ErrInvalidFaviconURL is returned when Config.FaviconURL is not supported.
var ErrInvalidFaviconURL = errors.New("monitor: favicon url must be a root-relative path or an absolute http(s) url")

// Config defines the configuration for the monitor middleware.
type Config struct {
	// Next passes matching requests to the rest of the Fiber stack. Requests
	// passed through this way are included in the HTTP metrics.
	Next func(c fiber.Ctx) bool

	// Title is shown in the page title and dashboard heading.
	Title string

	// Description is shown below the dashboard heading.
	Description string

	// Footer is shown at the bottom of the dashboard.
	Footer string

	// FaviconURL overrides the built-in favicon.
	// It must be a root-relative path or an absolute HTTP(S) URL.
	FaviconURL string

	// Refresh controls browser polling and server snapshot cache TTL.
	Refresh time.Duration

	// APIOnly makes the monitor endpoint return JSON regardless of Accept.
	APIOnly bool

	// EnableGCPauseMetrics enables exact GC pause metrics collected with
	// runtime.ReadMemStats. It is disabled by default because ReadMemStats
	// briefly stops the Go runtime while taking a consistent snapshot.
	EnableGCPauseMetrics bool

	// CustomHead is retained for source compatibility and has no effect.
	//
	// Deprecated: the embedded dashboard does not accept custom HTML.
	CustomHead string

	// FontURL is retained for source compatibility and has no effect.
	//
	// Deprecated: the embedded dashboard does not load external fonts.
	FontURL string

	// ChartJSURL is retained for source compatibility and has no effect.
	//
	// Deprecated: the embedded dashboard uses dependency-free Canvas charts.
	ChartJSURL string
}

// ConfigDefault is the default configuration.
var ConfigDefault = Config{
	Title:       defaultTitle,
	Description: defaultDescription,
	Footer:      defaultFooter,
	Refresh:     defaultRefresh,
	FontURL:     legacyDefaultFontURL,
	ChartJSURL:  legacyDefaultChartJSURL,
}

func configDefault(config ...Config) Config {
	base := ConfigDefault
	if len(config) == 0 {
		return base
	}

	cfg := config[0]
	if cfg.Next == nil {
		cfg.Next = base.Next
	}
	if cfg.Title == "" {
		cfg.Title = base.Title
	}
	if cfg.Description == "" {
		cfg.Description = base.Description
	}
	if cfg.Footer == "" {
		cfg.Footer = base.Footer
	}
	if cfg.FaviconURL == "" {
		cfg.FaviconURL = base.FaviconURL
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = base.Refresh
	}
	if !cfg.APIOnly {
		cfg.APIOnly = base.APIOnly
	}
	if !cfg.EnableGCPauseMetrics {
		cfg.EnableGCPauseMetrics = base.EnableGCPauseMetrics
	}
	if cfg.CustomHead == "" {
		cfg.CustomHead = base.CustomHead
	}
	if cfg.FontURL == "" {
		cfg.FontURL = base.FontURL
	}
	if cfg.ChartJSURL == "" {
		cfg.ChartJSURL = base.ChartJSURL
	}
	return cfg
}

func (c Config) normalized() (Config, error) {
	if c.Title == "" {
		c.Title = defaultTitle
	}
	if c.Description == "" {
		c.Description = defaultDescription
	}
	if c.Footer == "" {
		c.Footer = defaultFooter
	}
	if c.Refresh <= 0 {
		c.Refresh = defaultRefresh
	}
	if c.Refresh < minimumRefresh {
		c.Refresh = minimumRefresh
	}

	faviconURL, err := normalizeFaviconURL(c.FaviconURL)
	if err != nil {
		return Config{}, err
	}
	c.FaviconURL = faviconURL
	return c, nil
}

func normalizeFaviconURL(rawURL string) (string, error) {
	rawURL = utils.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", ErrInvalidFaviconURL
	}
	if strings.HasPrefix(rawURL, "/") &&
		!strings.HasPrefix(rawURL, "//") &&
		!strings.HasPrefix(rawURL, "/\\") &&
		parsed.Scheme == "" && parsed.Host == "" {
		return rawURL, nil
	}
	if (utils.EqualFold(parsed.Scheme, "http") || utils.EqualFold(parsed.Scheme, "https")) &&
		parsed.Host != "" && parsed.User == nil {
		return rawURL, nil
	}
	return "", ErrInvalidFaviconURL
}

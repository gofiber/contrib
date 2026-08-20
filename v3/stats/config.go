package stats

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
)

const (
	defaultPath        = "/stats"
	defaultTitle       = "Fiber Stats"
	defaultDescription = "Live process, runtime, system, and HTTP metrics for this Fiber service."
	defaultFooter      = "Powered by github.com/gofiber/contrib/v3/stats."
	defaultRefresh     = 2 * time.Second
	minimumRefresh     = time.Second
)

// ErrInvalidFaviconURL is returned when Config.FaviconURL is not supported.
var ErrInvalidFaviconURL = errors.New("stats: favicon url must be a root-relative path or an absolute http(s) url")

// Config defines the configuration for the stats middleware.
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	Next func(c fiber.Ctx) bool

	// Path is the dashboard endpoint.
	Path string

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

	// EnableGCPauseMetrics enables exact GC pause metrics collected with
	// runtime.ReadMemStats. It is disabled by default because ReadMemStats
	// briefly stops the Go runtime while taking a consistent snapshot.
	EnableGCPauseMetrics bool
}

// ConfigDefault is the default configuration.
var ConfigDefault = Config{
	Path:        defaultPath,
	Title:       defaultTitle,
	Description: defaultDescription,
	Footer:      defaultFooter,
	Refresh:     defaultRefresh,
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
	if cfg.Path == "" {
		cfg.Path = base.Path
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
	return cfg
}

func (c Config) normalized() (Config, error) {
	c.Path = normalizePath(c.Path)
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

func normalizePath(path string) string {
	path = utils.TrimSpace(path)
	if index := strings.IndexAny(path, "?#"); index >= 0 {
		path = path[:index]
	}
	path = utils.TrimSpace(path)
	if path == "" {
		return defaultPath
	}
	if path[0] != '/' {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	return path
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

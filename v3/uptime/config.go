package uptime

import (
	"errors"
	"time"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
	"github.com/gofiber/fiber/v3"
)

const (
	defaultSampleInterval  = 3 * time.Second
	defaultRetentionDays   = 90
	defaultDaysToShow      = 30
	defaultGreenThreshold  = 0.999
	defaultYellowThreshold = 0.99
	defaultUIPath          = "/uptime"
	defaultUITitle         = "Fiber Uptime"
	defaultUIDescription   = "Historical uptime for Fiber services."
	defaultUIFooter        = "Powered by github.com/gofiber/contrib/v3/uptime."
	defaultSQLitePath      = "./data/uptime.db"
	maintenanceInterval    = time.Minute
)

var (
	// ErrMissingServiceID is returned when Config.ServiceID is empty.
	ErrMissingServiceID = errors.New("uptime: service id is required")
)

// SQLiteConfig controls the default SQLite-backed uptime store.
type SQLiteConfig = storage.SQLiteConfig

// Config defines the configuration for the uptime middleware.
type Config struct {
	// App registers a shutdown hook to close the uptime runtime when the Fiber app stops.
	App *fiber.App

	// Next defines a function to skip this middleware when returned true.
	Next func(c fiber.Ctx) bool

	// ServiceID is the stable logical service identifier. It is required.
	ServiceID string
	// ServiceName is the display name shown in the dashboard. It defaults to ServiceID.
	ServiceName string
	// ServiceDescription is optional text shown under the service name.
	ServiceDescription string

	// SampleInterval is the heartbeat interval and uptime slot size.
	SampleInterval time.Duration
	// RetentionDays controls how many days of heartbeat data are kept.
	RetentionDays int
	// DaysToShow controls how many days are returned by the API and dashboard.
	DaysToShow int
	// Timezone is used to compute day boundaries. It defaults to time.Local.
	Timezone *time.Location

	// NodeID identifies this node when the default ID generator is used.
	NodeID int64
	// InstanceID overrides the generated process instance ID when non-zero.
	InstanceID int64
	// IDGenerator generates instance IDs when InstanceID is zero.
	IDGenerator IDGenerator

	// SQLite configures the default SQLite store.
	SQLite SQLiteConfig
	// Snapshot configures snapshot caching.
	Snapshot SnapshotConfig
	// UI configures the built-in dashboard.
	UI UIConfig
}

// UIConfig controls the built-in status page.
type UIConfig struct {
	// Title is used for the dashboard heading and document title.
	Title string
	// Path is the dashboard mount path. The JSON API is served below Path + "/api/status".
	Path string
	// Description is shown below the dashboard header.
	Description string
	// Footer is shown at the bottom of the dashboard.
	Footer string

	// GreenThreshold is the minimum uptime ratio for a green day, in the range [0, 1].
	GreenThreshold float64
	// YellowThreshold is the minimum uptime ratio for a yellow day, in the range [0, 1].
	YellowThreshold float64
}

// SnapshotConfig controls the in-memory snapshot cache.
type SnapshotConfig struct {
	// CacheTTL is the maximum age of a cached status snapshot.
	CacheTTL time.Duration
	// DisableCache forces every status request to query fresh storage data.
	DisableCache bool
	// DisableStaleIfError disables returning the previous cached snapshot after a refresh error.
	DisableStaleIfError bool
}

// ConfigDefault is the default configuration.
var ConfigDefault = Config{
	SampleInterval: defaultSampleInterval,
	RetentionDays:  defaultRetentionDays,
	DaysToShow:     defaultDaysToShow,
	Timezone:       time.Local,
	SQLite: SQLiteConfig{
		Path: defaultSQLitePath,
	},
	UI: UIConfig{
		Title:           defaultUITitle,
		Path:            defaultUIPath,
		Description:     defaultUIDescription,
		Footer:          defaultUIFooter,
		GreenThreshold:  defaultGreenThreshold,
		YellowThreshold: defaultYellowThreshold,
	},
}

func configDefault(config ...Config) Config {
	if len(config) < 1 {
		return ConfigDefault
	}
	return config[0]
}

func (c Config) normalized() (Config, error) {
	if c.ServiceID == "" {
		return Config{}, ErrMissingServiceID
	}
	if c.ServiceName == "" {
		c.ServiceName = c.ServiceID
	}
	if c.SampleInterval == 0 {
		c.SampleInterval = defaultSampleInterval
	}
	if c.SampleInterval < time.Second {
		return Config{}, errors.New("uptime: sample interval must be at least 1s")
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = defaultRetentionDays
	}
	if c.RetentionDays < 1 {
		return Config{}, errors.New("uptime: retention days must be positive")
	}
	if c.DaysToShow == 0 {
		c.DaysToShow = defaultDaysToShow
	}
	if c.DaysToShow < 1 {
		return Config{}, errors.New("uptime: days to show must be positive")
	}
	if c.DaysToShow > c.RetentionDays {
		c.DaysToShow = c.RetentionDays
	}
	if c.Timezone == nil {
		c.Timezone = time.Local
	}
	if c.Snapshot.CacheTTL == 0 {
		c.Snapshot.CacheTTL = c.SampleInterval
	}
	if !c.Snapshot.DisableCache && c.Snapshot.CacheTTL < time.Second {
		return Config{}, errors.New("uptime: snapshot cache ttl must be at least 1s")
	}
	if c.UI.Path == "" {
		c.UI.Path = defaultUIPath
	}
	c.UI.Path = normalizePath(c.UI.Path)
	if c.UI.Title == "" {
		c.UI.Title = defaultUITitle
	}
	if c.UI.Description == "" {
		c.UI.Description = defaultUIDescription
	}
	if c.UI.Footer == "" {
		c.UI.Footer = defaultUIFooter
	}
	if c.UI.GreenThreshold == 0 {
		c.UI.GreenThreshold = defaultGreenThreshold
	}
	if c.UI.YellowThreshold == 0 {
		c.UI.YellowThreshold = defaultYellowThreshold
	}
	if c.UI.GreenThreshold < 0 || c.UI.GreenThreshold > 1 {
		return Config{}, errors.New("uptime: green threshold must be between 0 and 1")
	}
	if c.UI.YellowThreshold < 0 || c.UI.YellowThreshold > 1 {
		return Config{}, errors.New("uptime: yellow threshold must be between 0 and 1")
	}
	if c.UI.GreenThreshold < c.UI.YellowThreshold {
		return Config{}, errors.New("uptime: green threshold must be greater than or equal to yellow threshold")
	}
	if c.SQLite.Path == "" {
		c.SQLite.Path = defaultSQLitePath
	}
	return c, nil
}

func normalizePath(path string) string {
	if path == "" {
		return defaultUIPath
	}
	if path[0] != '/' {
		path = "/" + path
	}
	for len(path) > 1 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

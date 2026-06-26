package uptime

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	defaultSampleInterval  = 3 * time.Second
	defaultRetentionDays   = 90
	defaultDaysToShow      = 90
	defaultGreenThreshold  = 0.99
	defaultYellowThreshold = 0.95
	defaultUIPath          = "/uptime"
	defaultUITitle         = "Fiber Uptime"
	defaultUIDescription   = "Historical uptime for Fiber services."
	defaultUIFooter        = "Powered by github.com/gofiber/contrib/v3/uptime."
	defaultSQLitePath      = "./data/uptime.db"
	maintenanceInterval    = time.Minute
)

var (
	ErrMissingServiceID = errors.New("uptime: service id is required")
)

// Config defines the configuration for the uptime middleware.
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	Next func(c fiber.Ctx) bool

	ServiceID          string
	ServiceName        string
	ServiceDescription string

	SampleInterval time.Duration
	RetentionDays  int
	DaysToShow     int
	Timezone       *time.Location

	NodeID      int64
	InstanceID  int64
	IDGenerator IDGenerator

	Store    Store
	SQLite   SQLiteConfig
	Postgres PostgresConfig

	Alert    AlertConfig
	Snapshot SnapshotConfig
	UI       UIConfig
}

// UIConfig controls the built-in status page.
type UIConfig struct {
	Title       string
	Path        string
	Description string
	Footer      string

	GreenThreshold  float64
	YellowThreshold float64
}

// SnapshotConfig controls the in-memory snapshot cache.
type SnapshotConfig struct {
	CacheTTL            time.Duration
	DisableCache        bool
	DisableStaleIfError bool
}

const (
	AlertStatusUp   = "up"
	AlertStatusDown = "down"
)

// AlertHook receives deduplicated service status transitions.
type AlertHook func(context.Context, AlertEvent) error

// AlertConfig controls optional status-transition notifications.
type AlertConfig struct {
	Hook AlertHook

	CheckInterval     time.Duration
	NotifyOnFirstDown bool
}

// AlertEvent describes one service status transition.
type AlertEvent struct {
	ServiceID      string        `json:"service_id"`
	ServiceName    string        `json:"service_name"`
	Description    string        `json:"description,omitempty"`
	PreviousStatus string        `json:"previous_status"`
	CurrentStatus  string        `json:"current_status"`
	LastSeenAt     time.Time     `json:"last_seen_at"`
	DetectedAt     time.Time     `json:"detected_at"`
	SampleInterval time.Duration `json:"sample_interval"`
	DownFor        time.Duration `json:"down_for"`
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
	if c.UI.GreenThreshold < c.UI.YellowThreshold {
		return Config{}, errors.New("uptime: green threshold must be greater than or equal to yellow threshold")
	}
	if c.SQLite.Path == "" {
		c.SQLite.Path = defaultSQLitePath
	}
	if c.Store == nil {
		if c.Postgres.Configured() {
			c.Store = NewPostgresStore(c.Postgres)
		} else {
			c.Store = NewSQLiteStore(c.SQLite)
		}
	}
	if c.Alert.Hook != nil {
		if _, ok := c.Store.(AlertStateStore); !ok {
			return Config{}, errors.New("uptime: alert hook requires a store that supports alert state")
		}
		if c.Alert.CheckInterval == 0 {
			c.Alert.CheckInterval = c.SampleInterval
		}
		if c.Alert.CheckInterval < time.Second {
			return Config{}, errors.New("uptime: alert check interval must be at least 1s")
		}
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

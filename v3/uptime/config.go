package uptime

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	fiberredis "github.com/gofiber/storage/redis/v3"
)

const (
	defaultSampleInterval   = 3 * time.Second
	defaultRetentionDays    = 90
	defaultDaysToShow       = 30
	defaultGreenThreshold   = 0.999
	defaultYellowThreshold  = 0.99
	defaultUIPath           = "/uptime"
	defaultUITitle          = "Fiber Uptime"
	defaultUIDescription    = "Historical uptime for Fiber services."
	defaultUIFooter         = "Powered by github.com/gofiber/contrib/v3/uptime."
	defaultStorageKeyPrefix = "fiber:uptime"
	defaultEndpointTimeout  = 5 * time.Second
	maintenanceInterval     = time.Minute
)

var (
	// ErrMissingServiceID is returned when neither Config.ServiceID nor Config.Endpoints is set.
	ErrMissingServiceID = errors.New("uptime: service id or endpoint is required")
	// ErrMissingStore is returned when Config.Store is not set.
	ErrMissingStore = errors.New("uptime: redis storage is required")
)

// Config defines the configuration for the uptime middleware.
type Config struct {
	// App registers a shutdown hook to close the uptime runtime when the Fiber app stops.
	App *fiber.App

	// Next defines a function to skip this middleware when returned true.
	Next func(c fiber.Ctx) bool

	// ServiceID is the stable logical service identifier for the current process.
	// It is required only when Endpoints is empty.
	ServiceID string
	// ServiceName is the display name shown in the dashboard. It defaults to ServiceID.
	ServiceName string
	// ServiceDescription is optional text shown under the service name.
	ServiceDescription string

	// Endpoints defines optional HTTP endpoints to probe as tracked services.
	Endpoints []EndpointConfig

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

	// Store is the Fiber Redis storage instance used for uptime state.
	// The caller owns the storage lifecycle and should close it when the app stops.
	Store *fiberredis.Storage
	// StorageKeyPrefix namespaces all uptime keys inside the selected Redis database.
	StorageKeyPrefix string
	// UI configures the built-in dashboard.
	UI UIConfig
}

// EndpointConfig defines one HTTP endpoint to probe as an uptime service.
type EndpointConfig struct {
	// ID is the stable logical endpoint identifier. It is required.
	ID string
	// Name is the display name shown in the dashboard. It defaults to ID.
	Name string
	// Description is optional text shown under the endpoint name.
	Description string

	// URL is the absolute HTTP or HTTPS URL to probe. It is required.
	URL string
	// Method is the HTTP method used for the probe. It defaults to GET.
	Method string
	// Headers are optional request headers sent with each probe.
	Headers map[string]string
	// ExpectedStatusCodes lists status codes that mark the endpoint up.
	// When empty, any 2xx or 3xx response is considered up.
	ExpectedStatusCodes []int

	// Interval is the endpoint heartbeat interval. It defaults to Config.SampleInterval.
	Interval time.Duration
	// Timeout is the maximum duration for one probe. It defaults to 5 seconds.
	Timeout time.Duration
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

// ConfigDefault is the default configuration.
var ConfigDefault = Config{
	SampleInterval:   defaultSampleInterval,
	RetentionDays:    defaultRetentionDays,
	DaysToShow:       defaultDaysToShow,
	Timezone:         time.Local,
	StorageKeyPrefix: defaultStorageKeyPrefix,
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
	if c.SampleInterval == 0 {
		c.SampleInterval = defaultSampleInterval
	}
	if c.SampleInterval < time.Second {
		return Config{}, errors.New("uptime: sample interval must be at least 1s")
	}
	if c.ServiceID == "" && len(c.Endpoints) == 0 {
		return Config{}, ErrMissingServiceID
	}
	if c.ServiceID != "" && c.ServiceName == "" {
		c.ServiceName = c.ServiceID
	}
	endpoints, err := normalizeEndpoints(c.ServiceID, c.Endpoints, c.SampleInterval)
	if err != nil {
		return Config{}, err
	}
	c.Endpoints = endpoints
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
	if c.StorageKeyPrefix == "" {
		c.StorageKeyPrefix = defaultStorageKeyPrefix
	}
	return c, nil
}

func normalizeEndpoints(serviceID string, endpoints []EndpointConfig, sampleInterval time.Duration) ([]EndpointConfig, error) {
	if len(endpoints) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(endpoints)+1)
	if serviceID != "" {
		seen[serviceID] = struct{}{}
	}
	normalized := make([]EndpointConfig, len(endpoints))
	for i, endpoint := range endpoints {
		endpoint.ID = strings.TrimSpace(endpoint.ID)
		if endpoint.ID == "" {
			return nil, errors.New("uptime: endpoint id is required")
		}
		if _, ok := seen[endpoint.ID]; ok {
			return nil, errors.New("uptime: endpoint id must be unique")
		}
		seen[endpoint.ID] = struct{}{}

		endpoint.URL = strings.TrimSpace(endpoint.URL)
		if endpoint.URL == "" {
			return nil, errors.New("uptime: endpoint url is required")
		}
		if err := validateEndpointURL(endpoint.URL); err != nil {
			return nil, err
		}
		if endpoint.Name == "" {
			endpoint.Name = endpoint.ID
		}
		if endpoint.Method == "" {
			endpoint.Method = http.MethodGet
		}
		endpoint.Method = strings.ToUpper(endpoint.Method)
		if !validHTTPMethod(endpoint.Method) {
			return nil, errors.New("uptime: endpoint method is invalid")
		}
		if endpoint.Interval == 0 {
			endpoint.Interval = sampleInterval
		}
		if endpoint.Interval < time.Second {
			return nil, errors.New("uptime: endpoint interval must be at least 1s")
		}
		if endpoint.Timeout == 0 {
			endpoint.Timeout = defaultEndpointTimeout
		}
		if endpoint.Timeout < time.Millisecond {
			return nil, errors.New("uptime: endpoint timeout must be at least 1ms")
		}
		for _, statusCode := range endpoint.ExpectedStatusCodes {
			if statusCode < 100 || statusCode > 599 {
				return nil, errors.New("uptime: endpoint expected status code must be between 100 and 599")
			}
		}
		endpoint.Headers = copyStringMap(endpoint.Headers)
		endpoint.ExpectedStatusCodes = append([]int(nil), endpoint.ExpectedStatusCodes...)
		normalized[i] = endpoint
	}
	return normalized, nil
}

func validateEndpointURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("uptime: endpoint url is invalid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("uptime: endpoint url must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("uptime: endpoint url host is required")
	}
	return nil
}

func validHTTPMethod(method string) bool {
	if method == "" {
		return false
	}
	for _, r := range method {
		if r <= ' ' || r >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={}", r) {
			return false
		}
	}
	return true
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
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

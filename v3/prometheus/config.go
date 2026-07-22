package prometheus

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
)

// Config defines the middleware configuration.
type Config struct {
	// Service is added as the `service` const label on every metric.
	//
	// Optional. Default: "" (label omitted).
	Service string

	// Namespace prefixes every metric name.
	//
	// Optional. Default: "http".
	Namespace string

	// Subsystem prefixes every metric name after Namespace.
	//
	// Optional. Default: "".
	Subsystem string

	// Labels are attached to every metric.
	//
	// Optional. Default: no labels.
	Labels prometheus.Labels

	// Registerer is used to register metrics.
	//
	// Optional. Default: a private registry.
	Registerer prometheus.Registerer

	// Gatherer provides metrics to the HTTP handler.
	//
	// Optional. Default: a private registry/gatherer pair created when neither
	// Registerer nor Gatherer is supplied. If only one is provided, it must also
	// implement the other interface or the middleware will panic to prevent
	// silently omitting metrics.
	Gatherer prometheus.Gatherer

	// DisableGoCollector disables the Go runtime metrics collector registration.
	//
	// Optional. Default: false (collector enabled).
	DisableGoCollector bool

	// DisableProcessCollector disables the process metrics collector registration.
	//
	// Optional. Default: false (collector enabled).
	DisableProcessCollector bool

	// RequestDurationBuckets configures the histogram buckets used for request
	// latency metrics. Provide nil to use the defaults.
	//
	// Optional. Default: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10, 15, 30, 60}.
	RequestDurationBuckets []float64

	// RequestSizeBuckets configures the histogram buckets used for request
	// payload size metrics. Provide nil to use the defaults.
	//
	// Optional. Default: []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}.
	RequestSizeBuckets []float64

	// ResponseSizeBuckets configures the histogram buckets used for response
	// payload size metrics. Provide nil to use the defaults.
	//
	// Optional. Default: []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}.
	ResponseSizeBuckets []float64

	// TrackUnmatchedRequests toggles metrics for requests that do not resolve to a
	// registered Fiber route.
	//
	// Optional. Default: false.
	TrackUnmatchedRequests bool

	// UnmatchedRouteLabel is the path label used when TrackUnmatchedRequests is
	// enabled and a request does not match a registered route.
	//
	// Optional. Default: "/__unmatched__".
	UnmatchedRouteLabel string

	// EnableOpenMetrics exposes the experimental OpenMetrics encoding.
	//
	// Optional. Default: false.
	EnableOpenMetrics bool

	// EnableOpenMetricsTextCreatedSamples adds synthetic `_created` samples to
	// OpenMetrics responses.
	//
	// Optional. Default: false.
	EnableOpenMetricsTextCreatedSamples bool

	// DisableCompression prevents gzip compression of metrics responses, even when
	// requested by the client (both gzip and zstd).
	//
	// Optional. Default: false.
	DisableCompression bool

	// SkipURIs excludes matching routes from instrumentation.
	//
	// Optional. Default: none.
	SkipURIs []string

	// IgnoreStatusCodes excludes matching response status codes from metrics.
	//
	// Optional. Default: none.
	IgnoreStatusCodes []int

	// Next skips the middleware when it returns true.
	//
	// Optional. Default: nil.
	Next func(fiber.Ctx) bool
}

var (
	defaultRequestDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10, 15, 30, 60}
	defaultRequestSizeBuckets     = []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}
	defaultResponseSizeBuckets    = []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}
)

// ConfigDefault holds the default middleware configuration.
var ConfigDefault = Config{
	Namespace:              "http",
	UnmatchedRouteLabel:    "/__unmatched__",
	RequestDurationBuckets: defaultRequestDurationBuckets,
	RequestSizeBuckets:     defaultRequestSizeBuckets,
	ResponseSizeBuckets:    defaultResponseSizeBuckets,
}

func configDefault(config ...Config) Config {
	if len(config) == 0 {
		cfg := ConfigDefault
		cfg.Labels = make(prometheus.Labels)
		cfg.RequestDurationBuckets = append([]float64(nil), ConfigDefault.RequestDurationBuckets...)
		cfg.RequestSizeBuckets = append([]float64(nil), ConfigDefault.RequestSizeBuckets...)
		cfg.ResponseSizeBuckets = append([]float64(nil), ConfigDefault.ResponseSizeBuckets...)
		return cfg
	}

	cfg := config[0]

	if cfg.Namespace == "" {
		cfg.Namespace = ConfigDefault.Namespace
	}

	if cfg.UnmatchedRouteLabel == "" {
		cfg.UnmatchedRouteLabel = ConfigDefault.UnmatchedRouteLabel
	} else {
		cfg.UnmatchedRouteLabel = strings.Clone(cfg.UnmatchedRouteLabel)
	}

	if cfg.RequestDurationBuckets == nil {
		cfg.RequestDurationBuckets = append([]float64(nil), ConfigDefault.RequestDurationBuckets...)
	} else {
		cfg.RequestDurationBuckets = append([]float64(nil), cfg.RequestDurationBuckets...)
	}

	if cfg.RequestSizeBuckets == nil {
		cfg.RequestSizeBuckets = append([]float64(nil), ConfigDefault.RequestSizeBuckets...)
	} else {
		cfg.RequestSizeBuckets = append([]float64(nil), cfg.RequestSizeBuckets...)
	}

	if cfg.ResponseSizeBuckets == nil {
		cfg.ResponseSizeBuckets = append([]float64(nil), ConfigDefault.ResponseSizeBuckets...)
	} else {
		cfg.ResponseSizeBuckets = append([]float64(nil), cfg.ResponseSizeBuckets...)
	}

	if cfg.Labels == nil {
		cfg.Labels = make(prometheus.Labels)
	} else {
		labels := make(prometheus.Labels, len(cfg.Labels))
		for key, value := range cfg.Labels {
			labels[key] = value
		}
		cfg.Labels = labels
	}

	cfg.SkipURIs = append([]string(nil), cfg.SkipURIs...)
	cfg.IgnoreStatusCodes = append([]int(nil), cfg.IgnoreStatusCodes...)

	return cfg
}

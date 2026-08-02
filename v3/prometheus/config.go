package prometheus

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric identifies one of the metric families the middleware exposes. The
// values are the metric names before Namespace and Subsystem are applied.
type Metric string

// The metric families the middleware exposes. Any of them can be turned off
// through Config.DisabledMetrics.
const (
	MetricRequestsTotal            Metric = "requests_total"
	MetricRequestsStatusClassTotal Metric = "requests_status_class_total"
	MetricRequestDuration          Metric = "request_duration_seconds"
	MetricRequestSize              Metric = "request_size_bytes"
	MetricResponseSize             Metric = "response_size_bytes"
	MetricRequestsInProgress       Metric = "requests_in_progress"
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

	// MetricsPath is the request path served with the Prometheus exposition
	// format. Unless Next returns true, requests to it are answered by the
	// middleware itself and are not instrumented, regardless of where the
	// middleware is mounted.
	//
	// Optional. Default: "/metrics".
	MetricsPath string

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
	//
	// Supplying both is trusted: pairing a wrapper such as
	// prometheus.WrapRegistererWithPrefix with the registry it wraps is the
	// reason to do so, and the wrapper is not itself a Gatherer to compare
	// against. Only a provably distinct pair, two different *prometheus.Registry
	// values, is rejected; pairing a wrapper with an unrelated registry is
	// accepted and scrapes return nothing.
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
	// An empty non-nil slice drops the classic buckets, but only when
	// NativeHistogramBucketFactor is also set: client_golang substitutes its
	// own defaults for a histogram that would otherwise have no buckets at all.
	//
	// Optional. Default: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10, 15, 30, 60}.
	RequestDurationBuckets []float64

	// RequestSizeBuckets configures the histogram buckets used for request
	// payload size metrics. Provide nil to use the defaults.
	//
	// As with RequestDurationBuckets, an empty non-nil slice only drops the
	// classic buckets when NativeHistogramBucketFactor is set. Without it the
	// substituted defaults are client_golang's latency buckets, which would
	// bucket byte counts at 0.005 through 10.
	//
	// Optional. Default: []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}.
	RequestSizeBuckets []float64

	// ResponseSizeBuckets configures the histogram buckets used for response
	// payload size metrics. Provide nil to use the defaults.
	//
	// The same caveat as RequestSizeBuckets applies to an empty non-nil slice.
	//
	// Optional. Default: []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}.
	ResponseSizeBuckets []float64

	// NativeHistogramBucketFactor enables native histograms on all three
	// histogram metrics when set to a value greater than 1. It caps the growth
	// factor between consecutive buckets, so a smaller value yields finer
	// resolution: 1.1 gives a resolution of about 10%. Native histograms
	// resolve latency without hand-tuned buckets, but require a Prometheus
	// server with native histograms enabled.
	//
	// Optional. Default: 0 (classic buckets only).
	NativeHistogramBucketFactor float64

	// NativeHistogramMaxBucketNumber bounds the number of native histogram
	// buckets kept per series. Once exceeded, resolution is reduced, or the
	// histogram is reset if it has not been reset for
	// NativeHistogramMinResetDuration. Set it to keep memory bounded when the
	// observed value range is unpredictable.
	//
	// Optional. Default: 0 (unlimited).
	NativeHistogramMaxBucketNumber uint32

	// NativeHistogramMinResetDuration is the minimum time that has to pass
	// before a native histogram is reset to control its bucket count.
	//
	// Optional. Default: 0 (resets are allowed at any time).
	NativeHistogramMinResetDuration time.Duration

	// TrackUnmatchedRequests toggles metrics for requests that do not resolve to a
	// registered Fiber route.
	//
	// Optional. Default: false.
	TrackUnmatchedRequests bool

	// UnmatchedRouteLabel is the path label used when TrackUnmatchedRequests is
	// enabled and a request does not match a registered route.
	//
	// This is a label value rather than a route, so unlike MetricsPath it is
	// taken as given: no leading slash is added. Set it to "unmatched" and the
	// path label reads "unmatched", which is useful to keep unmatched traffic
	// visibly distinct from real route patterns. Only trailing slashes are
	// trimmed, so that "/other/" and "/other" cannot become two series.
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

	// MetricsMaxRequestsInFlight limits how many scrapes the metrics endpoint
	// serves concurrently. Requests beyond the limit are answered with 503 so a
	// slow gatherer cannot pile up.
	//
	// Optional. Default: 0 (unlimited).
	MetricsMaxRequestsInFlight int

	// MetricsTimeout bounds how long a single scrape may take before it is
	// answered with 503.
	//
	// Optional. Default: 0 (no timeout).
	MetricsTimeout time.Duration

	// MetricsErrorLog receives errors encountered while gathering or writing
	// metrics, which are otherwise silent.
	//
	// Optional. Default: nil (errors are not logged).
	MetricsErrorLog promhttp.Logger

	// MetricsErrorHandling selects how gathering errors are reported to the
	// scraper.
	//
	// Optional. Default: promhttp.HTTPErrorOnError.
	MetricsErrorHandling promhttp.HandlerErrorHandling

	// DisabledMetrics lists metric families the middleware should not register
	// or record. Use it to drop families that are not worth their cardinality,
	// most commonly MetricRequestSize and MetricResponseSize.
	//
	// Optional. Default: none (every family is enabled).
	DisabledMetrics []Metric

	// SkipURIs excludes matching routes from instrumentation. Entries are
	// matched against the registered route pattern rather than the request
	// path, so use "/user/:id" instead of "/user/42". Trailing slashes are
	// ignored.
	//
	// An entry ending in "*" matches by prefix: "/admin/*" excludes "/admin"
	// and every route below it, and "/*" excludes everything.
	//
	// Optional. Default: none.
	SkipURIs []string

	// IgnoreStatusCodes excludes matching response status codes from metrics.
	// The status is the one the client receives, so codes produced by the
	// application error handler are matched as well.
	//
	// Optional. Default: none.
	IgnoreStatusCodes []int

	// IgnoreStatusClasses excludes whole status classes from metrics, saving
	// the need to enumerate every code. Valid entries are "1xx" through "5xx".
	//
	// Optional. Default: none.
	IgnoreStatusClasses []string

	// DynamicLabels adds labels whose values are computed per request, keyed by
	// label name. Each function runs once per recorded request, after the
	// handler chain has returned, so it can read anything the handlers left on
	// the context.
	//
	// The labels are added to every metric except MetricRequestsInProgress,
	// which is incremented before routing and so cannot see them. Names must
	// not collide with the built-in "status_code", "status_class", "method" and
	// "path" labels, or with Labels; New panics if they do.
	//
	// Every distinct value creates a new series, so returning request data
	// unchanged lets a client grow the registry without bound. Map untrusted
	// input onto a fixed set of values first.
	//
	// Returned values are copied, so a zero-copy string from c.Get or c.Params
	// is safe to return directly.
	//
	// Optional. Default: none.
	DynamicLabels map[string]func(fiber.Ctx) string

	// Next skips the middleware when it returns true. It runs before the
	// MetricsPath check, so returning true also stops the middleware from
	// serving a scrape.
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
	MetricsPath:            "/metrics",
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

	if cfg.MetricsPath == "" {
		cfg.MetricsPath = ConfigDefault.MetricsPath
	} else {
		cfg.MetricsPath = strings.Clone(cfg.MetricsPath)
		if !strings.HasPrefix(cfg.MetricsPath, "/") {
			cfg.MetricsPath = "/" + cfg.MetricsPath
		}
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

	if cfg.DynamicLabels == nil {
		cfg.DynamicLabels = make(map[string]func(fiber.Ctx) string)
	} else {
		dynamic := make(map[string]func(fiber.Ctx) string, len(cfg.DynamicLabels))
		for name, fn := range cfg.DynamicLabels {
			dynamic[name] = fn
		}
		cfg.DynamicLabels = dynamic
	}

	cfg.SkipURIs = append([]string(nil), cfg.SkipURIs...)
	cfg.IgnoreStatusCodes = append([]int(nil), cfg.IgnoreStatusCodes...)
	cfg.IgnoreStatusClasses = append([]string(nil), cfg.IgnoreStatusClasses...)
	cfg.DisabledMetrics = append([]Metric(nil), cfg.DisabledMetrics...)

	return cfg
}

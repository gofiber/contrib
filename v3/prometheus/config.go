package prometheus

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric identifies one of the metric families the middleware exposes. The
// values are the metric names before Namespace and Subsystem are applied.
type Metric string

// The metric families the middleware exposes; any can be turned off through
// Config.DisabledMetrics. The path label is the registered route pattern with
// trailing slashes trimmed. See the README for how payload sizes are measured.
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
	// ServiceName is added as the `service` const label on every metric.
	// Surrounding whitespace is trimmed; it has to be valid UTF-8.
	// Optional. Default: "" (label omitted).
	ServiceName string

	// Namespace prefixes every metric name. Surrounding whitespace is trimmed and the
	// result has to be a valid metric name, which New checks so client_golang cannot.
	// Optional. Default: "http".
	Namespace string

	// Subsystem prefixes every metric name after Namespace, trimmed and validated the
	// same way.
	// Optional. Default: "".
	Subsystem string

	// MetricsPath is the path served with the exposition format. Compared against the
	// full request path, case-sensitively, ignoring trailing slashes; on a group, set
	// it to the full path. Optional. Default: "/metrics".
	MetricsPath string

	// Labels are attached to every metric; a "service" key here is overridden by
	// ServiceName. Names must be valid UTF-8, must not start with "__" and must not
	// collide with a reserved label, or New panics. Optional. Default: no labels.
	Labels prometheus.Labels

	// Registerer is used to register metrics. Calling New twice with the same one
	// panics on duplicate registration; give each middleware its own registry, or a
	// distinct Namespace or Subsystem. Optional. Default: a private registry.
	Registerer prometheus.Registerer

	// Gatherer provides metrics to the HTTP handler. Supplying only one of the pair
	// panics unless it implements both; supplying both is trusted, so only a provably
	// distinct pair is rejected. Optional. Default: a private registry pair.
	Gatherer prometheus.Gatherer

	// DisableGoCollector skips registering the Go runtime metrics collector. A
	// registry that refuses it is reported to MetricsErrorLog rather than made fatal.
	// Optional. Default: false (collector enabled).
	DisableGoCollector bool

	// DisableProcessCollector skips registering the process metrics collector. A
	// refusal is handled as for DisableGoCollector.
	// Optional. Default: false (collector enabled).
	DisableProcessCollector bool

	// RequestDurationBuckets sets the request latency buckets; nil selects the
	// defaults. Bounds must be strictly increasing, with +Inf last only and never
	// alone and -Inf not at all, or New panics. Default: see ConfigDefault.
	RequestDurationBuckets []float64

	// RequestSizeBuckets sets the request payload size buckets; nil selects the
	// defaults. An empty non-nil slice only drops the classic buckets alongside
	// NativeHistogramBucketFactor. Default: see ConfigDefault.
	RequestSizeBuckets []float64

	// ResponseSizeBuckets sets the response payload size buckets; nil selects the
	// defaults, and the RequestSizeBuckets caveat applies.
	// Default: see ConfigDefault.
	ResponseSizeBuckets []float64

	// NativeHistogramBucketFactor enables native histograms above 1, capping the
	// growth between consecutive buckets - 1.1 is about 10% resolution. New panics on
	// any other non-zero value. Optional. Default: 0 (classic buckets only).
	NativeHistogramBucketFactor float64

	// NativeHistogramMaxBucketNumber bounds the native histogram buckets kept per
	// series; past it resolution drops, or the histogram resets.
	// Optional. Default: 0 (unlimited).
	NativeHistogramMaxBucketNumber uint32

	// NativeHistogramMinResetDuration is the minimum time before a native histogram
	// may be reset to control its bucket count.
	// Optional. Default: 0 (resets are allowed at any time).
	NativeHistogramMinResetDuration time.Duration

	// TrackUnmatchedRequests records requests that do not resolve to a registered
	// Fiber route. Known limitation: a request fasthttp rejects before routing is
	// counted as a 200 - see the README. Optional. Default: false.
	TrackUnmatchedRequests bool

	// UnmatchedRouteLabel is the path label used for unmatched requests. A label value
	// rather than a route, so no leading slash is added; whitespace and trailing
	// slashes are trimmed and it has to be valid UTF-8. Default: "/__unmatched__".
	UnmatchedRouteLabel string

	// EnableOpenMetrics exposes the experimental OpenMetrics encoding. Not what makes
	// exemplars reachable - the protobuf exposition carries them too.
	// Optional. Default: false.
	EnableOpenMetrics bool

	// EnableOpenMetricsTextCreatedSamples adds synthetic `_created` samples to
	// OpenMetrics responses. It needs EnableOpenMetrics to have any effect.
	// Optional. Default: false.
	EnableOpenMetricsTextCreatedSamples bool

	// DisableExemplars stops trace exemplars being attached to histogram
	// observations, and with them the request-context read every instrumented request
	// otherwise pays. Only sampled spans produce one. Optional. Default: false.
	DisableExemplars bool

	// DisableCompression prevents gzip and zstd compression of metrics responses,
	// even when the client asks for it.
	// Optional. Default: false.
	DisableCompression bool

	// MetricsMaxRequestsInFlight caps concurrent scrapes, answering the excess with
	// 503. New panics on a negative, which promhttp would read as unlimited.
	// Optional. Default: 0 (unlimited).
	MetricsMaxRequestsInFlight int

	// MetricsTimeout bounds a single scrape before it is answered with 503. New panics
	// on a negative, as for MetricsMaxRequestsInFlight.
	// Optional. Default: 0 (no timeout).
	MetricsTimeout time.Duration

	// MetricsErrorLog receives gather and write errors, plus the faults the middleware
	// absorbs: a panicking DynamicLabels or Next, and a refused runtime collector. A
	// typed nil panics. Optional. Default: nil (errors are not logged).
	MetricsErrorLog promhttp.Logger

	// MetricsErrorHandling selects how gathering errors reach the scraper. New panics
	// on a value outside promhttp's three; avoid PanicOnError, whose panic unwinds to
	// the unguarded connection goroutine. Default: promhttp.HTTPErrorOnError.
	MetricsErrorHandling promhttp.HandlerErrorHandling

	// DisabledMetrics lists families to skip registering and recording, most often the
	// two size histograms. Entries are trimmed and blanks skipped; New panics on a
	// name that is not a Metric constant. Optional. Default: none.
	DisabledMetrics []Metric

	// SkipURIs excludes matching routes, matched case-sensitively against the
	// registered route pattern - "/user/:id", not "/user/42". A trailing "*" matches
	// by prefix, and an entry may name UnmatchedRouteLabel. See the README.
	SkipURIs []string

	// SkipStatusCodes excludes response status codes, as the client received them, so
	// codes the error handler produced match too. New panics on anything but three
	// digits. Optional. Default: none.
	SkipStatusCodes []int

	// SkipStatusClasses excludes whole status classes: "1xx" through "5xx" and
	// "unknown". Case and whitespace are ignored and blanks skipped; New panics on
	// anything else. Optional. Default: none.
	SkipStatusClasses []string

	// DynamicLabels adds labels computed per request, after the handler chain has
	// returned. Names follow the Labels rules. Every distinct value is a new series,
	// so map untrusted input first. See the README. Optional. Default: none.
	DynamicLabels map[string]func(fiber.Ctx) string

	// Next skips the middleware when it returns true, including for MetricsPath, and
	// the chain's error is then returned unchanged. A panic is read as false and
	// reported once to MetricsErrorLog. Optional. Default: nil.
	Next func(fiber.Ctx) bool
}

var (
	defaultRequestDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10, 15, 30, 60}
	defaultRequestSizeBuckets     = []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}
	defaultResponseSizeBuckets    = []float64{256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 5242880}
)

// ConfigDefault holds the default middleware configuration. New reads it when no
// Config is supplied, so a package-level edit - a Next gating the metrics
// endpoint, say - reaches the argument-less call. The bucket fields are the one
// exception: they are shared with every copy taken from here, so an argument-less
// New takes its bounds from the package's own arrays rather than from whatever
// those fields now hold. Pass a Config to choose different buckets.
var ConfigDefault = Config{
	Namespace:           "http",
	MetricsPath:         "/metrics",
	UnmatchedRouteLabel: "/__unmatched__",
	// Copies, so that a caller who starts from ConfigDefault and adjusts one
	// bound in place cannot reshape the defaults every later New falls back to.
	RequestDurationBuckets: slices.Clone(defaultRequestDurationBuckets),
	RequestSizeBuckets:     slices.Clone(defaultRequestSizeBuckets),
	ResponseSizeBuckets:    slices.Clone(defaultResponseSizeBuckets),
}

// cloneBuckets returns a private copy of the supplied bounds, falling back to the
// defaults. The copy matters because client_golang aliases the slice it is handed
// into the live histogram.
func cloneBuckets(supplied, defaults []float64) []float64 {
	if supplied == nil {
		supplied = defaults
	}
	// slices.Clone keeps an empty non-nil slice non-nil, which Config documents
	// as meaning something different from nil: drop the classic buckets, rather
	// than take the defaults.
	return slices.Clone(supplied)
}

// configDefault fills in the defaults and copies the two things that outlive the
// call: the bucket bounds, which client_golang aliases, and Labels, which New
// writes "service" into.
func configDefault(config ...Config) Config {
	cfg := ConfigDefault
	if len(config) > 0 {
		cfg = config[0]
	} else {
		// ConfigDefault is exported, and its bucket slices are shared with every
		// copy a caller made from it, so one bound edited in place on any of those
		// copies is a bound edited here. Dropping them sends the fallback below to
		// the private arrays, so that "no config" keeps meaning the package
		// defaults - a caller who edited one into a non-increasing sequence gets a
		// panic on their own New, not on everyone else's.
		cfg.RequestDurationBuckets = nil
		cfg.RequestSizeBuckets = nil
		cfg.ResponseSizeBuckets = nil
	}

	// Trimmed and cloned: this becomes a const label on every family, so a trailing
	// newline would leave the metrics matching no dashboard, and TrimSpace returns a
	// view that would pin whatever blob the value was sliced out of.
	cfg.ServiceName = strings.Clone(strings.TrimSpace(cfg.ServiceName))

	// Trimmed like the others: these prefix every metric name, and under the
	// default UTF-8 validation scheme a stray newline is accepted rather than
	// rejected, so it silently renames all six families instead of failing.
	cfg.Namespace = strings.TrimSpace(cfg.Namespace)
	cfg.Subsystem = strings.TrimSpace(cfg.Subsystem)

	if cfg.Namespace == "" {
		cfg.Namespace = ConfigDefault.Namespace
	}

	// Trimmed like the list-valued options, so that a value carrying the
	// trailing newline an environment variable or mounted secret picks up does
	// not silently make the scrape endpoint unreachable.
	cfg.MetricsPath = strings.TrimSpace(cfg.MetricsPath)
	if cfg.MetricsPath == "" {
		cfg.MetricsPath = ConfigDefault.MetricsPath
	} else {
		// Concatenating already detaches the string from the caller's backing
		// array, which is the only thing the clone is there for.
		if strings.HasPrefix(cfg.MetricsPath, "/") {
			cfg.MetricsPath = strings.Clone(cfg.MetricsPath)
		} else {
			cfg.MetricsPath = "/" + cfg.MetricsPath
		}
	}

	// Trimmed for the same reason as MetricsPath: a value carrying a trailing
	// newline would become part of every unmatched series' label, valid UTF-8
	// and matched by no dashboard, recording rule or SkipURIs entry.
	cfg.UnmatchedRouteLabel = strings.TrimSpace(cfg.UnmatchedRouteLabel)
	if cfg.UnmatchedRouteLabel == "" {
		cfg.UnmatchedRouteLabel = ConfigDefault.UnmatchedRouteLabel
	} else {
		cfg.UnmatchedRouteLabel = strings.Clone(cfg.UnmatchedRouteLabel)
	}

	// From the private arrays rather than ConfigDefault, which is exported and
	// so is a value a caller may have edited.
	cfg.RequestDurationBuckets = cloneBuckets(cfg.RequestDurationBuckets, defaultRequestDurationBuckets)
	cfg.RequestSizeBuckets = cloneBuckets(cfg.RequestSizeBuckets, defaultRequestSizeBuckets)
	cfg.ResponseSizeBuckets = cloneBuckets(cfg.ResponseSizeBuckets, defaultResponseSizeBuckets)

	if cfg.Labels == nil {
		cfg.Labels = make(prometheus.Labels)
	} else {
		cfg.Labels = maps.Clone(cfg.Labels)
	}

	return cfg
}

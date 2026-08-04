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
//
// MetricRequestSize and MetricResponseSize record a payload only when its size
// is known: either Content-Length is set, or the body is buffered and can be
// measured. A stream of unannounced length - c.SendStream without a size, an SSE
// response, a chunked upload read through fiber.Config.StreamRequestBody - is
// left out of the histogram rather than recorded as zero bytes.
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
	//
	// Optional. Default: "" (label omitted).
	ServiceName string

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
	// middleware itself and are not instrumented.
	//
	// It is compared against the full request path, case-sensitively, with
	// trailing slashes ignored on both sides - "/metrics/" is served too. A
	// leading slash is added when missing. Two consequences: mounting the
	// middleware on a group leaves the default endpoint unreachable, because a
	// request to "/api/metrics" never equals "/metrics" and one to "/metrics"
	// never reaches the group - set this to the full path, "/api/metrics". And
	// because Fiber routes case-insensitively by default while this comparison
	// does not, "/METRICS" is instrumented as an ordinary request rather than
	// answered.
	//
	// Optional. Default: "/metrics".
	MetricsPath string

	// Labels are attached to every metric. A "service" key here is overridden by
	// ServiceName when that is also set.
	//
	// Optional. Default: no labels.
	Labels prometheus.Labels

	// Registerer is used to register metrics.
	//
	// Calling New twice with the same Registerer panics with a duplicate
	// registration error: the metric families are registered eagerly, and only
	// the Go and process collectors tolerate being registered twice. Give each
	// middleware its own registry, or a distinct Namespace or Subsystem.
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
	// most commonly MetricRequestSize and MetricResponseSize. New panics on a
	// name that is not one of the Metric constants, which would otherwise
	// disable nothing and say nothing.
	//
	// MetricRequestsStatusClassTotal is worth a look too: status_class is a
	// function of status_code, so every query against it has an equivalent
	// against MetricRequestsTotal - rate(...status_class_total{status_class=
	// "5xx"}[5m]) is rate(...requests_total{status_code=~"5.."}[5m]). Keeping it
	// buys shorter queries at the price of a second series per route, method and
	// class.
	//
	// Optional. Default: none (every family is enabled).
	DisabledMetrics []Metric

	// SkipURIs excludes matching routes from instrumentation. Entries are
	// matched against the registered route pattern rather than the request
	// path, so use "/user/:id" instead of "/user/42" - fiberzap's option of the
	// same name matches the request path instead. Surrounding whitespace and
	// trailing slashes are ignored, and a leading slash is added when missing,
	// since route patterns always carry one. Blank entries are skipped; exclude
	// the root route by asking for "/" explicitly.
	//
	// An entry ending in "*" matches by prefix: "/admin/*" excludes "/admin"
	// and every route below it, and "/*" excludes everything. It also still
	// matches a route pattern named exactly that, since Fiber patterns may end
	// in "*" themselves - "/static*" excludes the route "/static*" as well as
	// anything under "/static".
	//
	// Optional. Default: none.
	SkipURIs []string

	// SkipStatusCodes excludes matching response status codes from metrics.
	// The status is the one the client receives, so codes produced by the
	// application error handler are matched as well.
	//
	// Codes are three digits, per RFC 9110; New panics on anything else, since
	// a typo such as 4040 could only ever filter nothing.
	//
	// Optional. Default: none.
	SkipStatusCodes []int

	// SkipStatusClasses excludes whole status classes from metrics, saving
	// the need to enumerate every code. Valid entries are "1xx" through "5xx"
	// and "unknown", the class of a response outside those ranges. Case and
	// surrounding whitespace are ignored; New panics on anything else, since an
	// entry that matches no class would filter nothing silently.
	//
	// Blank entries are skipped rather than rejected, so splitting an unset
	// environment variable on "," does not stop the process from booting.
	//
	// Optional. Default: none.
	SkipStatusClasses []string

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

// cloneBuckets returns a private copy of the supplied bucket bounds, falling
// back to the defaults when none were given. The copy matters because
// client_golang aliases the slice it is handed into the live histogram, so a
// caller mutating its own slice afterwards would otherwise reshape a metric.
func cloneBuckets(supplied, defaults []float64) []float64 {
	if supplied == nil {
		supplied = defaults
	}
	return append([]float64(nil), supplied...)
}

// configDefault fills in the defaults and takes private copies of the two
// things that outlive the call: the bucket bounds, which client_golang aliases
// into the live histogram, and Labels, which New writes the "service" key into.
// Everything else New drains into its own maps before returning, so a caller
// mutating it afterwards cannot reach anything.
//
// A missing config is the zero config: keeping one code path means the two
// cannot drift apart.
func configDefault(config ...Config) Config {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}

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

	cfg.RequestDurationBuckets = cloneBuckets(cfg.RequestDurationBuckets, ConfigDefault.RequestDurationBuckets)
	cfg.RequestSizeBuckets = cloneBuckets(cfg.RequestSizeBuckets, ConfigDefault.RequestSizeBuckets)
	cfg.ResponseSizeBuckets = cloneBuckets(cfg.ResponseSizeBuckets, ConfigDefault.ResponseSizeBuckets)

	if cfg.Labels == nil {
		cfg.Labels = make(prometheus.Labels)
	} else {
		labels := make(prometheus.Labels, len(cfg.Labels))
		for key, value := range cfg.Labels {
			labels[key] = value
		}
		cfg.Labels = labels
	}

	return cfg
}

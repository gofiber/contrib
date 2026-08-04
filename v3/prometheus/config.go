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

// The metric families the middleware exposes. Any of them can be turned off
// through Config.DisabledMetrics.
//
// MetricRequestSize and MetricResponseSize record a payload only when its size
// is known: either Content-Length is set, or the body is buffered and can be
// measured. A stream of unannounced length - c.SendStream without a size, an SSE
// response, a chunked upload read through fiber.Config.StreamRequestBody - is
// left out of the histogram rather than recorded as zero bytes. On the request
// side what arrived is measured, so a client cannot bill the histogram for a
// body it never sent; the exception is a pre-parsed multipart form, where the
// buffer is not the body and reading it back would re-marshal every uploaded
// part into memory just to size it, so the announced length is used - unless it
// exceeds fiber.Config.BodyLimit, which means the body was never received in
// full and there is no honest size to record. A response that
// carries no body on the wire records zero however much the handler wrote: a
// HEAD, or any status RFC 9110 forbids a body on - 1xx, 204 and 304.
//
// The path label is the registered route pattern with trailing slashes trimmed,
// so under fiber.Config.StrictRouting two routes differing only by a trailing
// slash - "/foo" and "/foo/" - share one series. Trimming is what lets a
// SkipURIs entry match a pattern however it was spelled.
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

	// Namespace prefixes every metric name. It has to be valid UTF-8; New
	// panics otherwise, since client_golang would only reject it once the first
	// descriptor was built.
	//
	// Optional. Default: "http".
	Namespace string

	// Subsystem prefixes every metric name after Namespace. It has to be valid
	// UTF-8, as Namespace does.
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
	// Names must not be empty, must be valid UTF-8, must not begin with "__"
	// (which Prometheus keeps for itself), and must not collide with the
	// reserved "status_code", "status_class", "method", "path" and "le" labels;
	// values have to be valid UTF-8 too. New panics on any of those.
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
	// Bounds must be strictly increasing, with +Inf allowed only last. New
	// panics otherwise: client_golang checks them when it builds the first
	// histogram for a label set, which is on a request rather than at startup.
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
	// New panics on anything else non-zero - a negative value, or one between 0
	// and 1. client_golang enables native histograms above 1 only, and then
	// substitutes its latency defaults for any bucket slice deliberately left
	// empty, so 0.1 where 1.1 was meant would leave the byte histograms
	// bucketed in seconds with every real payload in +Inf alone.
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
	// Known limitation: a request fasthttp rejects before routing - a body over
	// fiber.Config.BodyLimit, headers over the buffer size, a read timeout - is
	// counted here as a 200. Fiber answers those through App.serverErrorHandler,
	// which replays the Use chain with non-Use routes skipped and writes the
	// real status only afterwards, so this middleware sees a chain that returned
	// no error and a response nothing has touched. Fiber v3.4.0 exposes no way
	// to tell that replay apart from an ordinary request answered by Use
	// handlers, which is recorded here too and must be. Leave this off if such
	// traffic would distort the counters more than missing genuine 404s would.
	//
	// Optional. Default: false.
	TrackUnmatchedRequests bool

	// UnmatchedRouteLabel is the path label used when TrackUnmatchedRequests is
	// enabled and a request does not match a registered route.
	//
	// This is a label value rather than a route, so unlike MetricsPath it is
	// taken as given: no leading slash is added. Set it to "unmatched" and the
	// path label reads "unmatched", which is useful to keep unmatched traffic
	// visibly distinct from real route patterns. Surrounding whitespace and
	// trailing slashes are trimmed - the first so that a value carrying the
	// newline an environment variable picks up does not become part of the
	// series name, the second so that "/other/" and "/other" cannot become two.
	//
	// It has to be valid UTF-8, which New checks: unlike Labels, this value
	// never passes through a descriptor, so client_golang would not see it until
	// the first unmatched request.
	//
	// Optional. Default: "/__unmatched__".
	UnmatchedRouteLabel string

	// EnableOpenMetrics exposes the experimental OpenMetrics encoding.
	//
	// It is not what makes exemplars reachable: the protobuf exposition carries
	// them too, and that is what a Prometheus server negotiates once native
	// histograms are on. Set this for a scraper that wants OpenMetrics text.
	//
	// Optional. Default: false.
	EnableOpenMetrics bool

	// EnableOpenMetricsTextCreatedSamples adds synthetic `_created` samples to
	// OpenMetrics responses. It does nothing on its own: the samples reach only
	// a scrape that negotiated the OpenMetrics encoding, so EnableOpenMetrics
	// has to be set as well.
	//
	// Optional. Default: false.
	EnableOpenMetricsTextCreatedSamples bool

	// DisableExemplars stops the middleware from attaching a trace exemplar to
	// each histogram observation.
	//
	// Collecting one means reading the request context, and Fiber installs a
	// background context on the request when the application never set one -
	// which the request then has to clear again on release. An application with
	// no tracing middleware pays that on every instrumented request for
	// exemplars that can never be produced, so turn this on when nothing in the
	// stack starts spans.
	//
	// Reaching a scraper takes an encoding that carries exemplars: OpenMetrics
	// text, which EnableOpenMetrics offers, or protobuf, which promhttp
	// negotiates without it.
	//
	// Only sampled spans produce one. Prometheus keeps a single exemplar per
	// bucket and overwrites it on each observation, so recording unsampled
	// traces would evict the links that lead somewhere.
	//
	// Optional. Default: false (exemplars collected when a span is present).
	DisableExemplars bool

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
	// metrics, which are otherwise silent. A typed nil panics: promhttp would
	// accept it and dereference it on the first gather failure instead.
	//
	// Optional. Default: nil (errors are not logged).
	MetricsErrorLog promhttp.Logger

	// MetricsErrorHandling selects how gathering errors are reported to the
	// scraper.
	//
	// Avoid promhttp.PanicOnError: the panic unwinds to the fasthttp connection
	// goroutine, which neither fasthttp nor Fiber guards, so a single failing
	// scrape takes the process down.
	//
	// Optional. Default: promhttp.HTTPErrorOnError.
	MetricsErrorHandling promhttp.HandlerErrorHandling

	// DisabledMetrics lists metric families the middleware should not register
	// or record. Use it to drop families that are not worth their cardinality,
	// most commonly MetricRequestSize and MetricResponseSize. New panics on a
	// name that is not one of the Metric constants, which would otherwise
	// disable nothing and say nothing. Surrounding whitespace is trimmed and
	// blank entries are skipped, as in the skip lists, so an environment
	// variable split on "," works whether it is unset or padded.
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
	// Matching is case-sensitive against the pattern as registered, while Fiber
	// routes case-insensitively by default: a route registered as "/Admin"
	// serves GET /admin and is labelled "/Admin", so an entry of "/admin" would
	// exclude nothing. Spell the entry the way the route was registered.
	//
	// An entry may also name UnmatchedRouteLabel, which excludes the traffic
	// TrackUnmatchedRequests would otherwise record. The label is matched as a
	// whole value rather than as a path, so "/__unmatched__" and
	// "/__unmatched__*" both exclude it - the star is stripped and the
	// remainder compared - but no prefix rule applies beyond that.
	//
	// An entry ending in "*" matches by prefix: "/admin/*" excludes "/admin"
	// and every route below it. Trailing stars are stripped as a group, so the
	// glob spelling "/admin/**" means the same thing. "/*" excludes everything,
	// and then no metric family is registered at all - not even
	// MetricRequestsInProgress, which is otherwise incremented before routing
	// and so beyond the reach of any per-route filter.
	//
	// Such an entry also still matches a route pattern named exactly that,
	// since Fiber patterns may end in "*" themselves - "/static*" excludes the
	// route "/static*" as well as anything under "/static".
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
	// which is incremented before routing and so cannot see them. Names follow
	// the same rules as Labels: not empty, valid UTF-8, and clear of the
	// reserved "status_code", "status_class", "method", "path" and "le" labels
	// as well as of Labels itself. New panics if they are not.
	//
	// Every distinct value creates a new series, so returning request data
	// unchanged lets a client grow the registry without bound. Map untrusted
	// input onto a fixed set of values first.
	//
	// A function that panics costs its request every metric rather than the
	// request itself: the sample is dropped and the response is unaffected. The
	// middleware cannot do better, since it calls these after the handler chain
	// has unwound, past any recover the application mounted - letting the panic
	// through would kill the connection instead. Guard your type assertions
	// rather than relying on that.
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
	// slices.Clone keeps an empty non-nil slice non-nil, which Config documents
	// as meaning something different from nil: drop the classic buckets, rather
	// than take the defaults.
	return slices.Clone(supplied)
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

	cfg.RequestDurationBuckets = cloneBuckets(cfg.RequestDurationBuckets, ConfigDefault.RequestDurationBuckets)
	cfg.RequestSizeBuckets = cloneBuckets(cfg.RequestSizeBuckets, ConfigDefault.RequestSizeBuckets)
	cfg.ResponseSizeBuckets = cloneBuckets(cfg.ResponseSizeBuckets, ConfigDefault.ResponseSizeBuckets)

	if cfg.Labels == nil {
		cfg.Labels = make(prometheus.Labels)
	} else {
		cfg.Labels = maps.Clone(cfg.Labels)
	}

	return cfg
}

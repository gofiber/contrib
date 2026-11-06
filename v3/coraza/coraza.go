// Package coraza provides Coraza WAF middleware for Fiber.
package coraza

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/corazawaf/coraza/v3"
	"github.com/corazawaf/coraza/v3/experimental"
	"github.com/corazawaf/coraza/v3/types"
	"github.com/gofiber/fiber/v3"
	fiberlog "github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/valyala/fasthttp"
)

const defaultBlockMessage = "Request blocked by Web Application Firewall"

// maxContentCodings bounds the decoders run per request, each of which may inflate to BodyLimit.
const maxContentCodings = 5

var (
	errRequestBodyDecode = errors.New("failed to decode request body")
	errMultipartForm     = errors.New("multipart form cannot be inspected as handlers read it")
)

// Config defines the configuration for the Coraza middleware and Engine.
//
// For zero-value-backed fields such as RequestBodyAccess=false,
// LogLevel=fiberlog.LevelTrace, or resetting MetricsCollector to the built-in
// default, start from ConfigDefault or use the WithRequestBodyAccess,
// WithLogLevel, and WithMetricsCollector helpers so the override remains
// explicit.
type Config struct {
	// Next defines a function to skip this middleware when it returns true.
	Next func(fiber.Ctx) bool
	// BlockHandler customizes the response returned for interrupted requests.
	BlockHandler BlockHandler
	// ErrorHandler customizes the response returned for middleware failures.
	ErrorHandler ErrorHandler
	// DirectivesFile lists Coraza directives files to load in order.
	// When empty, the engine starts without external rule files.
	DirectivesFile []string
	// RootFS is an optional filesystem used to resolve DirectivesFile entries.
	RootFS fs.FS
	// BlockMessage overrides the message used by the built-in block handler.
	BlockMessage string
	// LogLevel controls middleware lifecycle logging.
	LogLevel fiberlog.Level
	// RequestBodyAccess enables request body inspection in Coraza.
	RequestBodyAccess bool
	// MetricsCollector overrides the default in-memory metrics collector.
	MetricsCollector MetricsCollector

	logLevelSet          bool
	requestBodyAccessSet bool
	metricsCollectorSet  bool
}

// ConfigDefault provides the default Coraza configuration.
var ConfigDefault = Config{
	LogLevel:             fiberlog.LevelInfo,
	RequestBodyAccess:    true,
	logLevelSet:          true,
	requestBodyAccessSet: true,
}

// MiddlewareConfig customizes how Engine middleware behaves for a specific mount.
type MiddlewareConfig struct {
	// Next bypasses WAF inspection when it returns true.
	Next func(fiber.Ctx) bool
	// BlockHandler customizes the response returned for interrupted requests.
	BlockHandler BlockHandler
	// ErrorHandler customizes the response returned for middleware failures.
	ErrorHandler ErrorHandler
}

// MiddlewareError describes an operational failure that occurred while handling a request.
type MiddlewareError struct {
	// StatusCode is the HTTP status code suggested for the failure response.
	StatusCode int
	// Code is a stable application-level error code for the failure type.
	Code string
	// Message is the client-facing error message.
	Message string
	// Err is the underlying error when one is available.
	Err error
}

// InterruptionDetails describes a Coraza interruption returned by request inspection.
type InterruptionDetails struct {
	// StatusCode is the HTTP status code associated with the interruption.
	StatusCode int
	// Action is the Coraza action, such as "deny".
	Action string
	// RuleID is the matched Coraza rule identifier when available.
	RuleID int
	// Data contains rule-specific interruption data when available.
	Data string
	// Message is the message returned by the built-in block handler.
	Message string
}

// BlockHandler handles requests that were interrupted by the WAF.
type BlockHandler func(fiber.Ctx, InterruptionDetails) error

// ErrorHandler handles middleware errors that prevented request inspection.
type ErrorHandler func(fiber.Ctx, MiddlewareError) error

// Engine owns a Coraza WAF instance and exposes Fiber middleware around it.
type Engine struct {
	mu sync.RWMutex

	state *runtimeState

	waf             coraza.WAF
	wafWithOptions  experimental.WAFWithOptions
	supportsOptions bool
	initErr         error

	activeCfg      Config
	lastAttemptCfg Config
	blockMessage   string
	logLevel       fiberlog.Level
	metrics        MetricsCollector
	reloadCount    uint64
	lastLoadedAt   time.Time

	initSuccessCount   uint64
	initFailureCount   uint64
	reloadSuccessCount uint64
	reloadFailureCount uint64
}

type runtimeState struct {
	waf      coraza.WAF
	inflight sync.WaitGroup
}

type middlewareSnapshot struct {
	runtime         *runtimeState
	waf             coraza.WAF
	wafWithOptions  experimental.WAFWithOptions
	supportsOptions bool
	initErr         error
	metrics         MetricsCollector
	blockMessage    string
}

// New constructs Coraza Fiber middleware.
//
// It panics if the provided configuration cannot initialize a WAF instance.
func New(config ...Config) fiber.Handler {
	cfg := ConfigDefault
	if len(config) > 0 {
		cfg = resolveConfig(config[0])
	}

	engine, err := NewEngine(cfg)
	if err != nil {
		panic(err)
	}

	return engine.Middleware(MiddlewareConfig{
		Next:         cfg.Next,
		BlockHandler: cfg.BlockHandler,
		ErrorHandler: cfg.ErrorHandler,
	})
}

// NewEngine creates and initializes an Engine with the provided configuration.
func NewEngine(cfg Config) (*Engine, error) {
	engine := newEngine(nil)
	if err := engine.Init(cfg); err != nil {
		return nil, err
	}

	return engine, nil
}

// Init replaces the Engine's WAF instance using the provided configuration.
//
// On failure, the last working WAF instance is kept in place and the failure is
// recorded for observability. On success, Init blocks until requests still
// being inspected by the replaced WAF finish inspection, then closes it.
// Inspection ends before downstream handlers run, so the wait is bounded by
// WAF processing time, not by handler lifetimes.
func (e *Engine) Init(cfg Config) error {
	resolvedCfg := resolveConfig(cfg)
	metrics := resolveMetricsCollector(resolvedCfg.MetricsCollector)

	newWAF, err := createWAFWithConfig(resolvedCfg)
	logLevel := normalizeLogLevel(resolvedCfg.LogLevel)

	e.mu.Lock()
	e.lastAttemptCfg = cloneConfig(resolvedCfg)

	if err != nil {
		e.initErr = err
		e.initFailureCount++
		e.mu.Unlock()
		logWithLevel(logLevel, fiberlog.LevelError, "Coraza initialization failed", "error", err.Error())
		return err
	}

	oldState := e.state
	blockMessage := resolveBlockMessage(resolvedCfg.BlockMessage)
	newState := newRuntimeState(newWAF)
	e.waf = newWAF
	e.initErr = nil
	e.setWAFOptionsStateLocked(newWAF)
	e.activeCfg = cloneConfig(resolvedCfg)
	e.lastLoadedAt = time.Now()
	e.initSuccessCount++
	e.blockMessage = blockMessage
	e.logLevel = logLevel
	e.metrics = metrics
	e.state = newState
	supportsOptions := e.supportsOptions
	e.mu.Unlock()

	drainAndCloseState(oldState, logLevel)
	logWithLevel(logLevel, fiberlog.LevelInfo, "Coraza initialized successfully", "supports_options", supportsOptions)
	return nil
}

// SetBlockMessage overrides the default message returned by the built-in block handler.
func (e *Engine) SetBlockMessage(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.blockMessage = resolveBlockMessage(msg)
}

// Metrics returns the Engine's metrics collector.
func (e *Engine) Metrics() MetricsCollector {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.metrics
}

// Middleware creates a Fiber middleware handler backed by the Engine's WAF instance.
func (e *Engine) Middleware(config ...MiddlewareConfig) fiber.Handler {
	mwCfg := MiddlewareConfig{}
	if len(config) > 0 {
		mwCfg = config[0]
	}

	return func(c fiber.Ctx) error {
		if mwCfg.Next != nil && mwCfg.Next(c) {
			return c.Next()
		}

		startTime := time.Now()
		state := e.middlewareSnapshot()
		blocked := false

		// The in-flight registration must only cover actual WAF usage, which
		// ends when inspectRequest returns (the transaction is closed there).
		// Holding it across c.Next() would make Init/Reload drains wait on
		// downstream handlers and self-deadlock when Reload is called from a
		// handler running behind this middleware.
		released := false
		releaseInflight := func() {
			if !released {
				released = true
				if state.runtime != nil {
					state.runtime.inflight.Done()
				}
			}
		}

		defer func() {
			releaseInflight()
			if state.metrics != nil {
				state.metrics.ObserveRequest(time.Since(startTime), blocked)
			}
		}()

		if state.waf == nil {
			if state.initErr != nil {
				return e.handleError(c, mwCfg, MiddlewareError{
					StatusCode: http.StatusInternalServerError,
					Code:       "waf_init_failed",
					Message:    "WAF initialization failed",
					Err:        state.initErr,
				})
			}

			return e.handleError(c, mwCfg, MiddlewareError{
				StatusCode: http.StatusInternalServerError,
				Code:       "waf_not_initialized",
				Message:    "WAF instance not initialized",
			})
		}

		it, mwErr := e.inspectRequest(c, state.waf, state.supportsOptions, state.wafWithOptions)
		releaseInflight()
		if mwErr != nil {
			return e.handleError(c, mwCfg, *mwErr)
		}

		if it != nil {
			blocked = true

			details := InterruptionDetails{
				StatusCode: obtainStatusCodeFromInterruptionOrDefault(it, http.StatusForbidden),
				Action:     it.Action,
				RuleID:     it.RuleID,
				Data:       it.Data,
				Message:    state.blockMessage,
			}
			e.log(fiberlog.LevelWarn, "Coraza request interrupted",
				"rule_id", details.RuleID,
				"action", details.Action,
				"status", details.StatusCode)

			if mwCfg.BlockHandler != nil {
				return mwCfg.BlockHandler(c, details)
			}

			return defaultBlockHandler(c, details)
		}

		return c.Next()
	}
}

func (e *Engine) inspectRequest(
	c fiber.Ctx,
	currentWAF coraza.WAF,
	currentSupportsOptions bool,
	currentWAFWithOptions experimental.WAFWithOptions,
) (_ *types.Interruption, mwErr *MiddlewareError) {
	var tx types.Transaction

	defer func() {
		if r := recover(); r != nil {
			e.log(fiberlog.LevelError, "Coraza panic recovered",
				"panic", r,
				"method", c.Method(),
				"path", c.Path(),
				"ip", c.IP())

			mwErr = &MiddlewareError{
				StatusCode: http.StatusInternalServerError,
				Code:       "waf_panic_recovered",
				Message:    "WAF internal error",
				Err:        fmt.Errorf("panic recovered: %v", r),
			}
		}

		if tx != nil {
			e.finishTransaction(c, tx, &mwErr)
		}
	}()

	stdReq, err := convertFiberToStdRequest(c)
	if err != nil {
		return nil, &MiddlewareError{
			StatusCode: http.StatusInternalServerError,
			Code:       "waf_request_convert_failed",
			Message:    "Failed to convert request",
			Err:        err,
		}
	}

	if currentSupportsOptions && currentWAFWithOptions != nil {
		tx = currentWAFWithOptions.NewTransactionWithOptions(experimental.Options{
			Context: stdReq.Context(),
		})
	} else {
		tx = currentWAF.NewTransaction()
	}

	if tx.IsRuleEngineOff() {
		return nil, nil
	}

	it, err := processRequest(tx, stdReq, c)
	switch {
	case err == nil:
		return it, nil
	case errors.Is(err, errRequestBodyDecode):
		statusCode := http.StatusUnsupportedMediaType
		if errors.Is(err, fasthttp.ErrBodyTooLarge) {
			statusCode = http.StatusRequestEntityTooLarge
		}
		return nil, &MiddlewareError{
			StatusCode: statusCode,
			Code:       "waf_request_body_decode_failed",
			Message:    "Failed to decode request body",
			Err:        err,
		}
	case errors.Is(err, errMultipartForm):
		return nil, &MiddlewareError{
			StatusCode: http.StatusBadRequest,
			Code:       "waf_multipart_form_invalid",
			Message:    "Failed to inspect multipart form",
			Err:        err,
		}
	default:
		return nil, &MiddlewareError{
			StatusCode: http.StatusInternalServerError,
			Code:       "waf_request_processing_failed",
			Message:    "WAF request processing failed",
			Err:        err,
		}
	}
}

func (e *Engine) finishTransaction(c fiber.Ctx, tx types.Transaction, mwErr **MiddlewareError) {
	defer func() {
		if r := recover(); r != nil {
			e.log(fiberlog.LevelError, "Coraza cleanup panic recovered",
				"panic", r,
				"method", c.Method(),
				"path", c.Path(),
				"ip", c.IP())

			if *mwErr == nil {
				*mwErr = &MiddlewareError{
					StatusCode: http.StatusInternalServerError,
					Code:       "waf_cleanup_panic_recovered",
					Message:    "WAF internal error",
					Err:        fmt.Errorf("cleanup panic recovered: %v", r),
				}
			}
		}
	}()

	tx.ProcessLogging()
	if err := tx.Close(); err != nil {
		e.log(fiberlog.LevelDebug, "Coraza transaction close failed", "error", err.Error())
	}
}

// Reload rebuilds the current WAF instance using the active configuration.
//
// On success, Reload blocks until requests still being inspected by the
// replaced WAF finish inspection, then closes it. Inspection ends before
// downstream handlers run, so the wait is bounded by WAF processing time and
// it is safe to call Reload from a handler running behind this middleware.
func (e *Engine) Reload() error {
	e.mu.RLock()
	cfg := cloneConfig(e.activeCfg)
	e.mu.RUnlock()

	logLevel := normalizeLogLevel(cfg.LogLevel)
	logWithLevel(logLevel, fiberlog.LevelInfo, "Coraza starting manual reload")

	newWAF, err := createWAFWithConfig(cfg)
	if err != nil {
		e.mu.Lock()
		e.reloadFailureCount++
		e.mu.Unlock()
		logWithLevel(logLevel, fiberlog.LevelError, "Coraza reload failed", "error", err.Error())
		return fmt.Errorf("failed to reload WAF: %w", err)
	}

	e.mu.Lock()
	oldState := e.state
	newState := newRuntimeState(newWAF)
	e.waf = newWAF
	e.initErr = nil
	e.setWAFOptionsStateLocked(newWAF)
	e.reloadCount++
	e.reloadSuccessCount++
	e.lastLoadedAt = time.Now()
	reloadCount := e.reloadCount
	e.logLevel = logLevel
	e.state = newState
	e.mu.Unlock()

	drainAndCloseState(oldState, logLevel)
	logWithLevel(logLevel, fiberlog.LevelInfo, "Coraza reload completed successfully", "reload_count", reloadCount)
	return nil
}

func closeWAF(waf coraza.WAF, logLevel fiberlog.Level) {
	if waf == nil {
		return
	}

	closer, ok := waf.(experimental.WAFCloser)
	if !ok {
		return
	}

	if err := closer.Close(); err != nil {
		logWithLevel(logLevel, fiberlog.LevelWarn, "Coraza WAF close failed", "error", err.Error())
	}
}

func drainAndCloseState(state *runtimeState, logLevel fiberlog.Level) {
	if state == nil || state.waf == nil {
		return
	}

	state.inflight.Wait()
	closeWAF(state.waf, logLevel)
}

func (e *Engine) middlewareSnapshot() middlewareSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	state := e.state
	if state == nil {
		return middlewareSnapshot{
			initErr:         e.initErr,
			waf:             e.waf,
			wafWithOptions:  e.wafWithOptions,
			supportsOptions: e.supportsOptions,
			metrics:         e.metrics,
			blockMessage:    e.blockMessage,
		}
	}
	state.inflight.Add(1)

	return middlewareSnapshot{
		runtime:         state,
		waf:             e.waf,
		wafWithOptions:  e.wafWithOptions,
		supportsOptions: e.supportsOptions,
		initErr:         e.initErr,
		metrics:         e.metrics,
		blockMessage:    e.blockMessage,
	}
}

func (e *Engine) setWAFOptionsStateLocked(waf coraza.WAF) {
	if wafWithOptions, ok := waf.(experimental.WAFWithOptions); ok {
		e.wafWithOptions = wafWithOptions
		e.supportsOptions = true
		return
	}

	e.wafWithOptions = nil
	e.supportsOptions = false
}

func (e *Engine) handleError(c fiber.Ctx, cfg MiddlewareConfig, mwErr MiddlewareError) error {
	if cfg.ErrorHandler != nil {
		return cfg.ErrorHandler(c, mwErr)
	}

	return defaultErrorHandler(c, mwErr)
}

func newEngine(collector MetricsCollector) *Engine {
	metrics := resolveMetricsCollector(collector)
	return &Engine{
		blockMessage: defaultBlockMessage,
		logLevel:     fiberlog.LevelInfo,
		metrics:      metrics,
		state:        newRuntimeState(nil),
	}
}

func newRuntimeState(waf coraza.WAF) *runtimeState {
	return &runtimeState{waf: waf}
}

func isNilMetricsCollector(collector MetricsCollector) bool {
	if collector == nil {
		return true
	}

	value := reflect.ValueOf(collector)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func resolveMetricsCollector(collector MetricsCollector) MetricsCollector {
	if isNilMetricsCollector(collector) {
		return NewDefaultMetricsCollector()
	}

	return collector
}

func (e *Engine) observabilitySnapshot() EngineSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var lastInitError string
	if e.initErr != nil {
		lastInitError = e.initErr.Error()
	}

	configFiles := append([]string(nil), e.activeCfg.DirectivesFile...)
	lastAttemptConfigFiles := append([]string(nil), e.lastAttemptCfg.DirectivesFile...)

	return EngineSnapshot{
		Initialized:            e.waf != nil,
		SupportsOptions:        e.supportsOptions,
		ConfigFiles:            configFiles,
		LastAttemptConfigFiles: lastAttemptConfigFiles,
		LastInitError:          lastInitError,
		LastLoadedAt:           e.lastLoadedAt,
		InitSuccessTotal:       e.initSuccessCount,
		InitFailureTotal:       e.initFailureCount,
		ReloadSuccessTotal:     e.reloadSuccessCount,
		ReloadFailureTotal:     e.reloadFailureCount,
		ReloadCount:            e.reloadCount,
	}
}

func defaultBlockHandler(c fiber.Ctx, details InterruptionDetails) error {
	c.Set("X-WAF-Blocked", "true")
	return fiber.NewError(details.StatusCode, details.Message)
}

func defaultErrorHandler(_ fiber.Ctx, mwErr MiddlewareError) error {
	return fiber.NewError(mwErr.StatusCode, mwErr.Message)
}

func processRequest(tx types.Transaction, req *http.Request, c fiber.Ctx) (*types.Interruption, error) {
	client, cport := splitRemoteAddr(req.RemoteAddr)

	tx.ProcessConnection(client, cport, "", 0)
	tx.ProcessURI(req.URL.String(), req.Method, req.Proto)

	for k, values := range req.Header {
		for _, v := range values {
			tx.AddRequestHeader(k, v)
		}
	}

	if req.Host != "" {
		tx.AddRequestHeader("Host", req.Host)
		tx.SetServerName(req.Host)
	}

	for _, te := range req.TransferEncoding {
		tx.AddRequestHeader("Transfer-Encoding", te)
	}

	if in := tx.ProcessRequestHeaders(); in != nil {
		return in, nil
	}

	if tx.IsRequestBodyAccessible() && req.Body != nil && req.Body != http.NoBody {
		if err := decodeRequestBody(c.Request(), req, c.App().Config().BodyLimit); err != nil {
			return nil, fmt.Errorf("%w: %w", errRequestBodyDecode, err)
		}

		// No cut at BodyLimit: a re-serialized form can be longer than what was received.
		body := io.Reader(req.Body)
		if c.IsMultipart() {
			form, err := multipartFormBody(c, req.Header.Get(fiber.HeaderContentType))
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errMultipartForm, err)
			}
			body = form
		}

		it, _, err := tx.ReadRequestBodyFrom(body)
		if err != nil {
			return nil, err
		}
		if it != nil {
			return it, nil
		}
	}

	return tx.ProcessRequestBody()
}

// decodeRequestBody undoes the Content-Encoding in place so Coraza and all readers see the same
// bytes; c.Body() alone would not do, as PostArgs read the encoded body and multipart only gunzips.
func decodeRequestBody(fiberReq *fasthttp.Request, req *http.Request, bodyLimit int) error {
	if len(req.Header[fiber.HeaderContentEncoding]) == 0 {
		return nil
	}

	var names, codings []string
	for k, v := range fiberReq.Header.All() {
		if !strings.EqualFold(string(k), fiber.HeaderContentEncoding) {
			continue
		}
		names = append(names, string(k))
		for coding := range strings.SplitSeq(string(v), ",") {
			if coding = strings.ToLower(strings.TrimSpace(coding)); coding != "" && coding != fiber.StrIdentity {
				codings = append(codings, coding)
			}
		}
	}
	if len(codings) == 0 || len(fiberReq.Body()) == 0 {
		return nil
	}
	if len(codings) > maxContentCodings {
		return fmt.Errorf("%d content codings exceed the limit of %d", len(codings), maxContentCodings)
	}

	// Codings are listed in the order they were applied.
	for _, coding := range slices.Backward(codings) {
		var (
			body []byte
			err  error
		)
		switch coding {
		case fiber.StrGzip, "x-gzip":
			body, err = fiberReq.BodyGunzipWithLimit(bodyLimit)
		case fiber.StrBr, fiber.StrBrotli:
			body, err = fiberReq.BodyUnbrotliWithLimit(bodyLimit)
		case fiber.StrDeflate:
			body, err = fiberReq.BodyInflateWithLimit(bodyLimit)
		case fiber.StrZstd:
			body, err = fiberReq.BodyUnzstdWithLimit(bodyLimit)
		default:
			err = fmt.Errorf("unsupported content coding %q", coding)
		}
		if err != nil {
			return err
		}
		fiberReq.SetBodyRaw(body)
	}

	for _, name := range names {
		fiberReq.Header.Del(name)
	}
	body := fiberReq.Body()
	fiberReq.Header.SetContentLength(len(body))
	req.Body = io.NopCloser(bytes.NewReader(body))

	// A reader ahead of this middleware may have parsed PostArgs from the encoded body.
	postArgs := fiberReq.PostArgs()
	postArgs.Reset()
	if bytes.HasPrefix(fiberReq.Header.ContentType(), []byte(fiber.MIMEApplicationForm)) {
		postArgs.ParseBytes(body)
	}

	return nil
}

// multipartFormBody writes the form the handlers read under the boundary Coraza
// reads, so Coraza inspects exactly the parts the handlers see.
func multipartFormBody(c fiber.Ctx, contentType string) (io.Reader, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	boundary := params["boundary"]

	form, err := c.MultipartForm()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := fasthttp.WriteMultipartForm(&buf, form, boundary); err != nil {
		return nil, err
	}

	// The writer puts the delimiter once before each part and once at the end;
	// any other occurrence would end a part early for Coraza.
	parts := 0
	for _, values := range form.Value {
		parts += len(values)
	}
	for _, files := range form.File {
		parts += len(files)
	}
	if bytes.Count(buf.Bytes(), []byte("--"+boundary)) != parts+1 {
		return nil, errors.New("form content contains its boundary")
	}

	return &buf, nil
}

func obtainStatusCodeFromInterruptionOrDefault(it *types.Interruption, defaultStatusCode int) int {
	if it.Action == "deny" {
		if it.Status != 0 {
			return it.Status
		}
		return http.StatusForbidden
	}

	return defaultStatusCode
}

func convertFiberToStdRequest(c fiber.Ctx) (*http.Request, error) {
	req, err := adaptor.ConvertRequest(c, false)
	if err != nil {
		return nil, err
	}

	// Header values alias the Fiber request, whose Content-Length decodeRequestBody rewrites.
	if cl := req.Header[fiber.HeaderContentLength]; len(cl) > 0 && len(req.Header[fiber.HeaderContentEncoding]) > 0 {
		cl[0] = strings.Clone(cl[0])
	}

	req.RemoteAddr = net.JoinHostPort(c.IP(), c.Port())
	if req.Host == "" {
		req.Host = c.Hostname()
	}

	return req, nil
}

func createWAFWithConfig(cfg Config) (coraza.WAF, error) {
	var directivesFiles []string
	logLevel := normalizeLogLevel(cfg.LogLevel)
	for _, path := range cfg.DirectivesFile {
		expandedPaths, err := resolveDirectivesFiles(cfg.RootFS, path, logLevel)
		if err != nil {
			return nil, err
		}
		directivesFiles = append(directivesFiles, expandedPaths...)
	}

	wafConfig := coraza.NewWAFConfig()

	if cfg.RequestBodyAccess {
		wafConfig = wafConfig.WithRequestBodyAccess()
	}
	if cfg.RootFS != nil {
		wafConfig = wafConfig.WithRootFS(cfg.RootFS)
	}

	for _, path := range directivesFiles {
		wafConfig = wafConfig.WithDirectivesFromFile(path)
	}

	return coraza.NewWAF(wafConfig)
}

func resolveDirectivesFiles(root fs.FS, path string, logLevel fiberlog.Level) ([]string, error) {
	if strings.ContainsAny(path, "*?[") {
		logWithLevel(logLevel, fiberlog.LevelWarn,
			"Coraza directives path uses glob matching and is expanded before initialization",
			"path", path,
			"note", "all matching directives files will be loaded in sorted order",
		)

		var (
			matches []string
			err     error
		)
		if root != nil {
			matches, err = fs.Glob(root, path)
		} else {
			matches, err = filepath.Glob(path)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid Coraza directives glob %q: %w", path, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("coraza directives glob %q matched no files", path)
		}

		return matches, nil
	}

	if root != nil {
		if _, err := fs.Stat(root, path); err != nil {
			return nil, fmt.Errorf("coraza directives file %q not found in RootFS: %w", path, err)
		}
		return []string{path}, nil
	}

	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("coraza directives file %q not found: %w", path, err)
	}

	return []string{path}, nil
}

func splitRemoteAddr(remoteAddr string) (string, int) {
	host, port, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr, 0
	}

	portNum, err := strconv.Atoi(port)
	if err != nil {
		return host, 0
	}

	return host, portNum
}

func cloneConfig(cfg Config) Config {
	clone := cfg
	clone.DirectivesFile = append([]string(nil), cfg.DirectivesFile...)
	return clone
}

func resolveConfig(cfg Config) Config {
	resolved := ConfigDefault

	if cfg.Next != nil {
		resolved.Next = cfg.Next
	}
	if cfg.BlockHandler != nil {
		resolved.BlockHandler = cfg.BlockHandler
	}
	if cfg.ErrorHandler != nil {
		resolved.ErrorHandler = cfg.ErrorHandler
	}
	if cfg.DirectivesFile != nil {
		resolved.DirectivesFile = append([]string(nil), cfg.DirectivesFile...)
	}
	if cfg.RootFS != nil {
		resolved.RootFS = cfg.RootFS
	}
	if cfg.BlockMessage != "" {
		resolved.BlockMessage = cfg.BlockMessage
	}
	if cfg.logLevelSet || cfg.LogLevel != 0 {
		resolved.LogLevel = normalizeLogLevel(cfg.LogLevel)
	}
	if cfg.requestBodyAccessSet || cfg.RequestBodyAccess {
		resolved.RequestBodyAccess = cfg.RequestBodyAccess
	}
	if cfg.metricsCollectorSet || !isNilMetricsCollector(cfg.MetricsCollector) {
		resolved.MetricsCollector = cfg.MetricsCollector
	}

	resolved.logLevelSet = true
	resolved.requestBodyAccessSet = true
	resolved.metricsCollectorSet = cfg.metricsCollectorSet || !isNilMetricsCollector(cfg.MetricsCollector)

	return resolved
}

// WithLogLevel returns a copy of cfg with an explicit lifecycle log level.
func (cfg Config) WithLogLevel(level fiberlog.Level) Config {
	cfg.LogLevel = level
	cfg.logLevelSet = true
	return cfg
}

// WithRequestBodyAccess returns a copy of cfg with explicit request body inspection behavior.
func (cfg Config) WithRequestBodyAccess(enabled bool) Config {
	cfg.RequestBodyAccess = enabled
	cfg.requestBodyAccessSet = true
	return cfg
}

// WithMetricsCollector returns a copy of cfg with an explicit metrics collector choice.
func (cfg Config) WithMetricsCollector(collector MetricsCollector) Config {
	cfg.MetricsCollector = collector
	cfg.metricsCollectorSet = true
	return cfg
}

func resolveBlockMessage(msg string) string {
	if msg == "" {
		return defaultBlockMessage
	}

	return msg
}

func normalizeLogLevel(level fiberlog.Level) fiberlog.Level {
	switch level {
	case fiberlog.LevelTrace, fiberlog.LevelDebug, fiberlog.LevelInfo, fiberlog.LevelWarn, fiberlog.LevelError:
		return level
	default:
		return fiberlog.LevelInfo
	}
}

func logWithLevel(configLevel, targetLevel fiberlog.Level, msg string, keysAndValues ...any) {
	if normalizeLogLevel(configLevel) > normalizeLogLevel(targetLevel) {
		return
	}

	switch targetLevel {
	case fiberlog.LevelTrace:
		fiberlog.Tracew(msg, keysAndValues...)
	case fiberlog.LevelDebug:
		fiberlog.Debugw(msg, keysAndValues...)
	case fiberlog.LevelWarn:
		fiberlog.Warnw(msg, keysAndValues...)
	case fiberlog.LevelError:
		fiberlog.Errorw(msg, keysAndValues...)
	default:
		fiberlog.Infow(msg, keysAndValues...)
	}
}

func (e *Engine) currentLogLevel() fiberlog.Level {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.logLevel
}

func (e *Engine) log(targetLevel fiberlog.Level, msg string, keysAndValues ...any) {
	logWithLevel(e.currentLogLevel(), targetLevel, msg, keysAndValues...)
}

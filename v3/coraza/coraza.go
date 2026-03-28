// Package coraza provides Coraza WAF middleware for Fiber.
package coraza

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/corazawaf/coraza/v3"
	"github.com/corazawaf/coraza/v3/debuglog"
	"github.com/corazawaf/coraza/v3/experimental"
	"github.com/corazawaf/coraza/v3/types"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
)

const defaultBlockMessage = "Request blocked by Web Application Firewall"

// Config defines the configuration for the Coraza middleware and Engine.
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
	// RequestBodyAccess enables request body inspection in Coraza.
	RequestBodyAccess bool
	// RequestBodyLimit limits the maximum request body size Coraza will process.
	RequestBodyLimit int
	// RequestBodyInMemoryLimit limits how much request body data Coraza keeps in memory.
	RequestBodyInMemoryLimit int
	// DebugLogger enables Coraza debug logging when provided.
	DebugLogger debuglog.Logger
	// EnableErrorLog enables logging for matched rules via Coraza's error callback.
	EnableErrorLog bool
	// MetricsCollector overrides the default in-memory metrics collector.
	MetricsCollector MetricsCollector
}

// ConfigDefault provides the default Coraza configuration.
var ConfigDefault = Config{
	RequestBodyAccess:        true,
	RequestBodyLimit:         10 * 1024 * 1024,
	RequestBodyInMemoryLimit: 128 * 1024,
	EnableErrorLog:           true,
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

	waf             coraza.WAF
	wafWithOptions  experimental.WAFWithOptions
	supportsOptions bool
	initErr         error
	initialized     bool

	activeCfg      Config
	lastAttemptCfg Config
	blockMessage   string
	metrics        MetricsCollector
	reloadCount    uint64
	lastLoadedAt   time.Time

	initSuccessCount   uint64
	initFailureCount   uint64
	reloadSuccessCount uint64
	reloadFailureCount uint64
}

// New constructs Coraza Fiber middleware.
//
// It panics if the provided configuration cannot initialize a WAF instance.
func New(config ...Config) fiber.Handler {
	cfg := ConfigDefault
	if len(config) > 0 {
		cfg = config[0]
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
	engine := newEngine(cfg.MetricsCollector)
	if err := engine.Init(cfg); err != nil {
		return nil, err
	}

	return engine, nil
}

// Init replaces the Engine's WAF instance using the provided configuration.
//
// On failure, the last working WAF instance is kept in place and the failure is
// recorded for observability.
func (e *Engine) Init(cfg Config) error {
	newWAF, err := createWAFWithConfig(cfg)

	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastAttemptCfg = cloneConfig(cfg)

	if err != nil {
		e.initErr = err
		e.initFailureCount++
		slog.Error("[Coraza] initialization failed", "error", err.Error())
		return err
	}

	e.waf = newWAF
	e.initErr = nil
	e.setWAFOptionsStateLocked(newWAF)
	e.activeCfg = cloneConfig(cfg)
	e.initialized = true
	e.lastLoadedAt = time.Now()
	e.initSuccessCount++
	if cfg.BlockMessage != "" {
		e.blockMessage = cfg.BlockMessage
	}

	slog.Info("[Coraza] initialized successfully", "supports_options", e.supportsOptions)
	return nil
}

// SetBlockMessage overrides the default message returned by the built-in block handler.
func (e *Engine) SetBlockMessage(msg string) {
	if msg == "" {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.blockMessage = msg
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
		metrics := e.Metrics()
		metrics.RecordRequest()

		defer func() {
			metrics.RecordLatency(time.Since(startTime))
		}()

		currentWAF, currentErr, currentSupportsOptions, currentWAFWithOptions := e.snapshot()
		if currentWAF == nil {
			if currentErr != nil {
				return e.handleError(c, mwCfg, MiddlewareError{
					StatusCode: http.StatusInternalServerError,
					Code:       "waf_init_failed",
					Message:    "WAF initialization failed",
					Err:        currentErr,
				})
			}

			return e.handleError(c, mwCfg, MiddlewareError{
				StatusCode: http.StatusInternalServerError,
				Code:       "waf_not_initialized",
				Message:    "WAF instance not initialized",
			})
		}

		it, mwErr := e.inspectRequest(c, currentWAF, currentSupportsOptions, currentWAFWithOptions)
		if mwErr != nil {
			return e.handleError(c, mwCfg, *mwErr)
		}

		if it != nil {
			metrics.RecordBlock()

			details := InterruptionDetails{
				StatusCode: obtainStatusCodeFromInterruptionOrDefault(it, http.StatusForbidden),
				Action:     it.Action,
				RuleID:     it.RuleID,
				Data:       it.Data,
				Message:    e.blockMessageValue(),
			}

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
			slog.Error("Coraza panic recovered",
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

	it, err := processRequest(tx, stdReq)
	if err != nil {
		return nil, &MiddlewareError{
			StatusCode: http.StatusInternalServerError,
			Code:       "waf_request_processing_failed",
			Message:    "WAF request processing failed",
			Err:        err,
		}
	}

	return it, nil
}

func (e *Engine) finishTransaction(c fiber.Ctx, tx types.Transaction, mwErr **MiddlewareError) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Coraza cleanup panic recovered",
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
	_ = tx.Close()
}

// Reload rebuilds the current WAF instance using the active configuration.
func (e *Engine) Reload() error {
	e.mu.RLock()
	cfg := cloneConfig(e.activeCfg)
	e.mu.RUnlock()

	if len(cfg.DirectivesFile) == 0 {
		return fmt.Errorf("no configuration available for reload")
	}

	slog.Info("[Coraza] starting manual reload")

	newWAF, err := createWAFWithConfig(cfg)
	if err != nil {
		e.mu.Lock()
		e.reloadFailureCount++
		e.mu.Unlock()
		slog.Error("[Coraza] reload failed", "error", err.Error())
		return fmt.Errorf("failed to reload WAF: %w", err)
	}

	e.mu.Lock()
	e.waf = newWAF
	e.initErr = nil
	e.setWAFOptionsStateLocked(newWAF)
	e.reloadCount++
	e.reloadSuccessCount++
	e.lastLoadedAt = time.Now()
	e.mu.Unlock()

	slog.Info("[Coraza] reload completed successfully", "reload_count", e.reloadCount)
	return nil
}

func (e *Engine) snapshot() (coraza.WAF, error, bool, experimental.WAFWithOptions) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.waf, e.initErr, e.supportsOptions, e.wafWithOptions
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

func (e *Engine) blockMessageValue() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.blockMessage
}

func (e *Engine) handleError(c fiber.Ctx, cfg MiddlewareConfig, mwErr MiddlewareError) error {
	if cfg.ErrorHandler != nil {
		return cfg.ErrorHandler(c, mwErr)
	}

	return defaultErrorHandler(c, mwErr)
}

func newEngine(collector MetricsCollector) *Engine {
	if collector == nil {
		collector = NewDefaultMetricsCollector()
	}

	return &Engine{
		blockMessage: defaultBlockMessage,
		metrics:      collector,
	}
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

func logError(rule types.MatchedRule) {
	slog.Warn("Coraza rule matched",
		slog.String("severity", rule.Rule().Severity().String()),
		slog.String("error_log", rule.ErrorLog()),
		slog.String("request_uri", rule.URI()),
		slog.Int("rule_id", rule.Rule().ID()),
		slog.String("rule_msg", rule.Message()),
		slog.Time("timestamp", time.Now()),
	)
}

func processRequest(tx types.Transaction, req *http.Request) (*types.Interruption, error) {
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
		it, _, err := tx.ReadRequestBodyFrom(req.Body)
		if err != nil {
			return nil, err
		}
		if it != nil {
			return it, nil
		}

		rbr, err := tx.RequestBodyReader()
		if err != nil {
			return nil, fmt.Errorf("failed to get WAF request body reader: %w", err)
		}
		req.Body = io.NopCloser(io.MultiReader(rbr, req.Body))
	}

	return tx.ProcessRequestBody()
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

	req.RemoteAddr = net.JoinHostPort(c.IP(), c.Port())
	if req.Host == "" {
		req.Host = c.Hostname()
	}

	return req, nil
}

func createWAFWithConfig(cfg Config) (coraza.WAF, error) {
	for _, path := range cfg.DirectivesFile {
		if err := validateDirectivesFile(cfg.RootFS, path); err != nil {
			return nil, err
		}
	}

	wafConfig := coraza.NewWAFConfig()

	if cfg.EnableErrorLog {
		wafConfig = wafConfig.WithErrorCallback(logError)
	}
	if cfg.RequestBodyAccess {
		wafConfig = wafConfig.WithRequestBodyAccess()
	}
	if cfg.RequestBodyLimit > 0 {
		wafConfig = wafConfig.WithRequestBodyLimit(cfg.RequestBodyLimit)
	}
	if cfg.RequestBodyInMemoryLimit > 0 {
		wafConfig = wafConfig.WithRequestBodyInMemoryLimit(cfg.RequestBodyInMemoryLimit)
	}
	if cfg.RootFS != nil {
		wafConfig = wafConfig.WithRootFS(cfg.RootFS)
	}
	if cfg.DebugLogger != nil {
		wafConfig = wafConfig.WithDebugLogger(cfg.DebugLogger)
	}

	for _, path := range cfg.DirectivesFile {
		wafConfig = wafConfig.WithDirectivesFromFile(path)
	}

	return coraza.NewWAF(wafConfig)
}

func validateDirectivesFile(root fs.FS, path string) error {
	if strings.Contains(path, "*") {
		return nil
	}

	if root != nil {
		if _, err := fs.Stat(root, path); err != nil {
			return fmt.Errorf("Coraza directives file %q not found in RootFS: %w", path, err)
		}
		return nil
	}

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("Coraza directives file %q not found: %w", path, err)
	}

	return nil
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

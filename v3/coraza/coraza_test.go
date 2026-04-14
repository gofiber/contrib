package coraza

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/corazawaf/coraza/v3/debuglog"
	"github.com/corazawaf/coraza/v3/types"
	"github.com/gofiber/fiber/v3"
	fiberlog "github.com/gofiber/fiber/v3/log"
)

const testRules = `SecRuleEngine On
SecRequestBodyAccess On
SecRule ARGS:attack "@streq 1" "id:1001,phase:2,deny,status:403,msg:'attack detected'"`

func TestNewPanicsOnInvalidConfig(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected New to panic when config is invalid")
		}
	}()

	_ = New(Config{DirectivesFile: []string{"missing.conf"}})
}

func TestNewWithoutConfigReturnsMiddleware(t *testing.T) {
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/", nil))
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("expected body ok, got %q", string(body))
	}
}

func TestNewEngineWithLocalFile(t *testing.T) {
	path := writeRuleFile(t, t.TempDir(), "local.conf", testRules)
	engine, err := NewEngine(Config{
		LogLevel:          fiberlog.LevelInfo,
		DirectivesFile:    []string{path},
		BlockMessage:      "blocked from config",
		RequestBodyAccess: true,
	})
	if err != nil {
		t.Fatalf("expected successful initialization, got error: %v", err)
	}

	if engine.initErr != nil {
		t.Fatalf("expected successful initialization, got error: %v", engine.initErr)
	}
	if engine.waf == nil {
		t.Fatal("expected engine WAF to be initialized")
	}
	if engine.blockMessage != "blocked from config" {
		t.Fatalf("expected block message to be initialized from config, got %q", engine.blockMessage)
	}
}

func TestSetBlockMessageEmptyResetsDefault(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	engine.SetBlockMessage("custom block")
	engine.SetBlockMessage("")

	if got := engine.blockMessageValue(); got != defaultBlockMessage {
		t.Fatalf("expected empty block message to restore default, got %q", got)
	}
}

func TestNewEngineWithRootFS(t *testing.T) {
	tempDir := t.TempDir()
	writeRuleFile(t, tempDir, "rootfs.conf", testRules)

	engine, err := NewEngine(Config{
		DirectivesFile:    []string{"rootfs.conf"},
		RootFS:            os.DirFS(tempDir),
		RequestBodyAccess: true,
	})
	if err != nil {
		t.Fatalf("expected RootFS initialization to succeed, got error: %v", err)
	}

	if engine.initErr != nil {
		t.Fatalf("expected RootFS initialization to succeed, got error: %v", engine.initErr)
	}
	if engine.waf == nil {
		t.Fatal("expected engine WAF to be initialized from RootFS")
	}
}

func TestNewEngineMissingFile(t *testing.T) {
	_, err := NewEngine(Config{
		DirectivesFile: []string{"missing.conf"},
	})
	if err == nil {
		t.Fatal("expected initialization to fail for missing directives file")
	}
}

func TestNewReturnsMiddleware(t *testing.T) {
	path := writeRuleFile(t, t.TempDir(), "test.conf", testRules)

	app := fiber.New()
	app.Use(New(Config{
		DirectivesFile:    []string{path},
		RequestBodyAccess: true,
	}))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?attack=1", nil))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", resp.StatusCode)
	}
}

func TestNewAppliesConfigDefaults(t *testing.T) {
	bodyRules := `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQUEST_BODY "@contains attack" "id:1002,phase:2,deny,status:403,msg:'body attack detected'"`

	path := writeRuleFile(t, t.TempDir(), "body.conf", bodyRules)

	app := fiber.New()
	app.Use(New(Config{
		DirectivesFile: []string{path},
	}))
	app.Post("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload=attack"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := performRequest(t, app, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403 when New applies default request body access, got %d", resp.StatusCode)
	}
}

func TestResolveConfigHonorsExplicitZeroValueOverrides(t *testing.T) {
	resolved := resolveConfig(Config{}.WithRequestBodyAccess(false).WithLogLevel(fiberlog.LevelTrace))

	if resolved.RequestBodyAccess {
		t.Fatal("expected explicit request body access override to remain false")
	}
	if resolved.LogLevel != fiberlog.LevelTrace {
		t.Fatalf("expected explicit trace log level override, got %v", resolved.LogLevel)
	}
}

func TestEngineMiddlewareAllowsCleanRequest(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{})
	req := httptest.NewRequest(http.MethodGet, "/?name=safe", nil)

	resp := performRequest(t, app, req)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("expected body ok, got %q", string(body))
	}

	metrics := engine.MetricsSnapshot()
	if metrics.TotalRequests != 1 || metrics.BlockedRequests != 0 {
		t.Fatalf("unexpected metrics after clean request: %+v", metrics)
	}
}

func TestEngineMiddlewareBlocksMaliciousRequest(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{})
	req := httptest.NewRequest(http.MethodGet, "/?attack=1", nil)

	resp := performRequest(t, app, req)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-WAF-Blocked") != "true" {
		t.Fatalf("expected X-WAF-Blocked header to be true, got %q", resp.Header.Get("X-WAF-Blocked"))
	}
	if !strings.Contains(string(body), defaultBlockMessage) {
		t.Fatalf("expected block message in response body, got %q", string(body))
	}

	metrics := engine.MetricsSnapshot()
	if metrics.TotalRequests != 1 || metrics.BlockedRequests != 1 {
		t.Fatalf("unexpected metrics after blocked request: %+v", metrics)
	}
}

func TestEngineMiddlewareBlocksMaliciousRequestBody(t *testing.T) {
	bodyRules := `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQUEST_BODY "@contains attack" "id:1002,phase:2,deny,status:403,msg:'body attack detected'"`

	engine, err := newTestEngineWithRules(t, bodyRules)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload=attack"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := performRequest(t, app, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403 for malicious body, got %d", resp.StatusCode)
	}

	metrics := engine.MetricsSnapshot()
	if metrics.TotalRequests != 1 || metrics.BlockedRequests != 1 {
		t.Fatalf("unexpected metrics after blocked body request: %+v", metrics)
	}
}

func TestEngineMiddlewareRespectsFiberBodyLimit(t *testing.T) {
	bodyRules := `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQUEST_BODY "@contains attack" "id:1002,phase:2,deny,status:403,msg:'body attack detected'"`

	engine, err := newTestEngineWithRules(t, bodyRules)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := fiber.New(fiber.Config{
		BodyLimit: 8,
	})
	app.Use(engine.Middleware())
	app.Post("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload=attack"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	_, err = app.Test(req)
	if err == nil {
		t.Fatal("expected Fiber body limit error, got nil")
	}
	if err.Error() != "body size exceeds the given limit" {
		t.Fatalf("expected Fiber body limit error, got %v", err)
	}
}

func TestNewEngineProvidesInstanceIsolation(t *testing.T) {
	first, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create first engine: %v", err)
	}
	second, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create second engine: %v", err)
	}

	first.SetBlockMessage("blocked by first")
	second.SetBlockMessage("blocked by second")

	firstApp := newInstanceApp(first, MiddlewareConfig{})
	secondApp := newInstanceApp(second, MiddlewareConfig{})

	firstResp := performRequest(t, firstApp, httptest.NewRequest(http.MethodGet, "/?attack=1", nil))
	defer firstResp.Body.Close()
	firstBody, err := io.ReadAll(firstResp.Body)
	if err != nil {
		t.Fatalf("failed to read first response body: %v", err)
	}

	secondResp := performRequest(t, secondApp, httptest.NewRequest(http.MethodGet, "/?attack=1", nil))
	defer secondResp.Body.Close()
	secondBody, err := io.ReadAll(secondResp.Body)
	if err != nil {
		t.Fatalf("failed to read second response body: %v", err)
	}

	if !strings.Contains(string(firstBody), "blocked by first") {
		t.Fatalf("expected first engine response to contain its block message, got %q", string(firstBody))
	}
	if !strings.Contains(string(secondBody), "blocked by second") {
		t.Fatalf("expected second engine response to contain its block message, got %q", string(secondBody))
	}
}

func TestMiddlewareConfigNextBypassesInspection(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{
		Next: func(c fiber.Ctx) bool {
			return c.Query("attack") == "1"
		},
	})

	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?attack=1", nil))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected skipped request to pass through, got %d", resp.StatusCode)
	}

	metrics := engine.MetricsSnapshot()
	if metrics.TotalRequests != 0 || metrics.BlockedRequests != 0 {
		t.Fatalf("expected skipped request not to affect metrics, got %+v", metrics)
	}
}

func TestMiddlewareConfigCustomBlockHandler(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{
		BlockHandler: func(c fiber.Ctx, details InterruptionDetails) error {
			c.Set("X-Custom-Block", "true")
			return c.Status(http.StatusTeapot).JSON(fiber.Map{
				"rule_id": details.RuleID,
				"status":  details.StatusCode,
			})
		},
	})

	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?attack=1", nil))
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read custom block response body: %v", err)
	}

	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("expected custom block status 418, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Custom-Block") != "true" {
		t.Fatalf("expected custom block header, got %q", resp.Header.Get("X-Custom-Block"))
	}
	if !strings.Contains(string(body), `"rule_id":1001`) {
		t.Fatalf("expected custom block body to include rule id, got %q", string(body))
	}
}

func TestMiddlewareConfigCustomErrorHandler(t *testing.T) {
	engine := newEngine(NewDefaultMetricsCollector())
	app := newInstanceApp(engine, MiddlewareConfig{
		ErrorHandler: func(c fiber.Ctx, mwErr MiddlewareError) error {
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{
				"error_code": mwErr.Code,
				"message":    mwErr.Message,
			})
		},
	})

	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/", nil))
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read custom error response body: %v", err)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected custom error status 503, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"error_code":"waf_not_initialized"`) {
		t.Fatalf("expected custom error code in body, got %q", string(body))
	}
}

func TestEngineReportIncludesLifecycleSnapshot(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	report := engine.Report()
	if !report.Engine.Initialized {
		t.Fatal("expected report to include initialized engine state")
	}
	if report.Engine.InitSuccessTotal != 1 {
		t.Fatalf("expected init success count to be 1, got %d", report.Engine.InitSuccessTotal)
	}
	if len(report.Engine.ConfigFiles) != 1 {
		t.Fatalf("expected one config file in report, got %+v", report.Engine.ConfigFiles)
	}
}

func TestMetricsSnapshotHandlesNilCollectorSnapshot(t *testing.T) {
	engine := newEngine(nilSnapshotCollector{})

	snapshot := engine.MetricsSnapshot()

	if snapshot.TotalRequests != 0 || snapshot.BlockedRequests != 0 || snapshot.AvgLatencyMs != 0 || snapshot.BlockRate != 0 {
		t.Fatalf("expected zero-value metrics snapshot, got %+v", snapshot)
	}
	if snapshot.Timestamp.IsZero() {
		t.Fatal("expected metrics snapshot timestamp to be populated")
	}
}

func TestNewEngineFallsBackToDefaultCollectorForTypedNilMetricsCollector(t *testing.T) {
	var collector MetricsCollector = (*nilPtrSnapshotCollector)(nil)

	engine := newEngine(collector)

	if engine.Metrics() == nil {
		t.Fatal("expected typed-nil metrics collector to fall back to the default collector")
	}

	app := newInstanceApp(engine, MiddlewareConfig{})
	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/", nil))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status 500 with uninitialized WAF, got %d", resp.StatusCode)
	}

	snapshot := engine.MetricsSnapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("expected fallback collector to record one request, got %+v", snapshot)
	}
}

func TestEngineInitFailureKeepsLastWorkingWAF(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{})

	allowedBefore := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?name=safe", nil))
	defer allowedBefore.Body.Close()
	if allowedBefore.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 before failed reinit, got %d", allowedBefore.StatusCode)
	}

	err = engine.Init(Config{DirectivesFile: []string{filepath.Join(t.TempDir(), "missing.conf")}})
	if err == nil {
		t.Fatal("expected reinitialization with missing config to fail")
	}

	allowedAfter := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?name=safe", nil))
	defer allowedAfter.Body.Close()
	body, err := io.ReadAll(allowedAfter.Body)
	if err != nil {
		t.Fatalf("failed to read response body after failed reinit: %v", err)
	}

	if allowedAfter.StatusCode != http.StatusOK {
		t.Fatalf("expected last working WAF to continue serving after failed reinit, got %d with body %q", allowedAfter.StatusCode, string(body))
	}

	snapshot := engine.Snapshot()
	if snapshot.LastInitError == "" {
		t.Fatal("expected engine snapshot to retain the last initialization error for observability")
	}
	if len(snapshot.ConfigFiles) != 1 || !strings.HasSuffix(snapshot.ConfigFiles[0], "test.conf") {
		t.Fatalf("expected active config to remain unchanged, got %+v", snapshot.ConfigFiles)
	}
	if len(snapshot.LastAttemptConfigFiles) != 1 || !strings.HasSuffix(snapshot.LastAttemptConfigFiles[0], "missing.conf") {
		t.Fatalf("expected last attempted config to be reported, got %+v", snapshot.LastAttemptConfigFiles)
	}
}

func TestEngineInitClosesPreviousWAFOnSuccess(t *testing.T) {
	engine := newEngine(NewDefaultMetricsCollector())
	oldWAF := &fakeClosableWAF{}

	engine.mu.Lock()
	engine.waf = oldWAF
	engine.mu.Unlock()

	path := writeRuleFile(t, t.TempDir(), "reinit.conf", testRules)
	if err := engine.Init(Config{
		DirectivesFile:    []string{path},
		RequestBodyAccess: true,
	}); err != nil {
		t.Fatalf("expected init to succeed, got %v", err)
	}

	if oldWAF.closeCalls != 1 {
		t.Fatalf("expected previous WAF to be closed once, got %d", oldWAF.closeCalls)
	}
}

func TestMiddlewareFailsClosedWhenWAFPanicOccurs(t *testing.T) {
	engine := newEngine(NewDefaultMetricsCollector())
	engine.waf = fakePanicWAF{}

	app := newInstanceApp(engine, MiddlewareConfig{})
	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?name=safe", nil))
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read panic recovery response body: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status 500 when WAF panics, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "WAF internal error") {
		t.Fatalf("expected WAF internal error response, got %q", string(body))
	}

	metrics := engine.MetricsSnapshot()
	if metrics.TotalRequests != 1 || metrics.BlockedRequests != 0 {
		t.Fatalf("unexpected metrics after panic recovery: %+v", metrics)
	}
}

func TestEngineSnapshotTracksLifecycleCounters(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	if err := engine.Reload(); err != nil {
		t.Fatalf("expected reload to succeed, got %v", err)
	}

	snapshot := engine.Snapshot()
	if snapshot.ReloadSuccessTotal != 1 {
		t.Fatalf("expected ReloadSuccessTotal=1, got %#v", snapshot.ReloadSuccessTotal)
	}
	if snapshot.InitSuccessTotal != 1 {
		t.Fatalf("expected InitSuccessTotal=1, got %#v", snapshot.InitSuccessTotal)
	}
	if snapshot.ReloadCount != 1 {
		t.Fatalf("expected ReloadCount=1, got %#v", snapshot.ReloadCount)
	}
}

func TestEngineReloadClosesPreviousWAFOnSuccess(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	oldWAF := &fakeClosableWAF{}
	engine.mu.Lock()
	engine.waf = oldWAF
	engine.setWAFOptionsStateLocked(oldWAF)
	engine.mu.Unlock()

	if err := engine.Reload(); err != nil {
		t.Fatalf("expected reload to succeed, got %v", err)
	}

	if oldWAF.closeCalls != 1 {
		t.Fatalf("expected previous WAF to be closed once, got %d", oldWAF.closeCalls)
	}
}

func TestEngineInitReplacesMetricsCollectorWhenProvided(t *testing.T) {
	initialCollector := &countingCollector{}
	engine := newEngine(initialCollector)

	path := writeRuleFile(t, t.TempDir(), "collector.conf", testRules)
	replacementCollector := &countingCollector{}

	if err := engine.Init(Config{
		DirectivesFile:    []string{path},
		RequestBodyAccess: true,
		MetricsCollector:  replacementCollector,
	}); err != nil {
		t.Fatalf("expected init to succeed, got %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{})
	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?name=safe", nil))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 after init, got %d", resp.StatusCode)
	}
	if initialCollector.requests != 0 {
		t.Fatalf("expected initial collector to stop receiving updates, got %d requests", initialCollector.requests)
	}
	if replacementCollector.requests != 1 {
		t.Fatalf("expected replacement collector to record one request, got %d", replacementCollector.requests)
	}
}

func TestEngineInitResetsMetricsCollectorToDefaultWhenOmitted(t *testing.T) {
	initialCollector := &countingCollector{}
	engine, err := NewEngine(Config{
		DirectivesFile: []string{writeRuleFile(t, t.TempDir(), "collector.conf", testRules)},
	}.WithMetricsCollector(initialCollector))
	if err != nil {
		t.Fatalf("failed to create engine with custom collector: %v", err)
	}

	reloadPath := writeRuleFile(t, t.TempDir(), "collector-reload.conf", testRules)
	if err := engine.Init(Config{
		DirectivesFile: []string{reloadPath},
	}); err != nil {
		t.Fatalf("expected reinit without collector to succeed, got %v", err)
	}

	app := newInstanceApp(engine, MiddlewareConfig{})
	resp := performRequest(t, app, httptest.NewRequest(http.MethodGet, "/?name=safe", nil))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 after collector reset, got %d", resp.StatusCode)
	}
	if initialCollector.requests != 0 {
		t.Fatalf("expected original custom collector to stop receiving updates, got %d requests", initialCollector.requests)
	}

	snapshot := engine.MetricsSnapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("expected default collector to record one request after reset, got %+v", snapshot)
	}
}

func TestReloadWithoutDirectivesSucceeds(t *testing.T) {
	engine, err := NewEngine(Config{
		RequestBodyAccess: true,
	})
	if err != nil {
		t.Fatalf("failed to initialize engine without directives: %v", err)
	}

	if err := engine.Reload(); err != nil {
		t.Fatalf("expected reload without directives to succeed, got %v", err)
	}

	snapshot := engine.Snapshot()
	if snapshot.ReloadSuccessTotal != 1 {
		t.Fatalf("expected ReloadSuccessTotal=1, got %#v", snapshot.ReloadSuccessTotal)
	}
	if snapshot.InitSuccessTotal != 1 {
		t.Fatalf("expected InitSuccessTotal=1, got %#v", snapshot.InitSuccessTotal)
	}
	if snapshot.ReloadCount != 1 {
		t.Fatalf("expected ReloadCount=1, got %#v", snapshot.ReloadCount)
	}
}

func TestNewEngineWildcardDirectivesRequireMatch(t *testing.T) {
	_, err := NewEngine(Config{
		DirectivesFile: []string{filepath.Join(t.TempDir(), "*.conf")},
	})
	if err == nil {
		t.Fatal("expected wildcard directives with no matches to fail")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("expected wildcard match error, got %v", err)
	}
}

func TestNewEngineQuestionMarkGlobDirectivesMatch(t *testing.T) {
	rootDir := t.TempDir()
	writeRuleFile(t, rootDir, "rule-1.conf", testRules)

	engine, err := NewEngine(Config{
		DirectivesFile:    []string{filepath.Join(rootDir, "rule-?.conf")},
		RequestBodyAccess: true,
	})
	if err != nil {
		t.Fatalf("expected question mark glob to match directives file, got %v", err)
	}
	if engine == nil || engine.waf == nil {
		t.Fatal("expected engine to initialize from question mark glob")
	}
}

func TestNewEngineCharacterClassGlobDirectivesMatch(t *testing.T) {
	rootDir := t.TempDir()
	writeRuleFile(t, rootDir, "rule-1.conf", testRules)

	engine, err := NewEngine(Config{
		DirectivesFile:    []string{filepath.Join(rootDir, "rule-[12].conf")},
		RequestBodyAccess: true,
	})
	if err != nil {
		t.Fatalf("expected character class glob to match directives file, got %v", err)
	}
	if engine == nil || engine.waf == nil {
		t.Fatal("expected engine to initialize from character class glob")
	}
}

func TestNewEngineWildcardDirectivesWithRootFSRequireMatch(t *testing.T) {
	rootDir := t.TempDir()
	_, err := NewEngine(Config{
		DirectivesFile: []string{"*.conf"},
		RootFS:         os.DirFS(rootDir),
	})
	if err == nil {
		t.Fatal("expected RootFS wildcard directives with no matches to fail")
	}
	if !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("expected wildcard match error, got %v", err)
	}
}

func TestDefaultMetricsCollectorRecordLatencyUsesOnlineAverage(t *testing.T) {
	collector := NewDefaultMetricsCollector().(*defaultMetricsCollector)

	collector.RecordLatency(time.Millisecond)
	collector.RecordLatency(3 * time.Millisecond)
	collector.RecordLatency(-time.Millisecond)

	snapshot := collector.GetMetrics()
	if snapshot == nil {
		t.Fatal("expected metrics snapshot")
	}
	if collector.latencyCount != 2 {
		t.Fatalf("expected negative latency sample to be ignored, got %d", collector.latencyCount)
	}
	if snapshot.AvgLatencyMs != 2 {
		t.Fatalf("expected average latency to be 2ms, got %v", snapshot.AvgLatencyMs)
	}
}

func newInstanceApp(engine *Engine, cfg MiddlewareConfig) *fiber.App {
	app := fiber.New()
	app.Use(engine.Middleware(cfg))
	app.All("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	return app
}

func newTestEngine(t *testing.T) (*Engine, error) {
	t.Helper()
	return newTestEngineWithRules(t, testRules)
}

func newTestEngineWithRules(t *testing.T, rules string) (*Engine, error) {
	t.Helper()

	path := writeRuleFile(t, t.TempDir(), "test.conf", rules)
	return NewEngine(Config{
		LogLevel:          fiberlog.LevelInfo,
		DirectivesFile:    []string{path},
		RequestBodyAccess: true,
	})
}

func writeRuleFile(t *testing.T, dir, name, contents string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("failed to write directives file: %v", err)
	}

	return path
}

func performRequest(t *testing.T, app *fiber.App, req *http.Request) *http.Response {
	t.Helper()

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	return resp
}

type nilSnapshotCollector struct{}

func (nilSnapshotCollector) RecordRequest()               {}
func (nilSnapshotCollector) RecordBlock()                 {}
func (nilSnapshotCollector) RecordLatency(time.Duration)  {}
func (nilSnapshotCollector) GetMetrics() *MetricsSnapshot { return nil }
func (nilSnapshotCollector) Reset()                       {}

type nilPtrSnapshotCollector struct{}

func (*nilPtrSnapshotCollector) RecordRequest()               {}
func (*nilPtrSnapshotCollector) RecordBlock()                 {}
func (*nilPtrSnapshotCollector) RecordLatency(time.Duration)  {}
func (*nilPtrSnapshotCollector) GetMetrics() *MetricsSnapshot { return nil }
func (*nilPtrSnapshotCollector) Reset()                       {}

type countingCollector struct {
	requests uint64
	blocks   uint64
}

func (c *countingCollector) RecordRequest()              { c.requests++ }
func (c *countingCollector) RecordBlock()                { c.blocks++ }
func (c *countingCollector) RecordLatency(time.Duration) {}
func (c *countingCollector) GetMetrics() *MetricsSnapshot {
	return &MetricsSnapshot{
		TotalRequests:   c.requests,
		BlockedRequests: c.blocks,
		Timestamp:       time.Now(),
	}
}
func (c *countingCollector) Reset() {
	c.requests = 0
	c.blocks = 0
}

type fakePanicWAF struct{}

func (fakePanicWAF) NewTransaction() types.Transaction {
	return fakePanicTransaction{}
}

func (fakePanicWAF) NewTransactionWithID(string) types.Transaction {
	return fakePanicTransaction{}
}

type fakeClosableWAF struct {
	closeCalls int
}

func (*fakeClosableWAF) NewTransaction() types.Transaction {
	return fakePanicTransaction{}
}

func (*fakeClosableWAF) NewTransactionWithID(string) types.Transaction {
	return fakePanicTransaction{}
}

func (w *fakeClosableWAF) Close() error {
	w.closeCalls++
	return nil
}

type fakePanicTransaction struct{}

func (fakePanicTransaction) ProcessConnection(string, int, string, int)       {}
func (fakePanicTransaction) ProcessURI(string, string, string)                {}
func (fakePanicTransaction) SetServerName(string)                             {}
func (fakePanicTransaction) AddRequestHeader(string, string)                  {}
func (fakePanicTransaction) ProcessRequestHeaders() *types.Interruption       { panic("boom") }
func (fakePanicTransaction) RequestBodyReader() (io.Reader, error)            { return bytes.NewReader(nil), nil }
func (fakePanicTransaction) AddGetRequestArgument(string, string)             {}
func (fakePanicTransaction) AddPostRequestArgument(string, string)            {}
func (fakePanicTransaction) AddPathRequestArgument(string, string)            {}
func (fakePanicTransaction) AddResponseArgument(string, string)               {}
func (fakePanicTransaction) ProcessRequestBody() (*types.Interruption, error) { return nil, nil }
func (fakePanicTransaction) WriteRequestBody([]byte) (*types.Interruption, int, error) {
	return nil, 0, nil
}
func (fakePanicTransaction) ReadRequestBodyFrom(io.Reader) (*types.Interruption, int, error) {
	return nil, 0, nil
}
func (fakePanicTransaction) AddResponseHeader(string, string) {}
func (fakePanicTransaction) ProcessResponseHeaders(int, string) *types.Interruption {
	return nil
}
func (fakePanicTransaction) ResponseBodyReader() (io.Reader, error) { return bytes.NewReader(nil), nil }
func (fakePanicTransaction) ProcessResponseBody() (*types.Interruption, error) {
	return nil, nil
}
func (fakePanicTransaction) WriteResponseBody([]byte) (*types.Interruption, int, error) {
	return nil, 0, nil
}
func (fakePanicTransaction) ReadResponseBodyFrom(io.Reader) (*types.Interruption, int, error) {
	return nil, 0, nil
}
func (fakePanicTransaction) ProcessLogging()                {}
func (fakePanicTransaction) IsRuleEngineOff() bool          { return false }
func (fakePanicTransaction) IsRequestBodyAccessible() bool  { return false }
func (fakePanicTransaction) IsResponseBodyAccessible() bool { return false }
func (fakePanicTransaction) IsResponseBodyProcessable() bool {
	return false
}
func (fakePanicTransaction) IsInterrupted() bool               { return false }
func (fakePanicTransaction) Interruption() *types.Interruption { return nil }
func (fakePanicTransaction) MatchedRules() []types.MatchedRule { return nil }
func (fakePanicTransaction) DebugLogger() debuglog.Logger      { return nil }
func (fakePanicTransaction) ID() string                        { return "panic-tx" }
func (fakePanicTransaction) Close() error                      { return nil }

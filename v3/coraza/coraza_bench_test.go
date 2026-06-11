package coraza

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	corazawaf "github.com/corazawaf/coraza/v3"
	"github.com/corazawaf/coraza/v3/types"
	"github.com/gofiber/fiber/v3"
	fiberlog "github.com/gofiber/fiber/v3/log"
	"github.com/valyala/fasthttp"
)

const benchQueryRules = `SecRuleEngine On
SecRequestBodyAccess On
SecRule ARGS:attack "@streq 1" "id:1001,phase:2,deny,status:403,msg:'attack detected'"`

const benchBodyRules = `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQUEST_BODY "@contains attack" "id:1002,phase:2,deny,status:403,msg:'body attack detected'"`

const benchBodyLimit = 4 * 1024 * 1024

var (
	benchBody1KBAllow   = bytes.Repeat([]byte("payload=safe&"), 78)
	benchBody1KBBlock   = append(bytes.Repeat([]byte("payload=safe&"), 77), []byte("payload=attack")...)
	benchBody64KBAllow  = bytes.Repeat([]byte("payload=safe&"), 5041)
	benchObserveLatency = 2 * time.Millisecond
)

func BenchmarkFiberBaseline_GET(b *testing.B) {
	b.ReportAllocs()

	app := newBenchmarkApp(b, nil)
	req := httptest.NewRequest(http.MethodGet, "/?name=safe", nil)
	benchmarkAppRequest(b, app, func() *http.Request {
		return req
	})
}

func BenchmarkCoraza_NoRules_GET(b *testing.B) {
	b.ReportAllocs()

	engine := newBenchmarkEngine(b, "")
	app := newBenchmarkApp(b, engine)
	req := httptest.NewRequest(http.MethodGet, "/?name=safe", nil)
	benchmarkAppRequest(b, app, func() *http.Request {
		return req
	})
}

func BenchmarkCoraza_QueryRule_GET_Allow(b *testing.B) {
	b.ReportAllocs()

	engine := newBenchmarkEngine(b, benchQueryRules)
	app := newBenchmarkApp(b, engine)
	req := httptest.NewRequest(http.MethodGet, "/?name=safe", nil)
	benchmarkAppRequest(b, app, func() *http.Request {
		return req
	})
}

func BenchmarkCoraza_QueryRule_GET_Block(b *testing.B) {
	b.ReportAllocs()

	engine := newBenchmarkEngine(b, benchQueryRules)
	app := newBenchmarkApp(b, engine)
	req := httptest.NewRequest(http.MethodGet, "/?attack=1", nil)
	benchmarkAppRequest(b, app, func() *http.Request {
		return req
	})
}

func BenchmarkCoraza_BodyRule_POST_1KB_Allow(b *testing.B) {
	b.ReportAllocs()

	engine := newBenchmarkEngine(b, benchBodyRules)
	app := newBenchmarkApp(b, engine)
	benchmarkAppRequest(b, app, func() *http.Request {
		return newBenchmarkPostRequest(benchBody1KBAllow)
	})
}

func BenchmarkCoraza_BodyRule_POST_1KB_Block(b *testing.B) {
	b.ReportAllocs()

	engine := newBenchmarkEngine(b, benchBodyRules)
	app := newBenchmarkApp(b, engine)
	benchmarkAppRequest(b, app, func() *http.Request {
		return newBenchmarkPostRequest(benchBody1KBBlock)
	})
}

func BenchmarkCoraza_BodyRule_POST_64KB_Allow(b *testing.B) {
	b.ReportAllocs()

	engine := newBenchmarkEngine(b, benchBodyRules)
	app := newBenchmarkApp(b, engine)
	benchmarkAppRequest(b, app, func() *http.Request {
		return newBenchmarkPostRequest(benchBody64KBAllow)
	})
}

func BenchmarkCoraza_ManyHeaders_GET(b *testing.B) {
	b.ReportAllocs()

	engine := newBenchmarkEngine(b, benchQueryRules)
	app := newBenchmarkApp(b, engine)
	req := httptest.NewRequest(http.MethodGet, "/?name=safe", nil)
	addBenchmarkHeaders(req.Header, 64)

	benchmarkAppRequest(b, app, func() *http.Request {
		return req
	})
}

func BenchmarkConvertFiberToStdRequest_GET(b *testing.B) {
	b.ReportAllocs()

	app, ctx := newBenchmarkCtx(b, http.MethodGet, "/?name=safe", nil)
	defer app.ReleaseCtx(ctx)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := convertFiberToStdRequest(ctx)
		if err != nil {
			b.Fatal(err)
		}
		closeRequestBody(b, req)
	}
}

func BenchmarkConvertFiberToStdRequest_POST_1KB(b *testing.B) {
	b.ReportAllocs()

	app, ctx := newBenchmarkCtx(b, http.MethodPost, "/", benchBody1KBAllow)
	defer app.ReleaseCtx(ctx)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := convertFiberToStdRequest(ctx)
		if err != nil {
			b.Fatal(err)
		}
		closeRequestBody(b, req)
	}
}

func BenchmarkProcessRequest_FromStdRequest_GET(b *testing.B) {
	b.ReportAllocs()

	waf := newBenchmarkWAF(b, "")
	req := httptest.NewRequest(http.MethodGet, "/?name=safe", nil)
	req.RemoteAddr = "127.0.0.1:1234"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := waf.NewTransaction()
		if _, err := processRequest(tx, req, benchBodyLimit); err != nil {
			b.Fatal(err)
		}
		closeTransaction(b, tx)
	}
}

func BenchmarkProcessRequest_FromStdRequest_POST_1KB(b *testing.B) {
	b.ReportAllocs()

	waf := newBenchmarkWAF(b, benchBodyRules)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := waf.NewTransaction()
		req := newBenchmarkPostRequest(benchBody1KBAllow)
		req.RemoteAddr = "127.0.0.1:1234"
		if _, err := processRequest(tx, req, benchBodyLimit); err != nil {
			b.Fatal(err)
		}
		closeTransaction(b, tx)
		closeRequestBody(b, req)
	}
}

func BenchmarkCorazaTransaction_Direct_GET(b *testing.B) {
	b.ReportAllocs()

	waf := newBenchmarkWAF(b, "")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := waf.NewTransaction()
		processDirectRequest(b, tx, http.MethodGet, "/?name=safe", nil, 1)
		closeTransaction(b, tx)
	}
}

func BenchmarkCorazaTransaction_Direct_POST_1KB(b *testing.B) {
	b.ReportAllocs()

	waf := newBenchmarkWAF(b, benchBodyRules)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx := waf.NewTransaction()
		processDirectRequest(b, tx, http.MethodPost, "/", benchBody1KBAllow, 1)
		closeTransaction(b, tx)
	}
}

func BenchmarkDefaultMetricsCollector_ObserveRequest(b *testing.B) {
	b.ReportAllocs()

	collector := NewDefaultMetricsCollector()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		collector.ObserveRequest(benchObserveLatency, i%16 == 0)
	}
}

func BenchmarkDefaultMetricsCollector_ObserveRequestParallel(b *testing.B) {
	b.ReportAllocs()

	collector := NewDefaultMetricsCollector()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			collector.ObserveRequest(benchObserveLatency, i%16 == 0)
			i++
		}
	})
}

func newBenchmarkEngine(b *testing.B, rules string) *Engine {
	b.Helper()

	cfg := Config{
		LogLevel:          fiberlog.LevelError,
		RequestBodyAccess: true,
	}
	if rules != "" {
		cfg.DirectivesFile = []string{writeBenchmarkRuleFile(b, rules)}
	}

	engine, err := NewEngine(cfg)
	if err != nil {
		b.Fatal(err)
	}

	return engine
}

func newBenchmarkWAF(b *testing.B, rules string) corazawaf.WAF {
	b.Helper()

	cfg := corazawaf.NewWAFConfig().WithRequestBodyAccess()
	if rules != "" {
		cfg = cfg.WithDirectivesFromFile(writeBenchmarkRuleFile(b, rules))
	}

	waf, err := corazawaf.NewWAF(cfg)
	if err != nil {
		b.Fatal(err)
	}

	return waf
}

func writeBenchmarkRuleFile(b *testing.B, rules string) string {
	b.Helper()

	path := filepath.Join(b.TempDir(), "bench.conf")
	if err := os.WriteFile(path, []byte(rules), 0o600); err != nil {
		b.Fatal(err)
	}

	return path
}

func newBenchmarkApp(b *testing.B, engine *Engine) *fiber.App {
	b.Helper()

	app := fiber.New()
	if engine != nil {
		app.Use(engine.Middleware())
	}
	app.All("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	return app
}

func benchmarkAppRequest(b *testing.B, app *fiber.App, newReq func() *http.Request) {
	b.Helper()

	resp, err := app.Test(newReq())
	if err != nil {
		b.Fatal(err)
	}
	closeResponseBody(b, resp)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := app.Test(newReq())
		if err != nil {
			b.Fatal(err)
		}
		closeResponseBody(b, resp)
	}
}

func newBenchmarkPostRequest(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func addBenchmarkHeaders(header http.Header, count int) {
	for i := 0; i < count; i++ {
		header.Set("X-Bench-"+strconv.Itoa(i), "value-"+strconv.Itoa(i))
	}
}

func newBenchmarkCtx(b *testing.B, method, uri string, body []byte) (*fiber.App, fiber.Ctx) {
	b.Helper()

	app := fiber.New()
	fctx := &fasthttp.RequestCtx{}
	fctx.Request.Header.SetMethod(method)
	fctx.Request.SetRequestURI(uri)
	fctx.Request.Header.SetHost("example.com")
	if len(body) > 0 {
		fctx.Request.Header.SetContentType("application/x-www-form-urlencoded")
		fctx.Request.SetBody(body)
	}

	return app, app.AcquireCtx(fctx)
}

func processDirectRequest(b *testing.B, tx types.Transaction, method, uri string, body []byte, headerCount int) {
	b.Helper()

	tx.ProcessConnection("127.0.0.1", 1234, "", 0)
	tx.ProcessURI(uri, method, "HTTP/1.1")
	tx.AddRequestHeader("Host", "example.com")
	tx.SetServerName("example.com")
	for i := 0; i < headerCount; i++ {
		tx.AddRequestHeader("X-Bench-"+strconv.Itoa(i), "value-"+strconv.Itoa(i))
	}
	if it := tx.ProcessRequestHeaders(); it != nil {
		return
	}
	if len(body) > 0 {
		if it, _, err := tx.ReadRequestBodyFrom(bytes.NewReader(body)); err != nil {
			b.Fatal(err)
		} else if it != nil {
			return
		}
	}
	if _, err := tx.ProcessRequestBody(); err != nil {
		b.Fatal(err)
	}
}

func closeResponseBody(b *testing.B, resp *http.Response) {
	b.Helper()

	if resp.Body == nil {
		return
	}
	if err := resp.Body.Close(); err != nil {
		b.Fatal(err)
	}
}

func closeRequestBody(b *testing.B, req *http.Request) {
	b.Helper()

	if req.Body == nil || req.Body == http.NoBody {
		return
	}
	if err := req.Body.Close(); err != nil {
		b.Fatal(err)
	}
}

func closeTransaction(b *testing.B, tx interface {
	ProcessLogging()
	Close() error
}) {
	b.Helper()

	tx.ProcessLogging()
	if err := tx.Close(); err != nil {
		b.Fatal(err)
	}
}

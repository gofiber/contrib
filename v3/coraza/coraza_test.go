package coraza

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corazawaf/coraza/v3/debuglog"
	"github.com/corazawaf/coraza/v3/types"
	"github.com/gofiber/fiber/v3"
	fiberlog "github.com/gofiber/fiber/v3/log"
	"github.com/valyala/fasthttp"
)

const float64Epsilon = 1e-9

const testRules = `SecRuleEngine On
SecRequestBodyAccess On
SecRule ARGS:attack "@streq 1" "id:1001,phase:2,deny,status:403,msg:'attack detected'"`

func assertFloat64Within(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("expected %s to be a finite float, got %v", label, got)
	}
	if math.Abs(got-want) > float64Epsilon {
		t.Fatalf("expected %s to be %v +/- %v, got %v", label, want, float64Epsilon, got)
	}
}

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

	engine.mu.RLock()
	got := engine.blockMessage
	engine.mu.RUnlock()
	if got != defaultBlockMessage {
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
	// The default handler must not leak rule identifiers or match data.
	if strings.Contains(string(body), "1001") || strings.Contains(string(body), "attack detected") {
		t.Fatalf("expected rule details to be suppressed in response body, got %q", string(body))
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
	if !errors.Is(err, fasthttp.ErrBodyTooLarge) {
		t.Fatalf("expected Fiber body limit error, got %v", err)
	}
}

func TestEngineMiddlewareInspectsDecodedRequestBody(t *testing.T) {
	bodyRules := `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQUEST_HEADERS:X-Deny-Encoded "@streq 1" "id:1003,phase:2,deny,status:406,chain"
SecRule REQUEST_HEADERS:Content-Encoding "@streq gzip" "chain"
SecRule REQUEST_HEADERS:Content-Length "@streq %{REQUEST_HEADERS.X-Wire-Length}" ""
SecRule ARGS|REQUEST_BODY "@contains attack" "id:1002,phase:2,deny,status:403,msg:'body attack detected'"`

	engine, err := newTestEngineWithRules(t, bodyRules)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := fiber.New(fiber.Config{BodyLimit: 4096})
	// A reader ahead of the WAF must not pin the form values of the encoded body.
	app.Use(func(c fiber.Ctx) error {
		_ = c.FormValue("payload")
		return c.Next()
	})
	app.Use(engine.Middleware())
	app.Post("/", func(c fiber.Ctx) error {
		return c.SendString(strings.Join([]string{
			string(c.Body()), string(c.BodyRaw()), c.FormValue("payload"),
			c.Get(fiber.HeaderContentEncoding), c.Get(fiber.HeaderContentLength),
		}, "|"))
	})

	send := func(body []byte, contentType, encoding string, headers ...string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set(fiber.HeaderContentType, contentType)
		req.Header.Set(fiber.HeaderContentEncoding, encoding)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp := performRequest(t, app, req)
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read response: %v", err)
		}
		return resp.StatusCode, string(got)
	}

	// Long enough to be compressed rather than stored, which would leave it readable.
	deniedBody := []byte("pad=" + strings.Repeat("a", 256) + "&payload=attack")
	for encoding, body := range map[string][]byte{
		"gzip":          fasthttp.AppendGzipBytes(nil, deniedBody),
		"x-gzip":        fasthttp.AppendGzipBytes(nil, deniedBody),
		"deflate":       fasthttp.AppendDeflateBytes(nil, deniedBody),
		"br":            fasthttp.AppendBrotliBytes(nil, deniedBody),
		"zstd":          fasthttp.AppendZstdBytes(nil, deniedBody),
		"deflate, GZIP": fasthttp.AppendGzipBytes(nil, fasthttp.AppendDeflateBytes(nil, deniedBody)),
	} {
		if status, got := send(body, fiber.MIMEApplicationForm, encoding); status != http.StatusForbidden {
			t.Fatalf("%s: expected status 403 for an encoded body the rule denies, got %d with body %q", encoding, status, got)
		}
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("payload", "attack"); err != nil {
		t.Fatalf("failed to write multipart field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}
	if status, got := send(fasthttp.AppendGzipBytes(nil, form.Bytes()), writer.FormDataContentType(), "gzip"); status != http.StatusForbidden {
		t.Fatalf("expected status 403 for an encoded multipart body the rule denies, got %d with body %q", status, got)
	}

	// A clean encoded body reaches every reader decoded, even when the encoded
	// bytes spell form values of their own.
	var cleanGzip bytes.Buffer
	zw := gzip.NewWriter(&cleanGzip)
	zw.Extra = []byte("&payload=attack&")
	if _, err := zw.Write([]byte("payload=clean")); err != nil {
		t.Fatalf("failed to write gzip body: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	if status, got := send(cleanGzip.Bytes(), fiber.MIMEApplicationForm, "gzip"); status != http.StatusOK || got != "payload=clean|payload=clean|clean||13" {
		t.Fatalf("expected the decoded clean body, got %d with body %q", status, got)
	}

	// Rules still see the headers as they were received.
	if status, got := send(cleanGzip.Bytes(), fiber.MIMEApplicationForm, "gzip",
		"X-Deny-Encoded", "1", "X-Wire-Length", strconv.Itoa(cleanGzip.Len())); status != http.StatusNotAcceptable {
		t.Fatalf("expected status 406 from the header rule, got %d with body %q", status, got)
	}

	// What cannot be decoded cannot be inspected: PostArgs would still read it.
	for _, encoding := range []string{"gzip", "compress", "gzip, unknown"} {
		if status, got := send(deniedBody, fiber.MIMEApplicationForm, encoding); status != http.StatusUnsupportedMediaType {
			t.Fatalf("%s: expected status 415 for an undecodable body, got %d with body %q", encoding, status, got)
		}
	}

	stacked := deniedBody
	for layers := 1; layers <= maxContentCodings+1; layers++ {
		stacked = fasthttp.AppendGzipBytes(nil, stacked)
		want := http.StatusForbidden
		if layers > maxContentCodings {
			want = http.StatusUnsupportedMediaType
		}
		encoding := strings.TrimSuffix(strings.Repeat("gzip,", layers), ",")
		if status, got := send(stacked, fiber.MIMEApplicationForm, encoding); status != want {
			t.Fatalf("%d codings: expected status %d, got %d with body %q", layers, want, status, got)
		}
	}

	oversized := fasthttp.AppendGzipBytes(nil, bytes.Repeat([]byte("a"), 8192))
	if status, got := send(oversized, fiber.MIMEApplicationForm, "gzip"); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status 413 for a body decoding beyond BodyLimit, got %d with body %q", status, got)
	}
}

func TestEngineMiddlewareDecodesRequestBodyOnlyWhenInspected(t *testing.T) {
	bodyRules := `SecRuleEngine On
SecRequestBodyAccess Off
SecRule REQUEST_URI "@beginsWith /inspected" "id:1004,phase:1,pass,nolog,ctl:requestBodyAccess=On"
SecRule REQUEST_BODY "@contains attack" "id:1002,phase:2,deny,status:403,msg:'body attack detected'"`

	engine, err := NewEngine(Config{
		DirectivesFile: []string{writeRuleFile(t, t.TempDir(), "test.conf", bodyRules)},
	}.WithRequestBodyAccess(false))
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := fiber.New()
	app.Use(engine.Middleware())
	app.Post("/*", func(c fiber.Ctx) error {
		return c.Send(c.BodyRaw())
	})

	body := fasthttp.AppendGzipBytes(nil, []byte("pad="+strings.Repeat("a", 256)+"&payload=attack"))
	send := func(target string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationForm)
		req.Header.Set(fiber.HeaderContentEncoding, "gzip")
		return performRequest(t, app, req)
	}

	resp := send("/inspected")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403 once a rule enables body access, got %d", resp.StatusCode)
	}

	resp = send("/")
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Fatalf("expected the request to be left untouched without body access, got %d with body %q", resp.StatusCode, got)
	}
}

func TestEngineMiddlewareInspectsMultipartFormThatGrowsBeyondBodyLimit(t *testing.T) {
	uploadRules := `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQBODY_ERROR "!@eq 0" "id:200002,phase:2,t:none,deny,status:400"
SecRule MULTIPART_STRICT_ERROR "!@eq 0" "id:200003,phase:2,t:none,deny,status:400"
SecRule FILES "@endsWith .php" "id:1003,phase:2,deny,status:403,msg:'php upload detected'"`

	engine, err := newTestEngineWithRules(t, uploadRules)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	const bodyLimit = 64 * 1024

	app := fiber.New(fiber.Config{
		BodyLimit: bodyLimit,
	})
	app.Use(engine.Middleware())
	app.Post("/", func(c fiber.Ctx) error {
		file, err := c.FormFile("file")
		if err != nil {
			return err
		}
		return c.SendString(file.Filename)
	})

	// fasthttp re-serializes a pre-parsed form with every backslash of a field
	// name doubled, so the body Coraza sees grows past BodyLimit.
	for _, fieldName := range []string{"note", strings.Repeat("aa", bodyLimit/3), strings.Repeat(`\a`, bodyLimit/3)} {
		body := "--A\r\nContent-Disposition: form-data; name=\"" + fieldName + "\"\r\n\r\nx\r\n" +
			"--A\r\nContent-Disposition: form-data; name=\"file\"; filename=\"shell.php\"\r\n\r\ndata\r\n--A--\r\n"
		if len(body) >= bodyLimit {
			t.Fatalf("request body of %d bytes is not within the body limit", len(body))
		}

		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=A")

		resp := performRequest(t, app, req)
		resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected status 403 with a field name of %d bytes starting with %q, got %d", len(fieldName), fieldName[:2], resp.StatusCode)
		}
	}
}

func TestEngineMiddlewareRefusesStreamedBodyBeyondBodyLimit(t *testing.T) {
	bodyRules := `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQUEST_BODY "@contains attack" "id:1002,phase:2,deny,status:403,msg:'body attack detected'"`

	engine, err := newTestEngineWithRules(t, bodyRules)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	// With StreamRequestBody fasthttp hands a larger body on; the adaptor must refuse it.
	app := fiber.New(fiber.Config{
		BodyLimit:         8,
		StreamRequestBody: true,
	})
	app.Use(engine.Middleware())
	app.Post("/", func(c fiber.Ctx) error {
		return c.Send(c.Body())
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload=attack"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := performRequest(t, app, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status 413 for a streamed body beyond the body limit, got %d", resp.StatusCode)
	}
}

func TestEngineMiddlewareInspectsTheMultipartFormHandlersRead(t *testing.T) {
	uploadRules := `SecRuleEngine On
SecRequestBodyAccess On
SecRule REQBODY_ERROR "!@eq 0" "id:200002,phase:2,t:none,deny,status:400"
SecRule MULTIPART_STRICT_ERROR "!@eq 0" "id:200003,phase:2,t:none,deny,status:400"
SecRule FILES "@rx (?i)\.php$" "id:1000,phase:2,deny,status:403"
SecRule ARGS_POST "@contains ../" "id:1001,phase:2,deny,status:403"`

	engine, err := newTestEngineWithRules(t, uploadRules)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := fiber.New()
	app.Use(engine.Middleware())
	app.Post("/", func(c fiber.Ctx) error {
		file := ""
		if fh, err := c.FormFile("file"); err == nil {
			file = fh.Filename
		}
		return c.SendString("folder=" + c.FormValue("folder") + " file=" + file)
	})

	part := func(disposition, value string) string {
		return "--A\r\nContent-Disposition: form-data; " + disposition + "\r\n\r\n" + value + "\r\n"
	}
	qpNote := func(value string) string {
		return "--A\r\nContent-Disposition: form-data; name=\"note\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + value + "\r\n"
	}
	phpUpload := part(`name="file"; filename="shell.php"`, "<?php ?>")
	boundaryB := part(`name="folder"`, "../../etc") +
		part(`name="decoy"`, "\r\n--B\r\nContent-Disposition: form-data; name=\"folder\"\r\n\r\npictures\r\n--B--") + "--A--\r\n"

	clean := qpNote("hello=0D=0A") + part(`name="folder"`, "pictures") + part(`name="file"; filename="cat.png"`, "png") + "--A--\r\n"

	for _, tc := range []struct {
		name, contentType, body string
		preParsed               bool
		want                    int
	}{
		{"clean form", "multipart/form-data; boundary=A", clean, true, http.StatusOK},
		{"clean chunked form", "Multipart/Form-Data; Boundary=A", clean, false, http.StatusOK},
		{"control: quoted-printable note", "multipart/form-data; boundary=A", qpNote("hello=0D=0A") + phpUpload + "--A--\r\n", true, http.StatusForbidden},
		{"quoted-printable note closing the re-serialized form", "multipart/form-data; boundary=A", qpNote("hello=0D=0A--A--=0D=0A") + phpUpload + "--A--\r\n", true, http.StatusBadRequest},
		{"control: one boundary parameter", "multipart/form-data; boundary=A", boundaryB, false, http.StatusForbidden},
		{"duplicate boundary parameter", "multipart/form-data; boundary=A; boundary*=utf-8''B", boundaryB, false, http.StatusBadRequest},
		{"control: plain field", "multipart/form-data; boundary=A", part(`name="folder"`, "../../etc") + "--A--\r\n", false, http.StatusForbidden},
		{"filename* parameter", "multipart/form-data; boundary=A", part(`name="folder"; filename*=iso-8859-1''x`, "../../etc") + "--A--\r\n", false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				status int
				got    []byte
			)
			if tc.preParsed {
				// The server pre-parses a form that has a Content-Length.
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
				req.Header.Set(fiber.HeaderContentType, tc.contentType)
				resp := performRequest(t, app, req)
				defer resp.Body.Close()
				status = resp.StatusCode
				got, _ = io.ReadAll(resp.Body)
			} else {
				// A chunked form reaches the handlers as received.
				var ctx fasthttp.RequestCtx
				ctx.Request.Header.SetMethod(http.MethodPost)
				ctx.Request.SetRequestURI("/")
				ctx.Request.Header.SetHost("example.com")
				ctx.Request.Header.SetContentType(tc.contentType)
				ctx.Request.SetBodyString(tc.body)
				app.Handler()(&ctx)
				status = ctx.Response.StatusCode()
				got = ctx.Response.Body()
			}
			if status != tc.want {
				t.Fatalf("expected status %d, got %d with body %q", tc.want, status, got)
			}
			if tc.want == http.StatusOK && string(got) != "folder=pictures file=cat.png" {
				t.Fatalf("expected the handler to read the whole form, got %q", got)
			}
		})
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

	if snapshot.TotalRequests != 0 || snapshot.BlockedRequests != 0 || snapshot.BlockRate != 0 || snapshot.RecentLatencyMs != 0 || snapshot.RecentBlockRate != 0 {
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
	engine.state = newRuntimeState(oldWAF)
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
	engine.mu.Lock()
	engine.waf = fakePanicWAF{}
	engine.state = newRuntimeState(engine.waf)
	engine.mu.Unlock()

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
	engine.state = newRuntimeState(oldWAF)
	engine.mu.Unlock()

	if err := engine.Reload(); err != nil {
		t.Fatalf("expected reload to succeed, got %v", err)
	}

	if oldWAF.closeCalls != 1 {
		t.Fatalf("expected previous WAF to be closed once, got %d", oldWAF.closeCalls)
	}
}

func TestReloadWaitsForInflightRequestsBeforeClosingPreviousWAF(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	requestEntered := make(chan struct{})
	releaseRequest := make(chan struct{})
	oldWAF := &fakeDrainingWAF{
		closed:  make(chan struct{}),
		entered: requestEntered,
		release: releaseRequest,
	}
	engine.mu.Lock()
	engine.waf = oldWAF
	engine.setWAFOptionsStateLocked(oldWAF)
	engine.state = newRuntimeState(oldWAF)
	engine.mu.Unlock()

	app := fiber.New()
	app.Use(engine.Middleware())
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	requestDone := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil), fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			requestErr <- err
			return
		}
		requestDone <- resp
	}()

	// The fake WAF blocks inside inspection; the drain must wait for it.
	select {
	case <-requestEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request to enter WAF inspection")
	}

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- engine.Reload()
	}()

	select {
	case err := <-reloadDone:
		t.Fatalf("expected reload to wait for inflight request, got early result: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if oldWAF.closeCalls.Load() != 0 {
		t.Fatalf("expected previous WAF to remain open while request is inflight, got %d close calls", oldWAF.closeCalls.Load())
	}

	select {
	case <-oldWAF.closed:
		t.Fatal("expected previous WAF to remain open while request is inflight")
	default:
	}

	close(releaseRequest)

	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatalf("expected reload to succeed after request drain, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload to complete after request drain")
	}

	select {
	case resp := <-requestDone:
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
	case err := <-requestErr:
		t.Fatalf("request failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request to complete")
	}

	select {
	case <-oldWAF.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for previous WAF to close after request drain")
	}

	if oldWAF.closeCalls.Load() != 1 {
		t.Fatalf("expected previous WAF to be closed once after request drain, got %d", oldWAF.closeCalls.Load())
	}
}

func TestReloadFromHandlerBehindMiddlewareDoesNotDeadlock(t *testing.T) {
	engine, err := newTestEngine(t)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	app := fiber.New()
	app.Use(engine.Middleware())
	app.Get("/reload", func(c fiber.Ctx) error {
		// The drain must not wait on this request: its inspection finished
		// before the handler ran. A regression here hangs forever.
		if err := engine.Reload(); err != nil {
			return c.Status(http.StatusInternalServerError).SendString(err.Error())
		}
		return c.SendString("reloaded")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/reload", nil), fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("reload request failed (deadlock regression?): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 from reload handler, got %d", resp.StatusCode)
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

func TestDefaultMetricsCollectorObserveRequestTracksRecentMetrics(t *testing.T) {
	collector := NewDefaultMetricsCollector().(*defaultMetricsCollector)

	collector.ObserveRequest(time.Millisecond, false)
	first := collector.GetMetrics()
	if first == nil {
		t.Fatal("expected metrics snapshot")
	}
	if first.TotalRequests != 1 || first.BlockedRequests != 0 {
		t.Fatalf("unexpected first snapshot counts: %+v", first)
	}
	if first.BlockRate != 0 {
		t.Fatalf("expected cumulative block rate to be 0, got %v", first.BlockRate)
	}
	if first.RecentLatencyMs != 1 {
		t.Fatalf("expected first recent latency to seed at 1ms, got %v", first.RecentLatencyMs)
	}
	if first.RecentBlockRate != 0 {
		t.Fatalf("expected first recent block rate to seed at 0, got %v", first.RecentBlockRate)
	}

	collector.ObserveRequest(3*time.Millisecond, true)
	second := collector.GetMetrics()
	if second == nil {
		t.Fatal("expected metrics snapshot")
	}
	if second.TotalRequests != 2 || second.BlockedRequests != 1 {
		t.Fatalf("unexpected second snapshot counts: %+v", second)
	}
	if second.BlockRate != 0.5 {
		t.Fatalf("expected cumulative block rate to be 0.5, got %v", second.BlockRate)
	}
	assertFloat64Within(t, second.RecentLatencyMs, 1.4, "recent latency EWMA")
	assertFloat64Within(t, second.RecentBlockRate, 0.2, "recent block rate EWMA")

	collector.ObserveRequest(-time.Millisecond, false)
	third := collector.GetMetrics()
	if third == nil {
		t.Fatal("expected metrics snapshot")
	}
	if third.TotalRequests != 3 || third.BlockedRequests != 1 {
		t.Fatalf("unexpected third snapshot counts: %+v", third)
	}
	if third.BlockRate != (1.0 / 3.0) {
		t.Fatalf("expected cumulative block rate to be 1/3, got %v", third.BlockRate)
	}
	if third.RecentLatencyMs != second.RecentLatencyMs {
		t.Fatalf("expected negative latency sample to leave recent latency unchanged, got %v want %v", third.RecentLatencyMs, second.RecentLatencyMs)
	}
	assertFloat64Within(t, third.RecentBlockRate, 0.16, "recent block rate EWMA")

	collector.Reset()
	afterReset := collector.GetMetrics()
	if afterReset.TotalRequests != 0 || afterReset.BlockedRequests != 0 || afterReset.BlockRate != 0 {
		t.Fatalf("unexpected counts after reset: %+v", afterReset)
	}
	if afterReset.RecentLatencyMs != 0 || afterReset.RecentBlockRate != 0 {
		t.Fatalf("expected recent metrics to be 0 after reset, got %+v", afterReset)
	}

	// The first sample after a reset must seed exactly, not blend against 0.
	collector.ObserveRequest(2*time.Millisecond, true)
	reseeded := collector.GetMetrics()
	if reseeded.RecentLatencyMs != 2 {
		t.Fatalf("expected recent latency to re-seed at 2ms after reset, got %v", reseeded.RecentLatencyMs)
	}
	if reseeded.RecentBlockRate != 1 {
		t.Fatalf("expected recent block rate to re-seed at 1 after reset, got %v", reseeded.RecentBlockRate)
	}
}

func TestDefaultMetricsCollectorConcurrentObserveRequest(t *testing.T) {
	collector := NewDefaultMetricsCollector().(*defaultMetricsCollector)

	const (
		workers            = 8
		requestsPerWorker  = 200
		minLatency         = time.Millisecond
		maxLatency         = 4 * time.Millisecond
		totalRequestsCount = workers * requestsPerWorker
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < requestsPerWorker; i++ {
				latency := minLatency + time.Duration(i%4)*time.Millisecond
				collector.ObserveRequest(latency, worker%2 == 0)
			}
		}(w)
	}
	wg.Wait()

	snapshot := collector.GetMetrics()
	if snapshot.TotalRequests != totalRequestsCount {
		t.Fatalf("expected %d total requests, got %d", totalRequestsCount, snapshot.TotalRequests)
	}
	if snapshot.BlockedRequests != totalRequestsCount/2 {
		t.Fatalf("expected %d blocked requests, got %d", totalRequestsCount/2, snapshot.BlockedRequests)
	}
	// The EWMA is a convex combination of samples, so it must stay within the
	// sample envelope regardless of interleaving.
	minMs := float64(minLatency.Milliseconds())
	maxMs := float64(maxLatency.Milliseconds())
	if snapshot.RecentLatencyMs < minMs || snapshot.RecentLatencyMs > maxMs {
		t.Fatalf("expected recent latency within [%v, %v], got %v", minMs, maxMs, snapshot.RecentLatencyMs)
	}
	if snapshot.RecentBlockRate < 0 || snapshot.RecentBlockRate > 1 {
		t.Fatalf("expected recent block rate within [0, 1], got %v", snapshot.RecentBlockRate)
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

func (nilSnapshotCollector) ObserveRequest(time.Duration, bool) {}
func (nilSnapshotCollector) GetMetrics() *MetricsSnapshot       { return nil }
func (nilSnapshotCollector) Reset()                             {}

type nilPtrSnapshotCollector struct{}

func (*nilPtrSnapshotCollector) ObserveRequest(time.Duration, bool) {}
func (*nilPtrSnapshotCollector) GetMetrics() *MetricsSnapshot       { return nil }
func (*nilPtrSnapshotCollector) Reset()                             {}

type countingCollector struct {
	requests uint64
	blocks   uint64
}

func (c *countingCollector) ObserveRequest(_ time.Duration, blocked bool) {
	c.requests++
	if blocked {
		c.blocks++
	}
}
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

type fakeDrainingWAF struct {
	closeCalls atomic.Int32
	closed     chan struct{}
	entered    chan struct{}
	release    chan struct{}
}

func (w *fakeDrainingWAF) NewTransaction() types.Transaction {
	return fakeBlockingTransaction{entered: w.entered, release: w.release}
}

func (w *fakeDrainingWAF) NewTransactionWithID(string) types.Transaction {
	return fakeBlockingTransaction{entered: w.entered, release: w.release}
}

// fakeBlockingTransaction blocks inside inspection so tests can hold a request
// in the WAF while asserting drain behavior.
type fakeBlockingTransaction struct {
	fakeAllowTransaction
	entered chan struct{}
	release chan struct{}
}

func (t fakeBlockingTransaction) ProcessRequestHeaders() *types.Interruption {
	if t.entered != nil {
		close(t.entered)
	}
	if t.release != nil {
		<-t.release
	}
	return nil
}

func (w *fakeDrainingWAF) Close() error {
	if w.closeCalls.Add(1) == 1 && w.closed != nil {
		close(w.closed)
	}
	return nil
}

type fakePanicTransaction struct{}

type fakeAllowTransaction struct {
	fakePanicTransaction
}

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

func (fakeAllowTransaction) ProcessRequestHeaders() *types.Interruption       { return nil }
func (fakeAllowTransaction) ProcessRequestBody() (*types.Interruption, error) { return nil, nil }

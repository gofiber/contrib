package monitor

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestLegacyConfigFieldsCompileButDoNotAffectDashboard(t *testing.T) {
	app := fiber.New()
	app.Get("/metrics", New(Config{
		Title:      "Compatibility Monitor",
		CustomHead: "legacy-head-marker",
		FontURL:    "legacy-font-marker",
		ChartJSURL: "legacy-chart-marker",
	}))

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil))
	mustNoError(t, err)
	defer func() { mustNoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	mustNoError(t, err)
	page := string(body)
	mustContain(t, page, "Compatibility Monitor")
	mustNotContain(t, page, "legacy-head-marker")
	mustNotContain(t, page, "legacy-font-marker")
	mustNotContain(t, page, "legacy-chart-marker")
}

func TestLegacyRouteMountWithAPIOnly(t *testing.T) {
	app := fiber.New()
	app.Get("/metrics", New(Config{
		APIOnly:    true,
		CustomHead: "legacy",
		FontURL:    "legacy",
		ChartJSURL: "legacy",
	}))

	req := httptest.NewRequest(fiber.MethodGet, "/metrics", nil)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMETextHTML)
	resp, err := app.Test(req)
	mustNoError(t, err)
	defer func() { mustNoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	mustNoError(t, err)
	mustContain(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)
	mustContain(t, string(body), `"collection"`)
}

func TestConfigDefaultAPIOnlyRemainsSupported(t *testing.T) {
	original := ConfigDefault
	t.Cleanup(func() { ConfigDefault = original })
	ConfigDefault.APIOnly = true

	app := fiber.New()
	app.Get("/metrics", New(Config{Title: "Custom title"}))
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil))
	mustNoError(t, err)
	defer func() { mustNoError(t, resp.Body.Close()) }()
	mustContain(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)
}

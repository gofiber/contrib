package monitor

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	page := string(body)
	assert.Contains(t, page, "Compatibility Monitor")
	assert.NotContains(t, page, "legacy-head-marker")
	assert.NotContains(t, page, "legacy-font-marker")
	assert.NotContains(t, page, "legacy-chart-marker")
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
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)
	assert.Contains(t, string(body), `"collection"`)
}

func TestConfigDefaultAPIOnlyRemainsSupported(t *testing.T) {
	original := ConfigDefault
	t.Cleanup(func() { ConfigDefault = original })
	ConfigDefault.APIOnly = true

	app := fiber.New()
	app.Get("/metrics", New(Config{Title: "Custom title"}))
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/metrics", nil))
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	assert.Contains(t, resp.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON)
}

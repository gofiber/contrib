package monitor

import (
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := configDefault().normalized()
	mustNoError(t, err)
	mustEqual(t, defaultTitle, cfg.Title)
	mustEqual(t, defaultDescription, cfg.Description)
	mustEqual(t, defaultFooter, cfg.Footer)
	mustEqual(t, defaultRefresh, cfg.Refresh)
	mustFalse(t, cfg.APIOnly)
	mustFalse(t, cfg.EnableGCPauseMetrics)
	mustEqual(t, legacyDefaultFontURL, cfg.FontURL)
	mustEqual(t, legacyDefaultChartJSURL, cfg.ChartJSURL)
}

func TestConfigOverrides(t *testing.T) {
	cfg, err := configDefault(Config{
		Title:                "Service Monitor",
		Description:          "Description",
		Footer:               "Footer",
		FaviconURL:           "/favicon.svg",
		Refresh:              5 * time.Second,
		APIOnly:              true,
		EnableGCPauseMetrics: true,
		CustomHead:           "legacy head",
		FontURL:              "legacy font",
		ChartJSURL:           "legacy chart",
	}).normalized()
	mustNoError(t, err)
	mustEqual(t, "Service Monitor", cfg.Title)
	mustEqual(t, "Description", cfg.Description)
	mustEqual(t, "Footer", cfg.Footer)
	mustEqual(t, "/favicon.svg", cfg.FaviconURL)
	mustEqual(t, 5*time.Second, cfg.Refresh)
	mustTrue(t, cfg.APIOnly)
	mustTrue(t, cfg.EnableGCPauseMetrics)
	mustEqual(t, "legacy head", cfg.CustomHead)
	mustEqual(t, "legacy font", cfg.FontURL)
	mustEqual(t, "legacy chart", cfg.ChartJSURL)
}

func TestRefreshNormalization(t *testing.T) {
	tests := []struct {
		name     string
		input    time.Duration
		expected time.Duration
	}{
		{name: "negative", input: -time.Second, expected: defaultRefresh},
		{name: "zero", expected: defaultRefresh},
		{name: "below minimum", input: time.Millisecond, expected: minimumRefresh},
		{name: "custom", input: 5 * time.Second, expected: 5 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := (Config{Refresh: test.input}).normalized()
			mustNoError(t, err)
			mustEqual(t, test.expected, cfg.Refresh)
		})
	}
}

func TestFaviconURLValidation(t *testing.T) {
	valid := []string{"", "/assets/favicon.svg", "http://example.com/favicon.ico", "HTTPS://example.com/favicon.svg"}
	for _, value := range valid {
		actual, err := normalizeFaviconURL(value)
		mustNoError(t, err, value)
		mustEqual(t, value, actual)
	}

	invalid := []string{"//example.com/favicon.svg", "/\\example", "javascript:alert(1)", "data:image/svg+xml,x", "file:///tmp/icon", "ftp://example.com/icon", "https://user@example.com/icon"}
	for _, value := range invalid {
		_, err := normalizeFaviconURL(value)
		mustErrorIs(t, err, ErrInvalidFaviconURL, value)
	}
}

func TestNewPanicsForInvalidConfig(t *testing.T) {
	mustPanicsWithError(t, "fiber: monitor middleware error -> "+ErrInvalidFaviconURL.Error(), func() {
		New(Config{FaviconURL: "javascript:alert(1)"})
	})
}

package monitor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := configDefault().normalized()
	require.NoError(t, err)
	assert.Equal(t, defaultTitle, cfg.Title)
	assert.Equal(t, defaultDescription, cfg.Description)
	assert.Equal(t, defaultFooter, cfg.Footer)
	assert.Equal(t, defaultRefresh, cfg.Refresh)
	assert.False(t, cfg.APIOnly)
	assert.False(t, cfg.EnableGCPauseMetrics)
	assert.Equal(t, legacyDefaultFontURL, cfg.FontURL)
	assert.Equal(t, legacyDefaultChartJSURL, cfg.ChartJSURL)
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
	require.NoError(t, err)
	assert.Equal(t, "Service Monitor", cfg.Title)
	assert.Equal(t, "Description", cfg.Description)
	assert.Equal(t, "Footer", cfg.Footer)
	assert.Equal(t, "/favicon.svg", cfg.FaviconURL)
	assert.Equal(t, 5*time.Second, cfg.Refresh)
	assert.True(t, cfg.APIOnly)
	assert.True(t, cfg.EnableGCPauseMetrics)
	assert.Equal(t, "legacy head", cfg.CustomHead)
	assert.Equal(t, "legacy font", cfg.FontURL)
	assert.Equal(t, "legacy chart", cfg.ChartJSURL)
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
			require.NoError(t, err)
			assert.Equal(t, test.expected, cfg.Refresh)
		})
	}
}

func TestFaviconURLValidation(t *testing.T) {
	valid := []string{"", "/assets/favicon.svg", "http://example.com/favicon.ico", "HTTPS://example.com/favicon.svg"}
	for _, value := range valid {
		actual, err := normalizeFaviconURL(value)
		require.NoError(t, err, value)
		assert.Equal(t, value, actual)
	}

	invalid := []string{"//example.com/favicon.svg", "/\\example", "javascript:alert(1)", "data:image/svg+xml,x", "file:///tmp/icon", "ftp://example.com/icon", "https://user@example.com/icon"}
	for _, value := range invalid {
		_, err := normalizeFaviconURL(value)
		assert.ErrorIs(t, err, ErrInvalidFaviconURL, value)
	}
}

func TestNewPanicsForInvalidConfig(t *testing.T) {
	assert.PanicsWithError(t, "fiber: monitor middleware error -> "+ErrInvalidFaviconURL.Error(), func() {
		New(Config{FaviconURL: "javascript:alert(1)"})
	})
}

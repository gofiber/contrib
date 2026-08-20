package stats

import (
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := configDefault().normalized()
	mustNoError(t, err)
	mustEqual(t, defaultPath, cfg.Path)
	mustEqual(t, defaultTitle, cfg.Title)
	mustEqual(t, defaultDescription, cfg.Description)
	mustEqual(t, defaultFooter, cfg.Footer)
	mustEqual(t, defaultRefresh, cfg.Refresh)
	mustFalse(t, cfg.EnableGCPauseMetrics)
}

func TestGCPauseMetricsAreExplicitlyEnabled(t *testing.T) {
	cfg, err := configDefault(Config{EnableGCPauseMetrics: true}).normalized()
	mustNoError(t, err)
	mustTrue(t, cfg.EnableGCPauseMetrics)
}

func TestConfigNormalization(t *testing.T) {
	tests := map[string]string{
		"empty":              defaultPath,
		"relative":           "/stats",
		"absolute":           "/stats",
		"trailing slash":     "/stats",
		"query and fragment": "/admin/stats",
		"root":               "/",
	}
	inputs := map[string]string{
		"empty":              "",
		"relative":           "stats",
		"absolute":           "/stats",
		"trailing slash":     "/stats///",
		"query and fragment": " admin/stats?format=json#top ",
		"root":               "/",
	}
	for name, expected := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := (Config{Path: inputs[name]}).normalized()
			mustNoError(t, err)
			mustEqual(t, expected, cfg.Path)
		})
	}
}

func TestRefreshNormalization(t *testing.T) {
	tests := []struct {
		input    time.Duration
		expected time.Duration
	}{
		{input: -time.Second, expected: defaultRefresh},
		{input: 0, expected: defaultRefresh},
		{input: time.Millisecond, expected: minimumRefresh},
		{input: 3 * time.Second, expected: 3 * time.Second},
	}
	for _, test := range tests {
		cfg, err := (Config{Refresh: test.input}).normalized()
		mustNoError(t, err)
		mustEqual(t, test.expected, cfg.Refresh)
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
	mustPanicsWithError(t, "fiber: stats middleware error -> "+ErrInvalidFaviconURL.Error(), func() {
		New(Config{FaviconURL: "javascript:alert(1)"})
	})
}

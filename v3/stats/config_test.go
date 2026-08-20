package stats

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := configDefault().normalized()
	require.NoError(t, err)
	require.Equal(t, defaultPath, cfg.Path)
	require.Equal(t, defaultTitle, cfg.Title)
	require.Equal(t, defaultDescription, cfg.Description)
	require.Equal(t, defaultFooter, cfg.Footer)
	require.Equal(t, defaultRefresh, cfg.Refresh)
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
			require.NoError(t, err)
			require.Equal(t, expected, cfg.Path)
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
		require.NoError(t, err)
		require.Equal(t, test.expected, cfg.Refresh)
	}
}

func TestFaviconURLValidation(t *testing.T) {
	valid := []string{"", "/assets/favicon.svg", "http://example.com/favicon.ico", "HTTPS://example.com/favicon.svg"}
	for _, value := range valid {
		actual, err := normalizeFaviconURL(value)
		require.NoError(t, err, value)
		require.Equal(t, value, actual)
	}

	invalid := []string{"//example.com/favicon.svg", "/\\example", "javascript:alert(1)", "data:image/svg+xml,x", "file:///tmp/icon", "ftp://example.com/icon", "https://user@example.com/icon"}
	for _, value := range invalid {
		_, err := normalizeFaviconURL(value)
		require.ErrorIs(t, err, ErrInvalidFaviconURL, value)
	}
}

func TestNewPanicsForInvalidConfig(t *testing.T) {
	require.PanicsWithError(t, "fiber: stats middleware error -> "+ErrInvalidFaviconURL.Error(), func() {
		New(Config{FaviconURL: "javascript:alert(1)"})
	})
}

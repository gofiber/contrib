package jwtware

import (
	"fmt"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestPanicOnMissingConfiguration(t *testing.T) {
	t.Parallel()

	defer func() {
		// Assert
		if err := recover(); err == nil {
			t.Fatalf("Middleware should panic on missing configuration")
		}
	}()

	// Arrange
	config := make([]Config, 0)

	// Act
	makeCfg(config)
}

func TestDefaultConfiguration(t *testing.T) {
	t.Parallel()

	// Arrange
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
	})

	// Act
	cfg := makeCfg(config)

	// Assert
	require.NotNil(t, cfg.Claims, "Default claims should not be 'nil'")
	require.Equal(t, extractors.SourceAuthHeader, cfg.Extractor.Source, "Default extractor source should be '%v'", extractors.SourceAuthHeader)
	require.Equal(t, fiber.HeaderAuthorization, cfg.Extractor.Key, "Default extractor key should be '%v'", fiber.HeaderAuthorization)
	require.Equal(t, "Bearer", cfg.Extractor.AuthScheme, "Default auth scheme should be 'Bearer'")
}

func TestCustomExtractor(t *testing.T) {
	t.Parallel()

	// Arrange
	extractor := extractors.FromHeader("X-Auth-Token")
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
		Extractor:  extractor,
	})

	// Act
	cfg := makeCfg(config)

	// Assert
	require.Equal(t, extractor.Source, cfg.Extractor.Source, "Extractor source should be the custom one")
	require.Equal(t, extractor.Key, cfg.Extractor.Key, "Extractor key should be the custom one")
	require.Equal(t, "", cfg.Extractor.AuthScheme, "AuthScheme should be empty for non-Authorization extractors")
}

func TestPanicOnInvalidSigningKey(t *testing.T) {
	t.Parallel()
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: nil}, // Invalid key
	})
	require.Panics(t, func() { makeCfg(config) })
}

func TestPanicOnInvalidSigningKeys(t *testing.T) {
	t.Parallel()
	config := append(make([]Config, 0), Config{
		SigningKeys: map[string]SigningKey{
			"key1": {Key: nil}, // Invalid key
		},
	})
	require.Panics(t, func() { makeCfg(config) })
}

func TestPanicOnInvalidJWKSetURLs(t *testing.T) {
	t.Parallel()
	// Arrange
	config := append(make([]Config, 0), Config{
		JWKSetURLs: []string{"invalid-url"}, // This would cause panic in keyfunc
	})
	require.Panics(t, func() { makeCfg(config) })
}

func TestCustomClaims(t *testing.T) {
	t.Parallel()

	// Arrange
	customClaims := jwt.MapClaims{"custom": "claims"}
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
		Claims:     customClaims,
	})

	// Act
	cfg := makeCfg(config)

	// Assert
	require.NotNil(t, cfg.Claims, "Custom claims should be preserved")

	// Check if it's the same map by checking a key
	claimsMap, ok := cfg.Claims.(jwt.MapClaims)
	require.True(t, ok, "Claims should be MapClaims")
	require.Equal(t, "claims", claimsMap["custom"], "Custom claims content should be preserved")
}

func TestTokenProcessorFunc_Configured(t *testing.T) {
	t.Parallel()

	// Arrange
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
		TokenProcessorFunc: func(token string) (string, error) {
			return "", fmt.Errorf("processing failed")
		},
	})

	// Act
	cfg := makeCfg(config)

	// Assert
	require.NotNil(t, cfg.TokenProcessorFunc, "TokenProcessorFunc should be set")

	// Exercise the processor
	_, err := cfg.TokenProcessorFunc("dummy")
	require.Error(t, err, "TokenProcessorFunc should return error")
}

func TestPanicOnUnsupportedJWKSetURLScheme(t *testing.T) {
	t.Parallel()
	config := append(make([]Config, 0), Config{
		JWKSetURLs: []string{"ftp://example.com"}, // Unsupported scheme
	})
	require.Panics(t, func() { makeCfg(config) })
}

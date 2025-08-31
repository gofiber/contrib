package jwtware

import (
	"fmt"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
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

	defer func() {
		// Assert
		if err := recover(); err != nil {
			t.Fatalf("Middleware should not panic")
		}
	}()

	// Arrange
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
	})

	// Act
	cfg := makeCfg(config)

	// Assert
	if cfg.Claims == nil {
		t.Fatalf("Default claims should not be 'nil'")
	}

	if cfg.Extractor.Source != SourceAuthHeader {
		t.Fatalf("Default extractor source should be '%v'", SourceAuthHeader)
	}
	if cfg.Extractor.Key != fiber.HeaderAuthorization {
		t.Fatalf("Default extractor key should be '%v'", fiber.HeaderAuthorization)
	}
	if cfg.Extractor.AuthScheme != "Bearer" {
		t.Fatalf("Default auth scheme should be 'Bearer'")
	}
}

func TestCustomExtractor(t *testing.T) {
	t.Parallel()

	defer func() {
		// Assert
		if err := recover(); err != nil {
			t.Fatalf("Middleware should not panic")
		}
	}()

	// Arrange
	extractor := FromHeader("X-Auth-Token")
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
		Extractor:  extractor,
	})

	// Act
	cfg := makeCfg(config)

	if cfg.Extractor.Source != extractor.Source {
		t.Fatalf("Extractor source should be the custom one")
	}
	if cfg.Extractor.Key != extractor.Key {
		t.Fatalf("Extractor key should be the custom one")
	}
}

func TestPanicOnInvalidSigningKey(t *testing.T) {
	t.Parallel()

	defer func() {
		// Assert
		if err := recover(); err == nil {
			t.Fatalf("Middleware should panic on invalid signing key")
		}
	}()

	// Arrange
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: nil}, // Invalid key
	})

	// Act
	makeCfg(config)
}

func TestPanicOnInvalidSigningKeys(t *testing.T) {
	t.Parallel()

	defer func() {
		// Assert
		if err := recover(); err == nil {
			t.Fatalf("Middleware should panic on invalid signing keys")
		}
	}()

	// Arrange
	config := append(make([]Config, 0), Config{
		SigningKeys: map[string]SigningKey{
			"key1": {Key: nil}, // Invalid key
		},
	})

	// Act
	makeCfg(config)
}

func TestPanicOnInvalidJWKSetURLs(t *testing.T) {
	t.Parallel()

	defer func() {
		// Assert
		if err := recover(); err == nil {
			t.Fatalf("Middleware should panic on invalid JWK Set URLs")
		}
	}()

	// Arrange
	config := append(make([]Config, 0), Config{
		JWKSetURLs: []string{"invalid-url"}, // This would cause panic in keyfunc
	})

	// Act
	makeCfg(config)
}

func TestCustomClaims(t *testing.T) {
	t.Parallel()

	defer func() {
		// Assert
		if err := recover(); err != nil {
			t.Fatalf("Middleware should not panic")
		}
	}()

	// Arrange
	customClaims := jwt.MapClaims{"custom": "claims"}
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
		Claims:     customClaims,
	})

	// Act
	cfg := makeCfg(config)

	// Assert
	if cfg.Claims == nil {
		t.Fatalf("Custom claims should be preserved")
	}

	// Check if it's the same map by checking a key
	if claimsMap, ok := cfg.Claims.(jwt.MapClaims); ok {
		if claimsMap["custom"] != "claims" {
			t.Fatalf("Custom claims content should be preserved")
		}
	} else {
		t.Fatalf("Claims should be MapClaims")
	}
}

func TestTokenProcessorFuncPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		// Assert
		if err := recover(); err == nil {
			t.Fatalf("Middleware should panic on TokenProcessorFunc error")
		}
	}()

	// Arrange
	config := append(make([]Config, 0), Config{
		SigningKey: SigningKey{Key: []byte("")},
		TokenProcessorFunc: func(token string) (string, error) {
			return "", fmt.Errorf("processing failed")
		},
	})

	// Act
	cfg := makeCfg(config)

	// This would panic in real usage, but config creation should succeed
	if cfg.TokenProcessorFunc == nil {
		t.Fatalf("TokenProcessorFunc should be set")
	}
}

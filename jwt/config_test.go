package jwtware

import (
	"testing"

	"github.com/gofiber/fiber/v3"
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

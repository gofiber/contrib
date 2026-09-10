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

// TestPanicOnUnprocessableCriticalHeader refuses a configuration that claims to
// handle an extension only the parser could have handled.
func TestPanicOnUnprocessableCriticalHeader(t *testing.T) {
	t.Parallel()

	config := []Config{{
		SigningKey:           SigningKey{JWTAlg: HS256, Key: []byte("secret")},
		KnownCriticalHeaders: []string{"b64"},
	}}
	require.PanicsWithValue(
		t,
		`Fiber: JWT middleware configuration: KnownCriticalHeaders cannot contain "b64", which changes how the JWS is parsed; tokens carrying it are always rejected`,
		func() { makeCfg(config) },
	)
}

// TestValidAlgorithms pins which configurations produce a pinned algorithm list
// for the parser, and which leave the key material to decide.
func TestValidAlgorithms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config []Config
		want   []string
	}{
		{
			name:   "no configuration",
			config: nil,
			want:   nil,
		},
		{
			name:   "single key with an algorithm",
			config: []Config{{SigningKey: SigningKey{JWTAlg: HS256, Key: []byte("secret")}}},
			want:   []string{HS256},
		},
		{
			name:   "single key without an algorithm",
			config: []Config{{SigningKey: SigningKey{Key: []byte("secret")}}},
			want:   nil,
		},
		{
			name: "key set with algorithms",
			config: []Config{{SigningKeys: map[string]SigningKey{
				"one": {JWTAlg: HS512, Key: []byte("a")},
				"two": {JWTAlg: HS256, Key: []byte("b")},
			}}},
			want: []string{HS256, HS512},
		},
		{
			name: "key set with a duplicate algorithm",
			config: []Config{{SigningKeys: map[string]SigningKey{
				"one": {JWTAlg: HS256, Key: []byte("a")},
				"two": {JWTAlg: HS256, Key: []byte("b")},
			}}},
			want: []string{HS256},
		},
		{
			name: "one unrestricted key leaves the set unrestricted",
			config: []Config{{SigningKeys: map[string]SigningKey{
				"one": {JWTAlg: HS256, Key: []byte("a")},
				"two": {Key: []byte("b")},
			}}},
			want: nil,
		},
		{
			name: "a remote key set decides for itself",
			config: []Config{{
				JWKSetURLs: []string{"https://example.com/jwks.json"},
				SigningKey: SigningKey{JWTAlg: HS256, Key: []byte("secret")},
			}},
			want: nil,
		},
		{
			name: "a caller's key function decides for itself",
			config: []Config{{
				KeyFunc:    func(*jwt.Token) (any, error) { return nil, nil },
				SigningKey: SigningKey{JWTAlg: HS256, Key: []byte("secret")},
			}},
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.want, validAlgorithms(test.config))
		})
	}
}

// TestCheckCriticalHeaders walks the crit values RFC 7515 Section 4.1.11 admits
// and the malformed ones it does not.
func TestCheckCriticalHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header  map[string]any
		name    string
		known   []string
		wantErr bool
	}{
		{
			name:   "no crit header",
			header: map[string]any{"alg": HS256},
		},
		{
			name:   "understood extension",
			header: map[string]any{"alg": HS256, "crit": []any{"ext"}, "ext": true},
			known:  []string{"ext"},
		},
		{
			name:    "unknown extension",
			header:  map[string]any{"alg": HS256, "crit": []any{"ext"}, "ext": true},
			wantErr: true,
		},
		{
			name:    "empty list",
			header:  map[string]any{"alg": HS256, "crit": []any{}},
			wantErr: true,
		},
		{
			name:    "wrong type",
			header:  map[string]any{"alg": HS256, "crit": "ext", "ext": true},
			wantErr: true,
		},
		{
			// RFC 7797 changes how the payload is encoded, which the parser has
			// already settled; declaring it understood must not help.
			name:    "b64 declared understood",
			header:  map[string]any{"alg": HS256, "crit": []any{"b64"}, "b64": false},
			known:   []string{"b64"},
			wantErr: true,
		},
		{
			// RFC 7797 Section 6 requires the crit entry, so this token is not
			// conformant - but the payload is still encoded the way the parser
			// does not expect.
			name:    "b64 without the crit entry",
			header:  map[string]any{"alg": HS256, "b64": false},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := checkCriticalHeaders(test.header, knownCriticalHeaders(test.known))
			if test.wantErr {
				require.ErrorIs(t, err, ErrCriticalHeader)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestAuthSchemes collects the schemes an extractor answers for, in the order
// the challenge lists them.
func TestAuthSchemes(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"Bearer"}, authSchemes(extractors.FromAuthHeader("Bearer")))
	require.Empty(t, authSchemes(extractors.FromCookie("token")))
	require.Equal(t, []string{"Bearer"}, authSchemes(extractors.Chain(
		extractors.FromCookie("token"),
		extractors.FromAuthHeader("Bearer"),
	)))

	// Every scheme the chain accepts, in the order it tries them, without repeats.
	require.Equal(t, []string{"Basic", "Bearer"}, authSchemes(extractors.Chain(
		extractors.FromAuthHeader("Basic"),
		extractors.FromAuthHeader("Bearer"),
		extractors.FromAuthHeader("bearer"),
	)))
}

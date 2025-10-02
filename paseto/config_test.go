package pasetoware

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/extractors"
)

func assertRecoveryPanic(t *testing.T) {
	err := recover()
	assert.Equal(t, true, err != nil)
}

func Test_Config_No_SymmetricKey(t *testing.T) {
	defer assertRecoveryPanic(t)
	config := configDefault()

	assert.Equal(t, "", config.SymmetricKey)
}

func Test_Config_Invalid_SymmetricKey(t *testing.T) {
	defer assertRecoveryPanic(t)
	config := configDefault()

	assert.Equal(t, symmetricKey+symmetricKey, config.SymmetricKey)
}

func Test_ConfigDefault(t *testing.T) {
	config := configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
	})

	assert.Equal(t, extractors.SourceAuthHeader, config.Extractor.Source)
	assert.Equal(t, fiber.HeaderAuthorization, config.Extractor.Key)
	assert.Equal(t, "Bearer", config.Extractor.AuthScheme)
	assert.Empty(t, config.Extractor.Chain)

	assert.NotNil(t, config.Validate)
}

func Test_ConfigCustomLookup(t *testing.T) {
	config := configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		Extractor:    extractors.FromHeader("Custom-Header"),
	})
	assert.Equal(t, extractors.SourceHeader, config.Extractor.Source)
	assert.Equal(t, "Custom-Header", config.Extractor.Key)
	assert.Equal(t, "", config.Extractor.AuthScheme)

	config = configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		Extractor:    extractors.FromQuery("token"),
	})
	assert.Equal(t, extractors.SourceQuery, config.Extractor.Source)
	assert.Equal(t, "token", config.Extractor.Key)
	assert.Equal(t, "", config.Extractor.AuthScheme)
}

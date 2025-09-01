package pasetoware

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/gofiber/fiber/v3"
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

	assert.Equal(t, SourceAuthHeader, config.Extractor.Source)
	assert.Equal(t, fiber.HeaderAuthorization, config.Extractor.Key)
	assert.Equal(t, "Bearer", config.Extractor.AuthScheme)
	assert.Empty(t, config.Extractor.Chain)

	assert.NotNil(t, config.Validate)
}

func Test_ConfigCustomLookup(t *testing.T) {
	config := configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		Extractor:    FromHeader("Custom-Header"),
	})
	assert.Equal(t, SourceHeader, config.Extractor.Source)
	assert.Equal(t, "Custom-Header", config.Extractor.Key)
	assert.Equal(t, "", config.Extractor.AuthScheme)

	config = configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		Extractor:    FromQuery("token"),
	})
	assert.Equal(t, SourceQuery, config.Extractor.Source)
	assert.Equal(t, "token", config.Extractor.Key)
	assert.Equal(t, "", config.Extractor.AuthScheme)
}

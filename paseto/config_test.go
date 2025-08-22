package pasetoware

import (
	"github.com/stretchr/testify/assert"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
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

	assert.Equal(t, LookupHeader, config.TokenLookup[0])
	assert.Equal(t, fiber.HeaderAuthorization, config.TokenLookup[1])

	assert.Equal(t, DefaultContextKey, config.ContextKey)
	assert.Equal(t, true, config.Validate != nil)
}

func Test_ConfigCustomLookup(t *testing.T) {
	config := configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		TokenLookup:  [2]string{"", "Custom-Header"},
	})
	assert.Equal(t, LookupHeader, config.TokenLookup[0])
	assert.Equal(t, "Custom-Header", config.TokenLookup[1])

	config = configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		TokenLookup:  [2]string{LookupParam},
	})
	assert.Equal(t, LookupParam, config.TokenLookup[0])
	assert.Equal(t, fiber.HeaderAuthorization, config.TokenLookup[1])
}

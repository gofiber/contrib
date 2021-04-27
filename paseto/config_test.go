package pasetoware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"testing"
)

func assertRecoveryPanic(t *testing.T) {
	err := recover()
	utils.AssertEqual(t, true, err != nil)
}

func Test_Config_No_SymmetricKey(t *testing.T) {
	defer assertRecoveryPanic(t)
	config := configDefault()

	utils.AssertEqual(t, "", config.SymmetricKey)
}

func Test_ConfigDefault(t *testing.T) {
	config := configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
	})

	utils.AssertEqual(t, LookupHeader, config.TokenLookup[0])
	utils.AssertEqual(t, fiber.HeaderAuthorization, config.TokenLookup[1])

	utils.AssertEqual(t, DefaultContextKey, config.ContextKey)
	utils.AssertEqual(t, true, config.Validate != nil)
}

func Test_ConfigCustomLookup(t *testing.T) {
	config := configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		TokenLookup:  [2]string{"", "Custom-Header"},
	})
	utils.AssertEqual(t, LookupHeader, config.TokenLookup[0])
	utils.AssertEqual(t, "Custom-Header", config.TokenLookup[1])

	config = configDefault(Config{
		SymmetricKey: []byte(symmetricKey),
		TokenLookup:  [2]string{LookupParam},
	})
	utils.AssertEqual(t, LookupParam, config.TokenLookup[0])
	utils.AssertEqual(t, fiber.HeaderAuthorization, config.TokenLookup[1])
}

package pasetoware

import (
	"time"

	"github.com/google/uuid"
	"github.com/o1egl/paseto"
)

const (
	pasetoTokenAudience = "gofiber.gophers"
	pasetoTokenSubject  = "user-token"
	pasetoTokenField    = "data"
)

// NewPayload generates a new paseto.JSONToken and returns it and a error that can be caused by uuid
func NewPayload(userToken string, duration time.Duration) (*paseto.JSONToken, error) {
	tokenID, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	timeNow := time.Now()
	payload := &paseto.JSONToken{
		Audience:   pasetoTokenAudience,
		Jti:        tokenID.String(),
		Subject:    pasetoTokenSubject,
		IssuedAt:   timeNow,
		Expiration: timeNow.Add(duration),
		NotBefore:  timeNow,
	}

	payload.Set(pasetoTokenField, userToken)
	return payload, nil
}

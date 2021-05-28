package pasetoware

import (
	"github.com/google/uuid"
	"github.com/o1egl/paseto"
	"time"
)

const (
	pasetoTokenAudience = "gofiber.gophers"
	pasetoTokenSubject  = "user-token"
	pasetoTokenField    = "data"
)

type PayloadValidator func(decrypted []byte) (interface{}, error)

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
		Expiration: time.Now().Add(duration),
		NotBefore:  timeNow,
	}

	payload.Set(pasetoTokenField, userToken)
	return payload, nil
}

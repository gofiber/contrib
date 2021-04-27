package pasetoware

import (
	"github.com/google/uuid"
	"time"
)

type PayloadValidator func(decrypted []byte) (interface{}, error)

type Payload struct {
	ID        uuid.UUID `json:"id"`
	UserToken string    `json:"user_token"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiredAt time.Time `json:"expired_at"`
}

func NewPayload(userToken string, duration time.Duration) (*Payload, error) {
	tokenID, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}

	payload := &Payload{
		ID:        tokenID,
		UserToken: userToken,
		IssuedAt:  time.Now(),
		ExpiredAt: time.Now().Add(duration),
	}

	return payload, nil
}

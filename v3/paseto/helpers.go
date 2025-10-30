package pasetoware

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/o1egl/paseto"
)

type TokenPurpose int

const (
	PurposeLocal TokenPurpose = iota
	PurposePublic
)

var (
	ErrExpiredToken  = errors.New("token has expired")
	ErrMissingToken  = errors.New("missing PASETO token")
	ErrDataUnmarshal = errors.New("can't unmarshal token data to Payload type")
	pasetoObject     = paseto.NewV2()
)

// PayloadValidator Function that receives the decrypted payload and returns an interface and an error
// that's a result of validation logic
type PayloadValidator func(decrypted []byte) (interface{}, error)

// PayloadCreator Signature of a function that generates a payload token
type PayloadCreator func(key []byte, dataInfo string, duration time.Duration, purpose TokenPurpose) (string, error)

// Public helper functions

// CreateToken Create a new Token Payload that will be stored in PASETO
func CreateToken(key []byte, dataInfo string, duration time.Duration, purpose TokenPurpose) (string, error) {
	payload, err := NewPayload(dataInfo, duration)
	if err != nil {
		return "", err
	}

	switch purpose {
	case PurposeLocal:
		return pasetoObject.Encrypt(key, payload, nil)
	case PurposePublic:
		return pasetoObject.Sign(ed25519.PrivateKey(key), payload, nil)
	default:
		return pasetoObject.Encrypt(key, payload, nil)
	}
}

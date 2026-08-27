package storage

import (
	"context"
	"fmt"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
)

type externalStore struct {
	uptimestorage.Store
}

// NewExternalStore adapts a caller-owned public store to the runtime's internal
// lifecycle contract without initializing or closing the caller's resources.
func NewExternalStore(store uptimestorage.Store) Store {
	return &externalStore{Store: store}
}

func (*externalStore) Init(context.Context) error {
	return nil
}

func (*externalStore) Close() error {
	return nil
}

func (s *externalStore) Name() string {
	type named interface {
		Name() string
	}
	if store, ok := s.Store.(named); ok {
		return store.Name()
	}
	return fmt.Sprintf("%T", s.Store)
}

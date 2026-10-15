package uptime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	fiberredis "github.com/gofiber/storage/redis/v3"
)

// RedisStoreConfig configures a caller-owned Redis-backed uptime store.
type RedisStoreConfig struct {
	// Storage is required. The caller owns and closes the Fiber Redis storage.
	Storage *fiberredis.Storage
	// KeyPrefix must match Config.StorageKeyPrefix when accessing middleware data.
	// Empty uses "fiber:uptime". Leading and trailing colons are trimmed, as in
	// the built-in Redis backend; an all-colon prefix also uses the default.
	KeyPrefix string
	// RetentionDays must match Config.RetentionDays when accessing middleware data.
	// Zero defaults to 90. Negative values and values whose expiry backstop
	// (RetentionDays + 1) * 24 hours overflows time.Duration are rejected.
	RetentionDays int
}

// NewRedisStore returns an initialized Redis-backed storage.Store, ready for
// concurrent queries and writes. Initialization pings Redis using ctx and returns
// an error if Storage is missing, configuration is invalid, or initialization
// fails. Context cancellation and initialization errors can be inspected with
// errors.Is. No partially initialized store is returned on failure.
//
// WARNING: The returned Store exposes operations that directly modify monitoring
// data, including UpsertService, WriteHeartbeat, RollupDaily, and Cleanup. Do not
// modify records used by an active monitor without coordination. Use the correct
// Redis database, KeyPrefix, and RetentionDays for the data being accessed.
// Queries across multiple calls do not provide a transactionally consistent view.
//
// The caller retains ownership of Storage and its connection on both success and
// failure, and must close its resources only after all store users have stopped.
// This function starts no monitoring workers, HTTP server, or automatic maintenance.
// Writes use the same expiry backstop as the middleware: RetentionDays + 1 days
// for service, sample, and daily state, and 24 hours for instance metadata.
// RetentionDays does not schedule cleanup; callers explicitly invoke RollupDaily
// and Cleanup with the desired calendar-day boundaries when needed.
func NewRedisStore(ctx context.Context, cfg RedisStoreConfig) (uptimestorage.Store, error) {
	if cfg.Storage == nil {
		return nil, ErrMissingStore
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	days, err := normalizeRetentionDays(cfg.RetentionDays)
	if err != nil {
		return nil, err
	}
	// Validate before adding the extra day or converting to time.Duration.
	if days > int((1<<63-1)/(24*time.Hour))-1 {
		return nil, errors.New("uptime: retention days overflow Redis state TTL")
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = defaultStorageKeyPrefix
	}
	store := storage.NewRedisStore(newRedisStoreConfig(Config{
		Store: cfg.Storage, StorageKeyPrefix: cfg.KeyPrefix, RetentionDays: days,
	}))
	if err := store.Init(ctx); err != nil {
		return nil, fmt.Errorf("uptime: initialize Redis store: %w", err)
	}
	return store, nil
}

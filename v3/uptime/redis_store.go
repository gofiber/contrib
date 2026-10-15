package uptime

import (
	"context"
	"errors"
	"fmt"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	fiberredis "github.com/gofiber/storage/redis/v3"
)

// RedisStoreConfig configures a caller-owned Redis-backed uptime store.
type RedisStoreConfig struct {
	// Store is required. The caller owns and closes the Fiber Redis storage.
	Store *fiberredis.Storage
	// StorageKeyPrefix must match Config.StorageKeyPrefix when accessing middleware data.
	// Empty uses "fiber:uptime". Leading and trailing colons are trimmed, as in
	// the built-in Redis backend; an all-colon prefix also uses the default.
	StorageKeyPrefix string
	// RetentionDays must match Config.RetentionDays when accessing middleware data.
	// Zero defaults to 90. Negative values and values whose expiry backstop
	// (RetentionDays + 1) * 24 hours overflows time.Duration are rejected.
	RetentionDays int
}

// NewRedisStore returns an initialized Redis-backed storage.Store, ready for
// concurrent queries and writes. Initialization passes ctx to Redis, but network
// timeouts depend on the caller's client configuration, including go-redis's
// ContextTimeoutEnabled option. Cancellation may not immediately interrupt I/O.
// Missing storage, invalid configuration, or initialization failure returns a nil
// store and an error. Initialization errors preserve both the underlying error
// and ctx.Err(), when non-nil, for errors.Is.
//
// WARNING: The returned Store exposes operations that directly modify monitoring
// data, including UpsertService, WriteHeartbeat, RollupDaily, and Cleanup. Do not
// modify records used by an active monitor without coordination. Use the correct
// Redis database, StorageKeyPrefix, and RetentionDays for the data being accessed.
// Queries across multiple calls do not provide a transactionally consistent view.
//
// The caller retains ownership of Store and its connection on both success and
// failure, and must close its resources only after all store users have stopped.
// This function starts no monitoring workers, HTTP server, or automatic maintenance.
// Writes use the same expiry backstop as the middleware: RetentionDays + 1 days
// for service, sample, and daily state, and 24 hours for instance metadata.
// RetentionDays does not schedule cleanup; callers explicitly invoke RollupDaily
// and Cleanup with the desired calendar-day boundaries when needed.
func NewRedisStore(ctx context.Context, cfg RedisStoreConfig) (uptimestorage.Store, error) {
	if cfg.Store == nil {
		return nil, ErrMissingStore
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	days, err := normalizeRetentionDays(cfg.RetentionDays)
	if err != nil {
		return nil, err
	}
	if cfg.StorageKeyPrefix == "" {
		cfg.StorageKeyPrefix = defaultStorageKeyPrefix
	}
	redisConfig, err := newRedisStoreConfig(Config{
		Store: cfg.Store, StorageKeyPrefix: cfg.StorageKeyPrefix, RetentionDays: days,
	})
	if err != nil {
		return nil, err
	}
	store := storage.NewRedisStore(redisConfig)
	if err := store.Init(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(err, ctxErr)
		}
		return nil, fmt.Errorf("uptime: initialize Redis store: %w", err)
	}
	return store, nil
}

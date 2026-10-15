package uptime_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/gofiber/fiber/v3"
	fiberredis "github.com/gofiber/storage/redis/v3"
	"github.com/redis/go-redis/v9"
)

var _ func(context.Context, uptime.RedisStoreConfig) (uptimestorage.Store, error) = uptime.NewRedisStore

func TestNewRedisStoreValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  uptime.RedisStoreConfig
		want string
	}{
		{"missing storage", uptime.RedisStoreConfig{}, "storage is required"},
		{"missing connection", uptime.RedisStoreConfig{Store: new(fiberredis.Storage)}, "client is required"},
		{"negative retention", uptime.RedisStoreConfig{Store: new(fiberredis.Storage), RetentionDays: -1}, "retention days must be positive"},
		{"TTL overflow", uptime.RedisStoreConfig{Store: new(fiberredis.Storage), RetentionDays: 106751}, "overflow"},
		{"integer overflow", uptime.RedisStoreConfig{Store: new(fiberredis.Storage), RetentionDays: int(^uint(0) >> 1)}, "overflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := uptime.NewRedisStore(context.Background(), tc.cfg)
			if store != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("store = %v, error = %v, want nil store and %q", store, err, tc.want)
			}
			if tc.cfg.Store == nil && !errors.Is(err, uptime.ErrMissingStore) {
				t.Fatalf("error = %v, want ErrMissingStore", err)
			}
			if tc.want == "overflow" && !errors.Is(err, uptime.ErrRetentionDaysOverflow) {
				t.Fatalf("error = %v, want ErrRetentionDaysOverflow", err)
			}
		})
	}
}

func TestNewRedisStoreCanceledContext(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	for _, ctx := range []context.Context{canceled, expired} {
		store, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: new(fiberredis.Storage)})
		if store != nil || !errors.Is(err, ctx.Err()) {
			t.Fatalf("store = %v, error = %v, want %v", store, err, ctx.Err())
		}
	}
}

func TestNewRedisStoreInitializationFailure(t *testing.T) {
	t.Parallel()
	dialErr := errors.New("test dial failure")
	client, borrowed := redisStoreTestConnection(t, &redis.Options{
		MaxRetries: -1,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, dialErr
		},
	})
	store, err := uptime.NewRedisStore(context.Background(), uptime.RedisStoreConfig{Store: borrowed})
	if store != nil || !errors.Is(err, dialErr) {
		t.Fatalf("store = %v, error = %v, want dial failure", store, err)
	}
	// A failed factory must leave both the borrowed handle and its client open.
	if err := borrowed.Set("unused", []byte("value"), 0); !errors.Is(err, dialErr) {
		t.Fatalf("borrowed storage after failure: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = uptime.NewRedisStore(context.Background(), uptime.RedisStoreConfig{Store: borrowed})
	if store != nil || !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("store = %v, error = %v, want closed connection", store, err)
	}
}

func TestNewRedisStoreCancellationDuringInit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, borrowed := redisStoreTestConnection(t, &redis.Options{
		MaxRetries: -1,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			cancel()
			return nil, ctx.Err()
		},
	})
	store, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: borrowed})
	if store != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("store = %v, error = %v, want context.Canceled", store, err)
	}
}

func TestNewRedisStoreIntegrationQueriesAndWrites(t *testing.T) {
	ctx, client, borrowed := redisStoreTestRedis(t)
	prefix := redisStoreTestPrefix(t, client)
	store, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: borrowed, StorageKeyPrefix: prefix})
	redisStoreRequireNoError(t, err)
	services, err := store.ListServices(ctx)
	redisStoreRequireNoError(t, err)
	if len(services) != 0 {
		t.Fatalf("factory registered services: %+v", services)
	}
	_, err = store.QueryDaily(ctx, uptimestorage.QueryDailyOptions{})
	redisStoreRequireNoError(t, err)
	_, err = store.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{Day: "2026-06-25"})
	redisStoreRequireNoError(t, err)

	now := time.Now().UTC()
	redisStoreRequireNoError(t, store.UpsertService(ctx, uptimestorage.Service{ID: "api", Name: "API", CreatedAt: now, SampleInterval: time.Minute}))
	redisStoreRequireNoError(t, store.UpsertInstance(ctx, uptimestorage.Instance{ID: 1, ServiceID: "api", StartedAt: now}))
	redisStoreRequireNoError(t, store.WriteHeartbeat(ctx, uptimestorage.Heartbeat{ServiceID: "api", InstanceID: 1, Day: "2026-06-25", Slot: 0, SeenAt: now}))
	samples, err := store.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{Day: "2026-06-25"})
	redisStoreRequireNoError(t, err)
	if len(samples) != 1 || samples[0].UpSlots != 1 {
		t.Fatalf("samples = %+v, want one heartbeat", samples)
	}
	redisStoreRequireNoError(t, store.RollupDaily(ctx, uptimestorage.RollupOptions{
		BeforeDay: "2026-06-26", ExpectedSlots: func(uptimestorage.Service, string) int { return 1440 },
	}))
	// A second factory must neither clear data nor run retention maintenance.
	reader, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: borrowed, StorageKeyPrefix: prefix, RetentionDays: 1})
	redisStoreRequireNoError(t, err)
	daily, err := reader.QueryDaily(ctx, uptimestorage.QueryDailyOptions{})
	redisStoreRequireNoError(t, err)
	if len(daily) != 1 || daily[0].UpSlots != 1 || daily[0].ExpectedSlots != 1440 || !daily[0].Finalized {
		t.Fatalf("daily = %+v, want finalized history", daily)
	}
	redisStoreRequireNoError(t, store.Cleanup(ctx, uptimestorage.CleanupOptions{SamplesBeforeDay: "2026-06-26", DailyBeforeDay: "2026-06-26"}))
	daily, err = reader.QueryDaily(ctx, uptimestorage.QueryDailyOptions{})
	redisStoreRequireNoError(t, err)
	samples, err = reader.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{Day: "2026-06-25"})
	redisStoreRequireNoError(t, err)
	if len(daily) != 0 || len(samples) != 0 {
		t.Fatal("explicit cleanup did not remove history")
	}
	redisStoreRequireNoError(t, uptime.RemoveService(ctx, borrowed, prefix, "api"))
	services, err = store.ListServices(ctx)
	redisStoreRequireNoError(t, err)
	if len(services) != 0 {
		t.Fatal("RemoveService did not remove the factory's service")
	}
	redisStoreRequireNoError(t, borrowed.Set(prefix+":owner", []byte("still open"), 0))
	redisStoreRequireNoError(t, client.Ping(ctx).Err())
}

func TestNewRedisStoreDefaultsAndRetention(t *testing.T) {
	t.Parallel()
	// Intercept commands locally: never read or write a shared default namespace.
	for _, tc := range []struct {
		prefix string
		days   int
	}{
		{"", 0},
		{":::", 0},
		{"", 1},
		{"", 90},
		{"", 106750},
	} {
		client, borrowed := redisStoreTestConnection(t, &redis.Options{})
		hook := &redisStorePrefixHook{}
		client.AddHook(hook)
		store, err := uptime.NewRedisStore(context.Background(), uptime.RedisStoreConfig{
			Store: borrowed, StorageKeyPrefix: tc.prefix, RetentionDays: tc.days,
		})
		redisStoreRequireNoError(t, err)
		_, err = store.ListServices(context.Background())
		redisStoreRequireNoError(t, err)
		if hook.key != "fiber:uptime:services" {
			t.Fatalf("prefix %q queried %q, want default namespace", tc.prefix, hook.key)
		}
	}
}

type redisStorePrefixHook struct{ key string }

func (*redisStorePrefixHook) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("default prefix test must not connect to Redis")
	}
}

func (h *redisStorePrefixHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		switch cmd.Name() {
		case "ping":
			cmd.(*redis.StatusCmd).SetVal("PONG")
		case "smembers":
			h.key = cmd.Args()[1].(string)
			cmd.(*redis.StringSliceCmd).SetVal(nil)
		default:
			return errors.New("unexpected command in default prefix test")
		}
		return nil
	}
}

func (*redisStorePrefixHook) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error {
		return errors.New("unexpected pipeline in default prefix test")
	}
}

func TestNewRedisStoreIntegrationPrefixes(t *testing.T) {
	ctx, client, borrowed := redisStoreTestRedis(t)
	prefix := redisStoreTestPrefix(t, client)
	writer, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: borrowed, StorageKeyPrefix: ":" + prefix + ":"})
	redisStoreRequireNoError(t, err)
	redisStoreRequireNoError(t, writer.UpsertService(ctx, uptimestorage.Service{ID: "api", Name: "API", SampleInterval: time.Minute}))
	reader, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: borrowed, StorageKeyPrefix: prefix})
	redisStoreRequireNoError(t, err)
	services, err := reader.ListServices(ctx)
	redisStoreRequireNoError(t, err)
	if len(services) != 1 || services[0].ID != "api" {
		t.Fatalf("services = %+v, want api in canonical namespace", services)
	}
	isolated, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: borrowed, StorageKeyPrefix: prefix + ":isolated"})
	redisStoreRequireNoError(t, err)
	services, err = isolated.ListServices(ctx)
	redisStoreRequireNoError(t, err)
	if len(services) != 0 {
		t.Fatal("data crossed Redis namespaces")
	}
}

func TestNewRedisStoreIntegrationRuntimeAndTTL(t *testing.T) {
	ctx, client, borrowed := redisStoreTestRedis(t)
	for _, tc := range []struct {
		name string
		days int
		ttl  time.Duration
	}{{"default", 0, 91 * 24 * time.Hour}, {"one day", 1, 2 * 24 * time.Hour}, {"custom", 7, 8 * 24 * time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := redisStoreTestPrefix(t, client)
			store, err := uptime.NewRedisStore(ctx, uptime.RedisStoreConfig{Store: borrowed, StorageKeyPrefix: prefix, RetentionDays: tc.days})
			redisStoreRequireNoError(t, err)
			// Exercise both existing middleware entry points with the public reader.
			for _, custom := range []bool{false, true} {
				app := fiber.New()
				t.Cleanup(func() { _ = app.Shutdown() })
				cfg := uptime.Config{App: app, Store: borrowed, StorageKeyPrefix: prefix, ServiceID: "api", SampleInterval: time.Hour, RetentionDays: tc.days, Timezone: time.UTC}
				if custom {
					cfg.Store, cfg.Storage = nil, store
				}
				app.Use(uptime.New(cfg))
				resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/uptime/api/status", nil))
				redisStoreRequireNoError(t, err)
				var status uptime.StatusResponse
				err = json.NewDecoder(resp.Body).Decode(&status)
				resp.Body.Close()
				redisStoreRequireNoError(t, err)
				if resp.StatusCode != fiber.StatusOK || status.Storage.Driver != "redis" || len(status.Services) != 1 || status.Services[0].ID != "api" {
					t.Fatalf("runtime status = %+v", status)
				}
				redisStoreRequireNoError(t, app.Shutdown())
				redisStoreAssertTTL(t, ctx, client, prefix+":service:api", tc.ttl)
				redisStoreRequireNoError(t, borrowed.Set(prefix+":owner", []byte("still open"), 0))
			}
			now := time.Now().UTC()
			redisStoreRequireNoError(t, store.UpsertService(ctx, uptimestorage.Service{ID: "external", LastSeenAt: now, SampleInterval: time.Hour}))
			redisStoreAssertTTL(t, ctx, client, prefix+":service:external", tc.ttl)
			redisStoreRequireNoError(t, store.WriteHeartbeat(ctx, uptimestorage.Heartbeat{ServiceID: "external", InstanceID: 1, Day: "2026-06-25", Slot: 0, SeenAt: now}))
			redisStoreRequireNoError(t, store.RollupDaily(ctx, uptimestorage.RollupOptions{BeforeDay: "2026-06-26"}))
			for _, suffix := range []string{"service:external", "samples:external:2026-06-25", "sample-days:external", "daily:external:2026-06-25", "daily-days:external"} {
				redisStoreAssertTTL(t, ctx, client, prefix+":"+suffix, tc.ttl)
			}
			redisStoreAssertTTL(t, ctx, client, prefix+":instance:1", 24*time.Hour)
			if ttl := client.TTL(ctx, prefix+":services").Val(); ttl != -1 {
				t.Fatalf("registry TTL = %v, want no expiry", ttl)
			}
		})
	}
}

func redisStoreTestConnection(t *testing.T, options *redis.Options) (*redis.Client, *fiberredis.Storage) {
	t.Helper()
	client := redis.NewClient(options)
	borrowed := fiberredis.NewFromConnection(client)
	t.Cleanup(func() { _ = borrowed.Close(); _ = client.Close() })
	return client, borrowed
}

func redisStoreTestRedis(t *testing.T) (context.Context, *redis.Client, *fiberredis.Storage) {
	t.Helper()
	url := os.Getenv("UPTIME_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set UPTIME_TEST_REDIS_URL to run against a disposable Redis database")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("invalid UPTIME_TEST_REDIS_URL")
	}
	options.ContextTimeoutEnabled = true
	client, borrowed := redisStoreTestConnection(t, options)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx, client, borrowed
}

func redisStoreTestPrefix(t *testing.T, client *redis.Client) string {
	t.Helper()
	prefix := "uptime:test:" + rand.Text()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Runtime instance IDs are generated dynamically. Scan only this test's
		// random namespace, after its runtimes have shut down.
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, prefix+":*", 100).Result()
			redisStoreRequireNoError(t, err)
			if len(keys) > 0 {
				redisStoreRequireNoError(t, client.Del(ctx, keys...).Err())
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	})
	return prefix
}

func redisStoreAssertTTL(t *testing.T, ctx context.Context, client *redis.Client, key string, want time.Duration) {
	t.Helper()
	got, err := client.PTTL(ctx, key).Result()
	redisStoreRequireNoError(t, err)
	if got <= want-5*time.Second || got > want {
		t.Fatalf("TTL for %s = %v, want approximately %v", key, got, want)
	}
}

func redisStoreRequireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

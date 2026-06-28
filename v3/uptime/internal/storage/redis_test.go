package storage

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisStoreRollupAndCleanupLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newFakeRedisClient()
	store := newRedisStoreWithClient(RedisConfig{KeyPrefix: "test:uptime"}, client)
	mustNoErr(t, store.Init(ctx))
	t.Cleanup(func() { mustNoErr(t, store.Close()) })

	created := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	mustNoErr(t, store.UpsertService(ctx, Service{ID: "api", Name: "API", CreatedAt: created, LastSeenAt: created, SampleInterval: time.Minute}))
	mustNoErr(t, store.UpsertInstance(ctx, Instance{ID: 1, ServiceID: "api", StartedAt: created, LastSeenAt: created}))

	seenAt := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	writeHeartbeat := func(day string, slot int64) {
		mustNoErr(t, store.WriteHeartbeat(ctx, Heartbeat{ServiceID: "api", InstanceID: 1, Day: day, Slot: slot, SeenAt: seenAt}))
	}
	writeHeartbeat("2026-06-23", 5)
	writeHeartbeat("2026-06-25", 0)
	writeHeartbeat("2026-06-25", 1)
	writeHeartbeat("2026-06-25", 1) // duplicate slot must collapse to one up slot

	mustNoErr(t, store.RollupDaily(ctx, RollupOptions{
		BeforeDay:                  "2026-06-26",
		ExpectedSlotsForServiceDay: func(string, string) int { return 1440 },
	}))

	daily := queryDailyMap(t, store)
	if got := daily["2026-06-25"]; got.UpSlots != 2 || got.ExpectedSlots != 1440 || !got.Finalized {
		t.Fatalf("rolled-up day = %+v, want up=2 expected=1440 finalized=true", got)
	}
	if _, ok := daily["2026-06-23"]; !ok {
		t.Fatal("expected daily row for 2026-06-23 before cleanup")
	}

	mustNoErr(t, store.Cleanup(ctx, CleanupOptions{
		DailyBeforeDay:   "2026-06-24",
		SamplesBeforeDay: "2026-06-25",
	}))

	daily = queryDailyMap(t, store)
	if _, ok := daily["2026-06-23"]; ok {
		t.Fatal("daily row for 2026-06-23 should be removed by retention")
	}
	if _, ok := daily["2026-06-25"]; !ok {
		t.Fatal("daily row for 2026-06-25 should survive retention")
	}

	today, err := store.QueryTodaySamples(ctx, QueryTodaySamplesOptions{Day: "2026-06-25"})
	mustNoErr(t, err)
	if len(today) != 1 || today[0].UpSlots != 2 {
		t.Fatalf("samples for 2026-06-25 = %+v, want one row with up=2", today)
	}
	old, err := store.QueryTodaySamples(ctx, QueryTodaySamplesOptions{Day: "2026-06-23"})
	mustNoErr(t, err)
	if len(old) != 0 {
		t.Fatalf("samples for 2026-06-23 should be removed by retention, got %+v", old)
	}
}

func TestRedisStoreKeepsMaxLastSeenAt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newRedisStoreWithClient(RedisConfig{KeyPrefix: "test:uptime"}, newFakeRedisClient())
	mustNoErr(t, store.Init(ctx))
	t.Cleanup(func() { mustNoErr(t, store.Close()) })

	first := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	second := first.Add(-time.Hour)
	mustNoErr(t, store.UpsertService(ctx, Service{ID: "api", Name: "API", CreatedAt: first, LastSeenAt: first, SampleInterval: time.Minute}))
	mustNoErr(t, store.UpsertService(ctx, Service{ID: "api", Name: "API", CreatedAt: first, LastSeenAt: second, SampleInterval: time.Minute}))

	services, err := store.ListServices(ctx)
	mustNoErr(t, err)
	if len(services) != 1 {
		t.Fatalf("services len = %d, want 1", len(services))
	}
	if !services[0].LastSeenAt.Equal(first) {
		t.Fatalf("last seen = %s, want %s", services[0].LastSeenAt, first)
	}
}

func queryDailyMap(t *testing.T, store *RedisStore) map[string]DailyStatus {
	t.Helper()

	rows, err := store.QueryDaily(context.Background(), QueryDailyOptions{FromDay: "2026-06-01", ToDay: "2026-06-30"})
	mustNoErr(t, err)
	out := make(map[string]DailyStatus, len(rows))
	for _, row := range rows {
		out[row.Day] = row
	}
	return out
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakeRedisClient struct {
	hashes map[string]map[string]string
	sets   map[string]map[string]struct{}
	zsets  map[string]map[string]float64
	closed bool
}

func newFakeRedisClient() *fakeRedisClient {
	return &fakeRedisClient{
		hashes: make(map[string]map[string]string),
		sets:   make(map[string]map[string]struct{}),
		zsets:  make(map[string]map[string]float64),
	}
}

func (c *fakeRedisClient) Ping(context.Context) *redis.StatusCmd {
	return redis.NewStatusResult("PONG", nil)
}

func (c *fakeRedisClient) SAdd(_ context.Context, key string, members ...interface{}) *redis.IntCmd {
	set := c.sets[key]
	if set == nil {
		set = make(map[string]struct{})
		c.sets[key] = set
	}
	var added int64
	for _, member := range members {
		value := fmt.Sprint(member)
		if _, ok := set[value]; !ok {
			set[value] = struct{}{}
			added++
		}
	}
	return redis.NewIntResult(added, nil)
}

func (c *fakeRedisClient) SMembers(_ context.Context, key string) *redis.StringSliceCmd {
	set := c.sets[key]
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return redis.NewStringSliceResult(values, nil)
}

func (c *fakeRedisClient) SCard(_ context.Context, key string) *redis.IntCmd {
	return redis.NewIntResult(int64(len(c.sets[key])), nil)
}

func (c *fakeRedisClient) HSet(_ context.Context, key string, values ...interface{}) *redis.IntCmd {
	hash := c.hashes[key]
	if hash == nil {
		hash = make(map[string]string)
		c.hashes[key] = hash
	}
	for i := 0; i+1 < len(values); i += 2 {
		hash[fmt.Sprint(values[i])] = fmt.Sprint(values[i+1])
	}
	return redis.NewIntResult(0, nil)
}

func (c *fakeRedisClient) HSetNX(_ context.Context, key, field string, value interface{}) *redis.BoolCmd {
	hash := c.hashes[key]
	if hash == nil {
		hash = make(map[string]string)
		c.hashes[key] = hash
	}
	if _, ok := hash[field]; ok {
		return redis.NewBoolResult(false, nil)
	}
	hash[field] = fmt.Sprint(value)
	return redis.NewBoolResult(true, nil)
}

func (c *fakeRedisClient) HGet(_ context.Context, key, field string) *redis.StringCmd {
	hash := c.hashes[key]
	if hash == nil {
		return redis.NewStringResult("", redis.Nil)
	}
	value, ok := hash[field]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

func (c *fakeRedisClient) HGetAll(_ context.Context, key string) *redis.MapStringStringCmd {
	hash := c.hashes[key]
	out := make(map[string]string, len(hash))
	for field, value := range hash {
		out[field] = value
	}
	return redis.NewMapStringStringResult(out, nil)
}

func (c *fakeRedisClient) ZAdd(_ context.Context, key string, members ...redis.Z) *redis.IntCmd {
	zset := c.zsets[key]
	if zset == nil {
		zset = make(map[string]float64)
		c.zsets[key] = zset
	}
	var added int64
	for _, member := range members {
		value := fmt.Sprint(member.Member)
		if _, ok := zset[value]; !ok {
			added++
		}
		zset[value] = member.Score
	}
	return redis.NewIntResult(added, nil)
}

func (c *fakeRedisClient) ZRangeByScore(_ context.Context, key string, opt *redis.ZRangeBy) *redis.StringSliceCmd {
	zset := c.zsets[key]
	values := make([]string, 0, len(zset))
	for member, score := range zset {
		if redisScoreInRange(score, opt.Min, opt.Max) {
			values = append(values, member)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		left := zset[values[i]]
		right := zset[values[j]]
		if left == right {
			return values[i] < values[j]
		}
		return left < right
	})
	return redis.NewStringSliceResult(values, nil)
}

func (c *fakeRedisClient) ZRem(_ context.Context, key string, members ...interface{}) *redis.IntCmd {
	zset := c.zsets[key]
	var removed int64
	for _, member := range members {
		value := fmt.Sprint(member)
		if _, ok := zset[value]; ok {
			delete(zset, value)
			removed++
		}
	}
	return redis.NewIntResult(removed, nil)
}

func (c *fakeRedisClient) Del(_ context.Context, keys ...string) *redis.IntCmd {
	var removed int64
	for _, key := range keys {
		if _, ok := c.hashes[key]; ok {
			delete(c.hashes, key)
			removed++
		}
		if _, ok := c.sets[key]; ok {
			delete(c.sets, key)
			removed++
		}
		if _, ok := c.zsets[key]; ok {
			delete(c.zsets, key)
			removed++
		}
	}
	return redis.NewIntResult(removed, nil)
}

func (c *fakeRedisClient) Close() error {
	c.closed = true
	return nil
}

func redisScoreInRange(score float64, minRaw, maxRaw string) bool {
	min, minExclusive := parseRedisBoundary(minRaw, true)
	max, maxExclusive := parseRedisBoundary(maxRaw, false)
	if minExclusive {
		if score <= min {
			return false
		}
	} else if score < min {
		return false
	}
	if maxExclusive {
		if score >= max {
			return false
		}
	} else if score > max {
		return false
	}
	return true
}

func parseRedisBoundary(raw string, isMin bool) (float64, bool) {
	if raw == "" {
		if isMin {
			return math.Inf(-1), false
		}
		return math.Inf(1), false
	}
	if raw == "-inf" {
		return math.Inf(-1), false
	}
	if raw == "+inf" {
		return math.Inf(1), false
	}
	exclusive := strings.HasPrefix(raw, "(")
	raw = strings.TrimPrefix(raw, "(")
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		panic(err)
	}
	return value, exclusive
}

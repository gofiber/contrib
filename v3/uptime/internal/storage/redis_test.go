package storage

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisStoreRollupAndCleanupLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newFakeRedisClient()
	store := &RedisStore{config: RedisConfig{KeyPrefix: "test:uptime"}, client: client}
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

func TestRedisStoreRollupDoesNotOverwriteFinalizedDailyWhenSamplesAreGone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newFakeRedisClient()
	first := &RedisStore{config: RedisConfig{KeyPrefix: "test:uptime"}, client: client}
	second := &RedisStore{config: RedisConfig{KeyPrefix: "test:uptime"}, client: client}
	mustNoErr(t, first.Init(ctx))
	mustNoErr(t, second.Init(ctx))
	t.Cleanup(func() { mustNoErr(t, first.Close()) })
	t.Cleanup(func() { mustNoErr(t, second.Close()) })

	created := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	service := Service{ID: "api", Name: "API", CreatedAt: created, LastSeenAt: created, SampleInterval: time.Minute}
	mustNoErr(t, first.UpsertService(ctx, service))
	mustNoErr(t, first.WriteHeartbeat(ctx, Heartbeat{ServiceID: "api", InstanceID: 1, Day: "2026-06-25", Slot: 0, SeenAt: created}))
	mustNoErr(t, first.WriteHeartbeat(ctx, Heartbeat{ServiceID: "api", InstanceID: 1, Day: "2026-06-25", Slot: 1, SeenAt: created}))

	mustNoErr(t, first.RollupDaily(ctx, RollupOptions{
		BeforeDay:                  "2026-06-26",
		ExpectedSlotsForServiceDay: func(string, string) int { return 1440 },
	}))
	before := queryDailyMap(t, first)["2026-06-25"]
	if before.UpSlots != 2 || before.ExpectedSlots != 1440 || !before.Finalized {
		t.Fatalf("daily before stale rollup = %+v, want up=2 expected=1440 finalized=true", before)
	}

	mustNoErr(t, client.Del(ctx, first.sampleKey("api", "2026-06-25")).Err())
	mustNoErr(t, second.RollupDaily(ctx, RollupOptions{
		BeforeDay:                  "2026-06-26",
		ExpectedSlotsForServiceDay: func(string, string) int { return 1440 },
	}))

	after := queryDailyMap(t, first)["2026-06-25"]
	if after.UpSlots != 2 || after.ExpectedSlots != 1440 || !after.Finalized {
		t.Fatalf("daily after stale rollup = %+v, want original finalized row", after)
	}
}

func TestRedisStoreCleanupKeepsSamplesUntilDailyFinalized(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &RedisStore{config: RedisConfig{KeyPrefix: "test:uptime"}, client: newFakeRedisClient()}
	mustNoErr(t, store.Init(ctx))
	t.Cleanup(func() { mustNoErr(t, store.Close()) })

	created := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	mustNoErr(t, store.UpsertService(ctx, Service{ID: "api", Name: "API", CreatedAt: created, LastSeenAt: created, SampleInterval: time.Minute}))
	mustNoErr(t, store.WriteHeartbeat(ctx, Heartbeat{ServiceID: "api", InstanceID: 1, Day: "2026-06-25", Slot: 7, SeenAt: created}))

	mustNoErr(t, store.Cleanup(ctx, CleanupOptions{SamplesBeforeDay: "2026-06-26"}))

	today, err := store.QueryTodaySamples(ctx, QueryTodaySamplesOptions{Day: "2026-06-25"})
	mustNoErr(t, err)
	if len(today) != 1 || today[0].UpSlots != 1 {
		t.Fatalf("samples before finalized daily = %+v, want retained sample", today)
	}

	mustNoErr(t, store.RollupDaily(ctx, RollupOptions{
		BeforeDay:                  "2026-06-26",
		ExpectedSlotsForServiceDay: func(string, string) int { return 1440 },
	}))
	mustNoErr(t, store.Cleanup(ctx, CleanupOptions{SamplesBeforeDay: "2026-06-26"}))

	today, err = store.QueryTodaySamples(ctx, QueryTodaySamplesOptions{Day: "2026-06-25"})
	mustNoErr(t, err)
	if len(today) != 0 {
		t.Fatalf("samples after finalized daily should be removed, got %+v", today)
	}
}

func TestRedisStoreCloseDoesNotInvalidateConcurrentReaders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &RedisStore{config: RedisConfig{KeyPrefix: "test:uptime"}, client: newFakeRedisClient()}
	mustNoErr(t, store.Init(ctx))
	t.Cleanup(func() { mustNoErr(t, store.Close()) })

	created := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	mustNoErr(t, store.UpsertService(ctx, Service{ID: "api", Name: "API", CreatedAt: created, LastSeenAt: created, SampleInterval: time.Minute}))

	const readers = 8
	ready := make(chan struct{}, readers)
	start := make(chan struct{})
	done := make(chan struct{})
	errCh := make(chan error, readers)

	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}

				services, err := store.ListServices(ctx)
				if err != nil {
					errCh <- err
					return
				}
				if len(services) != 1 || services[0].ID != "api" {
					errCh <- fmt.Errorf("services = %+v, want api", services)
					return
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		<-ready
	}

	close(start)
	for i := 0; i < 1000; i++ {
		mustNoErr(t, store.Close())
	}
	close(done)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	services, err := store.ListServices(ctx)
	mustNoErr(t, err)
	if len(services) != 1 || services[0].ID != "api" {
		t.Fatalf("services after close = %+v, want api", services)
	}
}

func TestRedisStoreKeepsMaxLastSeenAt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &RedisStore{config: RedisConfig{KeyPrefix: "test:uptime"}, client: newFakeRedisClient()}
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

func TestRedisStoreKeepsMaxLastSeenAtConcurrently(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &RedisStore{config: RedisConfig{KeyPrefix: "test:uptime"}, client: newFakeRedisClient()}
	mustNoErr(t, store.Init(ctx))
	t.Cleanup(func() { mustNoErr(t, store.Close()) })

	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	latest := base.Add(500 * time.Minute)
	mustNoErr(t, store.UpsertService(ctx, Service{ID: "api", Name: "API", CreatedAt: base, LastSeenAt: base, SampleInterval: time.Minute}))
	mustNoErr(t, store.UpsertInstance(ctx, Instance{ID: 1, ServiceID: "api", StartedAt: base, LastSeenAt: base}))

	var wg sync.WaitGroup
	for i := 0; i <= 500; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			seenAt := base.Add(time.Duration(i) * time.Minute)
			if i%3 == 0 {
				seenAt = latest.Add(-time.Duration(i) * time.Second)
			}
			if i == 250 {
				seenAt = latest
			}
			mustNoErr(t, store.WriteHeartbeat(ctx, Heartbeat{
				ServiceID:  "api",
				InstanceID: 1,
				Day:        seenAt.Format("2006-01-02"),
				Slot:       int64(i),
				SeenAt:     seenAt,
			}))
		}()
	}
	wg.Wait()

	services, err := store.ListServices(ctx)
	mustNoErr(t, err)
	if len(services) != 1 {
		t.Fatalf("services len = %d, want 1", len(services))
	}
	if !services[0].LastSeenAt.Equal(latest) {
		t.Fatalf("last seen = %s, want %s", services[0].LastSeenAt, latest)
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
	mu     sync.Mutex
	hashes map[string]map[string]string
	sets   map[string]map[string]struct{}
	zsets  map[string]map[string]float64
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
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

	set := c.sets[key]
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return redis.NewStringSliceResult(values, nil)
}

func (c *fakeRedisClient) SCard(_ context.Context, key string) *redis.IntCmd {
	c.mu.Lock()
	defer c.mu.Unlock()

	return redis.NewIntResult(int64(len(c.sets[key])), nil)
}

func (c *fakeRedisClient) HSet(_ context.Context, key string, values ...interface{}) *redis.IntCmd {
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

	hash := c.hashes[key]
	out := make(map[string]string, len(hash))
	for field, value := range hash {
		out[field] = value
	}
	return redis.NewMapStringStringResult(out, nil)
}

func (c *fakeRedisClient) ZAdd(_ context.Context, key string, members ...redis.Z) *redis.IntCmd {
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

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
	c.mu.Lock()
	defer c.mu.Unlock()

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

func (c *fakeRedisClient) Eval(_ context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	switch script {
	case setMaxNanoScript:
		return c.evalSetMaxNano(keys, args...)
	case writeDailyIfUnfinalizedScript:
		return c.evalWriteDailyIfUnfinalized(keys, args...)
	default:
		return redis.NewCmdResult(nil, fmt.Errorf("unexpected eval script"))
	}
}

func (c *fakeRedisClient) evalSetMaxNano(keys []string, args ...interface{}) *redis.Cmd {
	if len(keys) != 1 || len(args) != 2 {
		return redis.NewCmdResult(nil, fmt.Errorf("unexpected eval call"))
	}
	field := fmt.Sprint(args[0])
	value, err := strconv.ParseInt(fmt.Sprint(args[1]), 10, 64)
	if err != nil {
		return redis.NewCmdResult(nil, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	hash := c.hashes[keys[0]]
	if hash == nil {
		hash = make(map[string]string)
		c.hashes[keys[0]] = hash
	}
	current := hash[field]
	if current != "" {
		currentValue, err := strconv.ParseInt(current, 10, 64)
		if err != nil {
			return redis.NewCmdResult(nil, err)
		}
		if currentValue >= value {
			return redis.NewCmdResult(int64(0), nil)
		}
	}
	hash[field] = strconv.FormatInt(value, 10)
	return redis.NewCmdResult(int64(1), nil)
}

func (c *fakeRedisClient) evalWriteDailyIfUnfinalized(keys []string, args ...interface{}) *redis.Cmd {
	if len(keys) != 2 || len(args) != 7 {
		return redis.NewCmdResult(nil, fmt.Errorf("unexpected write daily eval call"))
	}

	dailyKey := keys[0]
	daysKey := keys[1]
	serviceID := fmt.Sprint(args[0])
	day := fmt.Sprint(args[1])
	upSlots := fmt.Sprint(args[2])
	expectedSlots := fmt.Sprint(args[3])
	uptimeRate := fmt.Sprint(args[4])
	finalized := fmt.Sprint(args[5])
	score, err := strconv.ParseFloat(fmt.Sprint(args[6]), 64)
	if err != nil {
		return redis.NewCmdResult(nil, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	hash := c.hashes[dailyKey]
	if hash != nil {
		current := hash["finalized"]
		if current != "" && current != "0" {
			c.zaddLocked(daysKey, day, score)
			return redis.NewCmdResult(int64(0), nil)
		}
	}
	if hash == nil {
		hash = make(map[string]string)
		c.hashes[dailyKey] = hash
	}
	hash["service_id"] = serviceID
	hash["day"] = day
	hash["up_slots"] = upSlots
	hash["expected_slots"] = expectedSlots
	hash["uptime_rate"] = uptimeRate
	hash["finalized"] = finalized
	c.zaddLocked(daysKey, day, score)
	return redis.NewCmdResult(int64(1), nil)
}

func (c *fakeRedisClient) zaddLocked(key, member string, score float64) {
	zset := c.zsets[key]
	if zset == nil {
		zset = make(map[string]float64)
		c.zsets[key] = zset
	}
	zset[member] = score
}

func (c *fakeRedisClient) Pipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error) {
	pipe := &fakeRedisPipeline{ctx: ctx, client: c}
	if err := fn(pipe); err != nil {
		return nil, err
	}
	return pipe.cmds, nil
}

type fakeRedisPipeline struct {
	redis.Pipeliner
	ctx    context.Context
	client *fakeRedisClient
	cmds   []redis.Cmder
}

func (p *fakeRedisPipeline) HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd {
	if ctx == nil {
		ctx = p.ctx
	}
	cmd := p.client.HGetAll(ctx, key)
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *fakeRedisPipeline) SCard(ctx context.Context, key string) *redis.IntCmd {
	if ctx == nil {
		ctx = p.ctx
	}
	cmd := p.client.SCard(ctx, key)
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *fakeRedisPipeline) ZRangeByScore(ctx context.Context, key string, opt *redis.ZRangeBy) *redis.StringSliceCmd {
	if ctx == nil {
		ctx = p.ctx
	}
	cmd := p.client.ZRangeByScore(ctx, key, opt)
	p.cmds = append(p.cmds, cmd)
	return cmd
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

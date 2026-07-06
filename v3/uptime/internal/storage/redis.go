package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	fiberredis "github.com/gofiber/storage/redis/v3"
	"github.com/redis/go-redis/v9"
)

const defaultRedisKeyPrefix = "fiber:uptime"

// setMaxNanoScript stores ARGV[2] in hash field ARGV[1] on KEYS[1] when
// the current value is missing or lower. Keeping the comparison inside Redis
// prevents older concurrent heartbeats from moving last_seen_at backwards.
const setMaxNanoScript = `
local current = redis.call("HGET", KEYS[1], ARGV[1])
if (not current) or tonumber(current) < tonumber(ARGV[2]) then
	redis.call("HSET", KEYS[1], ARGV[1], ARGV[2])
	return 1
end
return 0
`

// writeDailyIfUnfinalizedScript writes a finalized daily row only while the
// row is not already finalized. This keeps late concurrent rollups from
// replacing a previously finalized day after another instance has cleaned up
// the raw samples.
const writeDailyIfUnfinalizedScript = `
local finalized = redis.call("HGET", KEYS[1], "finalized")
if finalized and finalized ~= "0" then
	redis.call("ZADD", KEYS[2], ARGV[7], ARGV[2])
	return 0
end
redis.call("HSET", KEYS[1],
	"service_id", ARGV[1],
	"day", ARGV[2],
	"up_slots", ARGV[3],
	"expected_slots", ARGV[4],
	"uptime_rate", ARGV[5],
	"finalized", ARGV[6])
redis.call("ZADD", KEYS[2], ARGV[7], ARGV[2])
return 1
`

// RedisConfig controls the Redis-backed uptime store.
type RedisConfig struct {
	// Storage is the Fiber Redis storage instance used for uptime state.
	Storage *fiberredis.Storage
	// KeyPrefix namespaces all uptime keys inside the selected Redis database.
	KeyPrefix string
}

// RedisStore stores uptime state in Redis.
type RedisStore struct {
	config RedisConfig
	client redisClient
}

type redisClient interface {
	Ping(ctx context.Context) *redis.StatusCmd
	SAdd(ctx context.Context, key string, members ...interface{}) *redis.IntCmd
	SMembers(ctx context.Context, key string) *redis.StringSliceCmd
	SCard(ctx context.Context, key string) *redis.IntCmd
	HSet(ctx context.Context, key string, values ...interface{}) *redis.IntCmd
	HSetNX(ctx context.Context, key, field string, value interface{}) *redis.BoolCmd
	HGet(ctx context.Context, key, field string) *redis.StringCmd
	HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd
	ZAdd(ctx context.Context, key string, members ...redis.Z) *redis.IntCmd
	ZRangeByScore(ctx context.Context, key string, opt *redis.ZRangeBy) *redis.StringSliceCmd
	ZRem(ctx context.Context, key string, members ...interface{}) *redis.IntCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
	Pipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error)
}

// NewRedisStore creates a Redis-backed uptime store.
func NewRedisStore(config RedisConfig) *RedisStore {
	return &RedisStore{config: config}
}

func (s *RedisStore) Name() string {
	return "redis"
}

func (s *RedisStore) Init(ctx context.Context) (err error) {
	if s.client != nil {
		return s.client.Ping(ctx).Err()
	}

	if s.config.Storage == nil {
		return errors.New("redis uptime store: storage is required")
	}
	s.client = s.config.Storage.Conn()
	if s.client == nil {
		return errors.New("redis uptime store: client is required")
	}
	return s.client.Ping(ctx).Err()
}

func (s *RedisStore) UpsertService(ctx context.Context, service Service) error {
	if service.ID == "" {
		return errors.New("redis uptime store: service id is required")
	}

	serviceKey := s.serviceKey(service.ID)
	if err := s.client.SAdd(ctx, s.servicesKey(), service.ID).Err(); err != nil {
		return err
	}
	if service.CreatedAt.IsZero() {
		service.CreatedAt = service.LastSeenAt
	}
	if !service.CreatedAt.IsZero() {
		if err := s.client.HSetNX(ctx, serviceKey, "created_at", unixNano(service.CreatedAt)).Err(); err != nil {
			return err
		}
	}
	if err := s.client.HSet(ctx, serviceKey,
		"service_id", service.ID,
		"name", service.Name,
		"description", service.Description,
		"sample_interval_nanos", int64(service.SampleInterval),
	).Err(); err != nil {
		return err
	}
	return s.setMaxNano(ctx, serviceKey, "last_seen_at", unixNano(service.LastSeenAt))
}

func (s *RedisStore) UpsertInstance(ctx context.Context, instance Instance) error {
	if instance.ID == 0 {
		return errors.New("redis uptime store: instance id is required")
	}

	instanceKey := s.instanceKey(instance.ID)
	if instance.StartedAt.IsZero() {
		instance.StartedAt = instance.LastSeenAt
	}
	if !instance.StartedAt.IsZero() {
		if err := s.client.HSetNX(ctx, instanceKey, "started_at", unixNano(instance.StartedAt)).Err(); err != nil {
			return err
		}
	}
	if err := s.client.HSet(ctx, instanceKey,
		"instance_id", instance.ID,
		"service_id", instance.ServiceID,
		"hostname", instance.Hostname,
		"pid", instance.PID,
	).Err(); err != nil {
		return err
	}
	return s.setMaxNano(ctx, instanceKey, "last_seen_at", unixNano(instance.LastSeenAt))
}

func (s *RedisStore) WriteHeartbeat(ctx context.Context, heartbeat Heartbeat) error {
	if err := s.setMaxNano(ctx, s.serviceKey(heartbeat.ServiceID), "last_seen_at", unixNano(heartbeat.SeenAt)); err != nil {
		return err
	}
	if err := s.setMaxNano(ctx, s.instanceKey(heartbeat.InstanceID), "last_seen_at", unixNano(heartbeat.SeenAt)); err != nil {
		return err
	}
	if err := s.client.SAdd(ctx, s.sampleKey(heartbeat.ServiceID, heartbeat.Day), strconv.FormatInt(heartbeat.Slot, 10)).Err(); err != nil {
		return err
	}
	return s.addDay(ctx, s.sampleDaysKey(heartbeat.ServiceID), heartbeat.Day)
}

func (s *RedisStore) RollupDaily(ctx context.Context, options RollupOptions) error {
	if options.BeforeDay == "" {
		return nil
	}

	services, err := s.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, service := range services {
		days, err := s.daysBefore(ctx, s.sampleDaysKey(service.ID), options.BeforeDay)
		if err != nil {
			return err
		}
		for _, day := range days {
			upSlots64, err := s.client.SCard(ctx, s.sampleKey(service.ID, day)).Result()
			if err != nil {
				return err
			}
			expectedSlots := 0
			if options.ExpectedSlotsForServiceDay != nil {
				expectedSlots = options.ExpectedSlotsForServiceDay(service.ID, day)
			} else if options.ExpectedSlotsForDay != nil {
				expectedSlots = options.ExpectedSlotsForDay(day)
			}
			if err := s.writeDaily(ctx, DailyStatus{
				ServiceID:     service.ID,
				Day:           day,
				UpSlots:       int(upSlots64),
				ExpectedSlots: expectedSlots,
				UptimeRate:    rate(int(upSlots64), expectedSlots),
				Finalized:     true,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *RedisStore) Cleanup(ctx context.Context, options CleanupOptions) error {
	services, err := s.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, service := range services {
		if options.SamplesBeforeDay != "" {
			if err := s.cleanupSampleDays(ctx, service.ID, options.SamplesBeforeDay); err != nil {
				return err
			}
		}
		if options.DailyBeforeDay != "" {
			if err := s.cleanupDays(ctx, s.dailyDaysKey(service.ID), options.DailyBeforeDay, func(day string) string {
				return s.dailyKey(service.ID, day)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *RedisStore) ListServices(ctx context.Context) ([]Service, error) {
	serviceIDs, err := s.client.SMembers(ctx, s.servicesKey()).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(serviceIDs)
	if len(serviceIDs) == 0 {
		return nil, nil
	}

	cmds := make([]*redis.MapStringStringCmd, len(serviceIDs))
	if _, err := s.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, serviceID := range serviceIDs {
			cmds[i] = pipe.HGetAll(ctx, s.serviceKey(serviceID))
		}
		return nil
	}); err != nil {
		return nil, err
	}

	services := make([]Service, 0, len(serviceIDs))
	for i, serviceID := range serviceIDs {
		fields, err := cmds[i].Result()
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue
		}
		service, err := serviceFromRedisHash(serviceID, fields)
		if err != nil {
			return nil, err
		}
		services = append(services, service)
	}
	return services, nil
}

func (s *RedisStore) QueryDaily(ctx context.Context, options QueryDailyOptions) ([]DailyStatus, error) {
	services, err := s.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, nil
	}

	type dailyQuery struct {
		serviceID string
		day       string
	}
	queries := make([]dailyQuery, 0)

	dayCommands := make([]*redis.StringSliceCmd, len(services))
	if _, err := s.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, service := range services {
			cmd, err := s.daysBetweenPipe(ctx, pipe, s.dailyDaysKey(service.ID), options.FromDay, options.ToDay)
			if err != nil {
				return err
			}
			dayCommands[i] = cmd
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for i, service := range services {
		days, err := dayCommands[i].Result()
		if err != nil {
			return nil, err
		}
		for _, day := range days {
			queries = append(queries, dailyQuery{serviceID: service.ID, day: day})
		}
	}
	if len(queries) == 0 {
		return nil, nil
	}

	cmds := make([]*redis.MapStringStringCmd, len(queries))
	if _, err := s.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, query := range queries {
			cmds[i] = pipe.HGetAll(ctx, s.dailyKey(query.serviceID, query.day))
		}
		return nil
	}); err != nil {
		return nil, err
	}

	statuses := make([]DailyStatus, 0, len(queries))
	for i, query := range queries {
		fields, err := cmds[i].Result()
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue
		}
		status, err := dailyFromRedisHash(query.serviceID, query.day, fields)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (s *RedisStore) QueryTodaySamples(ctx context.Context, options QueryTodaySamplesOptions) ([]TodaySampleStatus, error) {
	if options.Day == "" {
		return nil, nil
	}

	services, err := s.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, nil
	}

	cmds := make([]*redis.IntCmd, len(services))
	if _, err := s.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, service := range services {
			cmds[i] = pipe.SCard(ctx, s.sampleKey(service.ID, options.Day))
		}
		return nil
	}); err != nil {
		return nil, err
	}

	statuses := make([]TodaySampleStatus, 0, len(services))
	for i, service := range services {
		upSlots, err := cmds[i].Result()
		if err != nil {
			return nil, err
		}
		if upSlots == 0 {
			continue
		}
		statuses = append(statuses, TodaySampleStatus{
			ServiceID: service.ID,
			Day:       options.Day,
			UpSlots:   int(upSlots),
		})
	}
	return statuses, nil
}

func (s *RedisStore) Close() error {
	// The Fiber Redis storage is supplied by the caller and owns the connection.
	return nil
}

func (s *RedisStore) setMaxNano(ctx context.Context, key, field string, value int64) error {
	if value == 0 {
		return nil
	}
	return s.client.Eval(ctx, setMaxNanoScript, []string{key}, field, value).Err()
}

func (s *RedisStore) writeDaily(ctx context.Context, status DailyStatus) error {
	finalized := 0
	if status.Finalized {
		finalized = 1
	}
	score, err := redisDayScore(status.Day)
	if err != nil {
		return err
	}
	return s.client.Eval(ctx, writeDailyIfUnfinalizedScript,
		[]string{s.dailyKey(status.ServiceID, status.Day), s.dailyDaysKey(status.ServiceID)},
		status.ServiceID,
		status.Day,
		status.UpSlots,
		status.ExpectedSlots,
		strconv.FormatFloat(status.UptimeRate, 'f', -1, 64),
		finalized,
		score,
	).Err()
}

func (s *RedisStore) addDay(ctx context.Context, key, day string) error {
	score, err := redisDayScore(day)
	if err != nil {
		return err
	}
	return s.client.ZAdd(ctx, key, redis.Z{Score: float64(score), Member: day}).Err()
}

func (s *RedisStore) daysBefore(ctx context.Context, key, beforeDay string) ([]string, error) {
	score, err := redisDayScore(beforeDay)
	if err != nil {
		return nil, err
	}
	return s.client.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: "-inf",
		Max: "(" + strconv.FormatInt(score, 10),
	}).Result()
}

func (s *RedisStore) daysBetweenPipe(ctx context.Context, pipe redis.Pipeliner, key, fromDay, toDay string) (*redis.StringSliceCmd, error) {
	min := "-inf"
	if fromDay != "" {
		score, err := redisDayScore(fromDay)
		if err != nil {
			return nil, err
		}
		min = strconv.FormatInt(score, 10)
	}
	max := "+inf"
	if toDay != "" {
		score, err := redisDayScore(toDay)
		if err != nil {
			return nil, err
		}
		max = strconv.FormatInt(score, 10)
	}
	if pipe == nil {
		return s.client.ZRangeByScore(ctx, key, &redis.ZRangeBy{
			Min: min,
			Max: max,
		}), nil
	}
	return pipe.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: min,
		Max: max,
	}), nil
}

func (s *RedisStore) cleanupDays(ctx context.Context, zsetKey, beforeDay string, keyForDay func(string) string) error {
	days, err := s.daysBefore(ctx, zsetKey, beforeDay)
	if err != nil {
		return err
	}
	for _, day := range days {
		if err := s.client.Del(ctx, keyForDay(day)).Err(); err != nil {
			return err
		}
		if err := s.client.ZRem(ctx, zsetKey, day).Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *RedisStore) cleanupSampleDays(ctx context.Context, serviceID, beforeDay string) error {
	days, err := s.daysBefore(ctx, s.sampleDaysKey(serviceID), beforeDay)
	if err != nil {
		return err
	}
	for _, day := range days {
		finalized, err := s.dailyFinalized(ctx, serviceID, day)
		if err != nil {
			return err
		}
		if !finalized {
			continue
		}
		if err := s.client.Del(ctx, s.sampleKey(serviceID, day)).Err(); err != nil {
			return err
		}
		if err := s.client.ZRem(ctx, s.sampleDaysKey(serviceID), day).Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *RedisStore) dailyFinalized(ctx context.Context, serviceID, day string) (bool, error) {
	raw, err := s.client.HGet(ctx, s.dailyKey(serviceID, day), "finalized").Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return raw != "" && raw != "0", nil
}

func (s *RedisStore) servicesKey() string {
	return s.key("services")
}

func (s *RedisStore) serviceKey(serviceID string) string {
	return s.key("service", serviceID)
}

func (s *RedisStore) instanceKey(instanceID int64) string {
	return s.key("instance", strconv.FormatInt(instanceID, 10))
}

func (s *RedisStore) sampleKey(serviceID, day string) string {
	return s.key("samples", serviceID, day)
}

func (s *RedisStore) sampleDaysKey(serviceID string) string {
	return s.key("sample-days", serviceID)
}

func (s *RedisStore) dailyKey(serviceID, day string) string {
	return s.key("daily", serviceID, day)
}

func (s *RedisStore) dailyDaysKey(serviceID string) string {
	return s.key("daily-days", serviceID)
}

func (s *RedisStore) key(parts ...string) string {
	prefix := strings.Trim(s.config.KeyPrefix, ":")
	if prefix == "" {
		prefix = defaultRedisKeyPrefix
	}
	if len(parts) == 0 {
		return prefix
	}
	return prefix + ":" + strings.Join(parts, ":")
}

func serviceFromRedisHash(serviceID string, fields map[string]string) (Service, error) {
	createdAt, err := int64Field(fields, "created_at")
	if err != nil {
		return Service{}, err
	}
	lastSeenAt, err := int64Field(fields, "last_seen_at")
	if err != nil {
		return Service{}, err
	}
	intervalNanos, err := int64Field(fields, "sample_interval_nanos")
	if err != nil {
		return Service{}, err
	}
	name := fields["name"]
	if name == "" {
		name = serviceID
	}
	return Service{
		ID:             serviceID,
		Name:           name,
		Description:    fields["description"],
		CreatedAt:      fromUnixNano(createdAt),
		LastSeenAt:     fromUnixNano(lastSeenAt),
		SampleInterval: time.Duration(intervalNanos),
	}, nil
}

func dailyFromRedisHash(serviceID, day string, fields map[string]string) (DailyStatus, error) {
	upSlots, err := intField(fields, "up_slots")
	if err != nil {
		return DailyStatus{}, err
	}
	expectedSlots, err := intField(fields, "expected_slots")
	if err != nil {
		return DailyStatus{}, err
	}
	finalized, err := intField(fields, "finalized")
	if err != nil {
		return DailyStatus{}, err
	}
	uptimeRate, err := floatField(fields, "uptime_rate")
	if err != nil {
		return DailyStatus{}, err
	}
	return DailyStatus{
		ServiceID:     serviceID,
		Day:           day,
		UpSlots:       upSlots,
		ExpectedSlots: expectedSlots,
		UptimeRate:    uptimeRate,
		Finalized:     finalized != 0,
	}, nil
}

func intField(fields map[string]string, name string) (int, error) {
	value, err := int64Field(fields, name)
	return int(value), err
}

func int64Field(fields map[string]string, name string) (int64, error) {
	raw := fields[name]
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("redis uptime store: parse %s: %w", name, err)
	}
	return value, nil
}

func floatField(fields map[string]string, name string) (float64, error) {
	raw := fields[name]
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("redis uptime store: parse %s: %w", name, err)
	}
	return value, nil
}

func redisDayScore(day string) (int64, error) {
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil {
		return 0, fmt.Errorf("redis uptime store: parse day %q: %w", day, err)
	}
	return parsed.Unix() / int64((24 * time.Hour / time.Second)), nil
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func fromUnixNano(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

func rate(upSlots, expectedSlots int) float64 {
	if expectedSlots <= 0 {
		return 0
	}
	value := float64(upSlots) / float64(expectedSlots)
	if value > 1 {
		return 1
	}
	if value < 0 || math.IsNaN(value) {
		return 0
	}
	return value
}

package storage

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is a Redis storage implementation for rate limiter
type Redis struct {
	client redis.Cmdable
	prefix string
}

// NewRedis creates a new Redis storage with the given client and key prefix
func NewRedis(client redis.Cmdable, prefix string) *Redis {
	if prefix == "" {
		prefix = "ratelimit:"
	}
	return &Redis{
		client: client,
		prefix: prefix,
	}
}

// Get retrieves the count for the given key
func (r *Redis) Get(ctx context.Context, key string) (int, error) {
	val, err := r.client.Get(ctx, r.prefix+key).Result()
	if err != nil {
		if err == redis.Nil {
			return 0, nil
		}
		return 0, err
	}
	
	count, err := strconv.Atoi(val)
	if err != nil {
		return 0, err
	}
	
	return count, nil
}

// Increment increments the count for the given key and sets expiration
func (r *Redis) Increment(ctx context.Context, key string, expiration time.Duration) (int, bool, error) {
	redisKey := r.prefix + key
	
	// Use Lua script for atomic increment and set expiration
	luaScript := `
		local current = redis.call("GET", KEYS[1])
		if current == false then
			redis.call("SET", KEYS[1], 1)
			redis.call("EXPIRE", KEYS[1], ARGV[1])
			return {1, 1}
		else
			local count = redis.call("INCR", KEYS[1])
			return {count, 0}
		end
	`
	
	expireSeconds := int(expiration.Seconds())
	result, err := r.client.Eval(ctx, luaScript, []string{redisKey}, expireSeconds).Result()
	if err != nil {
		return 0, false, err
	}
	
	resultSlice := result.([]interface{})
	count := int(resultSlice[0].(int64))
	isNew := resultSlice[1].(int64) == 1
	
	return count, isNew, nil
}

// Reset resets the count for the given key
func (r *Redis) Reset(ctx context.Context, key string) error {
	return r.client.Del(ctx, r.prefix+key).Err()
}

// Close closes the Redis connection if it's a *redis.Client
func (r *Redis) Close() error {
	if client, ok := r.client.(*redis.Client); ok {
		return client.Close()
	}
	return nil
}
package otel

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOptionCacheIsBounded(t *testing.T) {
	t.Parallel()

	var cache optionCache[int, string]
	for i := range maxCachedSets + 10 {
		cache.store(i, "set")
	}
	require.Equal(t, int32(maxCachedSets), cache.size.Load())

	value, ok := cache.load(0)
	require.True(t, ok)
	require.Equal(t, "set", value)

	_, ok = cache.load(maxCachedSets + 5)
	require.False(t, ok, "a key past the bound is built per request instead")
}

// A key is stored once: a racing store neither replaces it nor counts twice.
func TestOptionCacheKeepsFirstValue(t *testing.T) {
	t.Parallel()

	var cache optionCache[string, int]

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			cache.store("GET /users/:id 200", i)
			_, ok := cache.load("GET /users/:id 200")
			assert.True(t, ok)
		})
	}
	wg.Wait()

	require.Equal(t, int32(1), cache.size.Load())
}

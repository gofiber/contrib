package uptime_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	"github.com/gofiber/fiber/v3"
)

func TestProbeResultConcurrentEndpoints(t *testing.T) {
	t.Parallel()
	var requestMu sync.Mutex
	requests := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requests[r.URL.Path]++
		count := requests[r.URL.Path]
		requestMu.Unlock()
		if count == 1 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)

	var mu sync.Mutex
	calls := make(map[string]int)
	active := make(map[string]int)
	overlapped := false
	entered := make(chan string, 2)
	results := make(chan uptime.ProbeResult, 64)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	store := newCustomStore()
	app := fiber.New()
	t.Cleanup(func() {
		unblock()
		if err := app.Shutdown(); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	app.Use(uptime.New(uptime.Config{
		App: app, Storage: store,
		Endpoints: []uptime.EndpointConfig{
			{ID: "first", URL: server.URL + "/first", Interval: time.Second},
			{ID: "second", URL: server.URL + "/second", Interval: time.Second},
		},
		OnProbeResult: func(result uptime.ProbeResult) {
			mu.Lock()
			calls[result.EndpointID]++
			first := calls[result.EndpointID] == 1
			active[result.EndpointID]++
			overlapped = overlapped || active[result.EndpointID] > 1
			mu.Unlock()
			if first {
				entered <- result.EndpointID
				<-release
			}
			results <- result
			mu.Lock()
			active[result.EndpointID]--
			mu.Unlock()
		},
	}))

	// Both initial callbacks must enter before Listen or either callback returns.
	initial := make(map[string]bool)
	for range 2 {
		select {
		case id := <-entered:
			initial[id] = true
			if _, slots := store.serviceState(id); slots != 0 {
				t.Fatalf("%s heartbeat was stored before its callback returned", id)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("initial endpoint callbacks did not run concurrently")
		}
	}
	if len(initial) != 2 {
		t.Fatalf("initial callbacks = %v, want both endpoints", initial)
	}
	unblock()

	seen := make(map[string][]uptime.ProbeResult)
	timeout := time.After(5 * time.Second)
	for len(seen["first"]) < 2 || len(seen["second"]) < 2 {
		select {
		case result := <-results:
			seen[result.EndpointID] = append(seen[result.EndpointID], result)
		case <-timeout:
			t.Fatal("missing subsequent endpoint results")
		}
	}
	if err := app.Shutdown(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		got := seen[id]
		if !got[0].Success || got[0].StatusCode != http.StatusOK ||
			got[1].Success || got[1].StatusCode != http.StatusServiceUnavailable ||
			!got[0].CheckedAt.Before(got[1].CheckedAt) {
			t.Fatalf("%s results out of order: %+v", id, got)
		}
		if _, slots := store.serviceState(id); slots != 1 {
			t.Fatalf("%s heartbeat slots = %d, want 1", id, slots)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if overlapped {
		t.Fatal("callbacks for the same endpoint overlapped")
	}
}

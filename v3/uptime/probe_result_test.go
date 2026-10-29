package uptime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeResultHTTP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		status   int
		expected []int
		success  bool
		tls      bool
	}{
		{"OK", http.StatusOK, nil, true, false},
		{"HTTPS", http.StatusOK, nil, true, true},
		{"expected status", http.StatusNoContent, []int{http.StatusNoContent}, true, false},
		{"unexpected status", http.StatusServiceUnavailable, nil, false, false},
		{"default redirect", http.StatusFound, nil, true, false},
		{"unexpected redirect", http.StatusFound, []int{http.StatusOK}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "/must-not-be-followed")
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			var results []ProbeResult
			u := &runtime{config: Config{OnProbeResult: func(result ProbeResult) {
				results = append(results, result)
			}}}
			if tc.tls {
				server.StartTLS()
				u.httpClient = server.Client()
			} else {
				server.Start()
			}
			probe := newEndpointProbe(EndpointConfig{
				ID: "health", URL: server.URL, Timeout: time.Second,
				ExpectedStatusCodes: tc.expected,
			})
			started := time.Now()
			requireEqual(t, tc.success, u.probeEndpoint(context.Background(), probe))
			requireEqual(t, 1, len(results))
			result := results[0]
			requireEqual(t, "health", result.EndpointID)
			requireEqual(t, tc.success, result.Success)
			requireEqual(t, tc.status, result.StatusCode)
			if result.CheckedAt.Before(started) || result.CheckedAt.After(time.Now()) || result.Duration < 0 {
				t.Fatalf("invalid completion time or duration: %+v", result)
			}
		})
	}
}

func TestProbeResultRequestFailures(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsServer.Close)
	for _, tc := range []struct {
		name   string
		url    string
		method string
		client *http.Client
	}{
		{name: "request construction", url: "http://example.invalid", method: "invalid method"},
		{name: "connection", url: closed.URL},
		{name: "TLS", url: tlsServer.URL},
		{
			name: "DNS", url: "http://example.invalid",
			client: &http.Client{Transport: &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, &net.DNSError{Err: "test DNS failure", Name: "example.invalid"}
				},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results []ProbeResult
			u := &runtime{
				config:     Config{OnProbeResult: func(result ProbeResult) { results = append(results, result) }},
				httpClient: tc.client,
			}
			probe := newEndpointProbe(EndpointConfig{ID: "health", URL: tc.url, Method: tc.method, Timeout: time.Second})
			requireEqual(t, false, u.probeEndpoint(context.Background(), probe))
			requireEqual(t, 1, len(results))
			requireEqual(t, "health", results[0].EndpointID)
			requireEqual(t, false, results[0].Success)
			requireEqual(t, 0, results[0].StatusCode)
		})
	}
}

func TestProbeResultTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelParent := range []bool{false, true} {
		name := "probe timeout"
		if cancelParent {
			name = "shutdown cancellation"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			var results []ProbeResult
			u := &runtime{config: Config{OnProbeResult: func(result ProbeResult) { results = append(results, result) }}}
			timeout := 250 * time.Millisecond
			if cancelParent {
				timeout = 5 * time.Second
			}
			probe := newEndpointProbe(EndpointConfig{ID: "health", URL: server.URL, Timeout: timeout})
			done := make(chan bool, 1)
			go func() { done <- u.probeEndpoint(ctx, probe) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not start")
			}
			if cancelParent {
				cancel()
			}
			select {
			case success := <-done:
				requireEqual(t, false, success)
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not stop")
			}
			if cancelParent {
				requireEqual(t, 0, len(results))
			} else {
				requireEqual(t, 1, len(results))
				requireEqual(t, false, results[0].Success)
				requireEqual(t, 0, results[0].StatusCode)
			}
		})
	}
}

func TestProbeResultStorageOrdering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		status       int
		initFails    bool
		writeFails   bool
		refreshFails bool
		panicHook    bool
		nilHook      bool
	}{
		{name: "success", status: http.StatusOK},
		{name: "unexpected status", status: http.StatusServiceUnavailable},
		{name: "initialization failure", status: http.StatusOK, initFails: true},
		{name: "heartbeat failure", status: http.StatusOK, writeFails: true},
		{name: "service refresh failure", status: http.StatusServiceUnavailable, refreshFails: true},
		{name: "callback panic", status: http.StatusOK, panicHook: true},
		{name: "nil hook success", status: http.StatusOK, nilHook: true},
		{name: "nil hook failure", status: http.StatusServiceUnavailable, nilHook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			store := newSnapshotStore()
			store.services, store.today = nil, nil
			cfg := normalizedTestConfig(t, Config{Endpoints: []EndpointConfig{{ID: "health", URL: server.URL}}})
			target := buildRecordTargets(cfg, store.now, 7)[0]
			var results []ProbeResult
			var panicFormatted atomic.Bool
			if !tc.nilHook {
				cfg.OnProbeResult = func(result ProbeResult) {
					results = append(results, result)
					// Initial registration is allowed; no post-probe writes may precede the hook.
					requireEqual(t, 1, store.upsertServiceCalls)
					requireEqual(t, 0, len(store.today))
					if tc.panicHook {
						panic(probeResultPanicValue{formatted: &panicFormatted})
					}
				}
			}
			u := &runtime{config: cfg, store: store}
			failure := errors.New("test storage failure")
			if tc.initFails {
				store.initErr = failure
			} else {
				requireNoError(t, u.ensureTargetRegistered(context.Background(), target))
			}
			if tc.writeFails {
				store.writeHeartbeatErr = failure
			}
			if tc.refreshFails {
				store.upsertServiceErr = failure
			}
			err := u.recordTarget(context.Background(), target, store.now)
			if tc.initFails || tc.writeFails || tc.refreshFails {
				if !errors.Is(err, failure) {
					t.Fatalf("record error = %v, want storage failure", err)
				}
			} else {
				requireNoError(t, err)
			}
			wantEvents := 1
			if tc.initFails || tc.nilHook {
				wantEvents = 0
			}
			requireEqual(t, wantEvents, len(results))
			if len(results) == 1 {
				requireEqual(t, tc.status == http.StatusOK, results[0].Success)
				requireEqual(t, tc.status, results[0].StatusCode)
			}
			wantRequests := int32(1)
			if tc.initFails {
				wantRequests = 0
			}
			requireEqual(t, wantRequests, requests.Load())
			wantHeartbeats := 0
			if tc.status == http.StatusOK && !tc.initFails && !tc.writeFails {
				wantHeartbeats = 1
			}
			requireEqual(t, wantHeartbeats, len(store.today))
			requireEqual(t, false, panicFormatted.Load())
		})
	}
}

type probeResultPanicValue struct {
	formatted *atomic.Bool
}

func (p probeResultPanicValue) String() string {
	p.formatted.Store(true)
	return "sensitive panic value"
}

func TestProbeResultDurationExcludesCallback(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	var result ProbeResult
	var callbackDuration time.Duration
	u := &runtime{config: Config{OnProbeResult: func(r ProbeResult) {
		result = r
		started := time.Now()
		time.Sleep(25 * time.Millisecond)
		callbackDuration = time.Since(started)
	}}}
	probe := newEndpointProbe(EndpointConfig{ID: "health", URL: server.URL, Timeout: time.Second})
	started := time.Now()
	requireEqual(t, true, u.probeEndpoint(context.Background(), probe))
	if elapsed := time.Since(started); elapsed < result.Duration+callbackDuration {
		t.Fatalf("probe duration %s includes callback time %s (elapsed %s)", result.Duration, callbackDuration, elapsed)
	}
	if time.Since(result.CheckedAt) < callbackDuration {
		t.Fatal("CheckedAt includes callback time")
	}
}

func TestProbeResultIgnoresProcessHeartbeat(t *testing.T) {
	t.Parallel()
	store := newSnapshotStore()
	calls := 0
	cfg := normalizedTestConfig(t, Config{ServiceID: "process", OnProbeResult: func(ProbeResult) { calls++ }})
	u := &runtime{config: cfg, store: store}
	target := buildRecordTargets(cfg, store.now, 7)[0]
	requireNoError(t, u.recordTarget(context.Background(), target, store.now))
	requireEqual(t, 0, calls)
}

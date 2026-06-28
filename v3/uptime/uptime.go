package uptime

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/contrib/v3/uptime/internal/storage"
	"github.com/gofiber/fiber/v3"
	fiberlog "github.com/gofiber/fiber/v3/log"
)

// IDGenerator generates instance IDs.
type IDGenerator interface {
	NextID() int64
}

// Uptime records service heartbeats and serves uptime history.
type Uptime struct {
	config Config
	store  storage.Store

	service  storage.Service
	instance storage.Instance
	targets  []recordTarget

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	httpClient *http.Client

	closeOnce sync.Once

	errMu     sync.RWMutex
	lastErr   error
	lastErrAt time.Time

	maintenanceMu      sync.Mutex
	lastMaintenance    time.Time
	lastMaintenanceDay string
}

type recordTarget struct {
	service  storage.Service
	instance storage.Instance
	interval time.Duration
	probe    *endpointProbe
}

type endpointProbe struct {
	method              string
	url                 string
	headers             map[string]string
	timeout             time.Duration
	expectedStatusCodes map[int]struct{}
}

const (
	headerCacheControl        = "Cache-Control"
	headerXContentTypeOptions = "X-Content-Type-Options"
	headerAllow               = "Allow"
)

// New creates a Fiber handler that records uptime and serves the dashboard/API.
func New(config ...Config) fiber.Handler {
	u, err := NewRuntime(config...)
	if err != nil {
		panic(fmt.Errorf("fiber: uptime middleware error -> %w", err))
	}
	return u.Handler()
}

// NewRuntime initializes the store and starts recording uptime.
func NewRuntime(config ...Config) (*Uptime, error) {
	cfg, err := configDefault(config...).normalized()
	if err != nil {
		return nil, err
	}

	store := storage.NewRedisStore(storage.RedisConfig{
		Config:    cfg.Redis,
		KeyPrefix: cfg.StorageKeyPrefix,
	})
	return newWithStore(cfg, store, time.Now())
}

func newWithStore(cfg Config, store storage.Store, now time.Time) (*Uptime, error) {
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("uptime: init store: %w", err)
	}

	instanceID := cfg.InstanceID
	if instanceID == 0 && cfg.IDGenerator != nil {
		instanceID = cfg.IDGenerator.NextID()
	}
	if instanceID == 0 {
		instanceID = instanceIDForNode(cfg.NodeID)
	}

	targets := buildRecordTargets(cfg, now, instanceID)
	for _, target := range targets {
		if err := store.UpsertService(ctx, target.service); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("uptime: upsert service: %w", err)
		}
		if err := store.UpsertInstance(ctx, target.instance); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("uptime: upsert instance: %w", err)
		}
	}

	runCtx, cancel := context.WithCancel(context.Background())
	u := &Uptime{
		config:     cfg,
		store:      store,
		service:    targets[0].service,
		instance:   targets[0].instance,
		targets:    targets,
		ctx:        runCtx,
		cancel:     cancel,
		done:       make(chan struct{}),
		httpClient: &http.Client{},
	}

	if err := u.runMaintenance(ctx, now, true); err != nil {
		cancel()
		_ = store.Close()
		return nil, fmt.Errorf("uptime: maintenance: %w", err)
	}
	for _, target := range targets {
		if target.probe != nil {
			continue
		}
		if err := u.recordTarget(ctx, target, now); err != nil {
			cancel()
			_ = store.Close()
			return nil, fmt.Errorf("uptime: initial heartbeat: %w", err)
		}
	}

	go u.loop()
	registerShutdownHook(cfg.App, u)
	return u, nil
}

func buildRecordTargets(cfg Config, now time.Time, firstInstanceID int64) []recordTarget {
	hostname, _ := os.Hostname()
	targets := make([]recordTarget, 0, len(cfg.Endpoints)+1)
	nextInstanceID := func(index int) int64 {
		if index == 0 && firstInstanceID != 0 {
			return firstInstanceID
		}
		if cfg.InstanceID != 0 {
			return cfg.InstanceID + int64(index)
		}
		if cfg.IDGenerator != nil {
			if id := cfg.IDGenerator.NextID(); id != 0 {
				return id
			}
		}
		return instanceIDForNode(cfg.NodeID + int64(index))
	}
	addTarget := func(index int, service storage.Service, probe *endpointProbe) {
		instance := storage.Instance{
			ID:         nextInstanceID(index),
			ServiceID:  service.ID,
			Hostname:   hostname,
			PID:        os.Getpid(),
			StartedAt:  now,
			LastSeenAt: service.LastSeenAt,
		}
		targets = append(targets, recordTarget{
			service:  service,
			instance: instance,
			interval: service.SampleInterval,
			probe:    probe,
		})
	}

	index := 0
	if cfg.ServiceID != "" {
		addTarget(index, storage.Service{
			ID:             cfg.ServiceID,
			Name:           cfg.ServiceName,
			Description:    cfg.ServiceDescription,
			CreatedAt:      now,
			LastSeenAt:     now,
			SampleInterval: cfg.SampleInterval,
		}, nil)
		index++
	}
	for _, endpoint := range cfg.Endpoints {
		addTarget(index, storage.Service{
			ID:             endpoint.ID,
			Name:           endpoint.Name,
			Description:    endpoint.Description,
			CreatedAt:      now,
			SampleInterval: endpoint.Interval,
		}, newEndpointProbe(endpoint))
		index++
	}
	return targets
}

func newEndpointProbe(endpoint EndpointConfig) *endpointProbe {
	expectedStatusCodes := make(map[int]struct{}, len(endpoint.ExpectedStatusCodes))
	for _, statusCode := range endpoint.ExpectedStatusCodes {
		expectedStatusCodes[statusCode] = struct{}{}
	}
	return &endpointProbe{
		method:              endpoint.Method,
		url:                 endpoint.URL,
		headers:             copyStringMap(endpoint.Headers),
		timeout:             endpoint.Timeout,
		expectedStatusCodes: expectedStatusCodes,
	}
}

func registerShutdownHook(app *fiber.App, u *Uptime) {
	if app == nil || u == nil {
		return
	}
	app.Hooks().OnPreShutdown(u.Close)
}

// Close stops background work and closes the store.
func (u *Uptime) Close() error {
	if u == nil {
		return nil
	}
	var err error
	u.closeOnce.Do(func() {
		if u.cancel != nil {
			u.cancel()
			if u.done != nil {
				<-u.done
			}
		}
		if u.store != nil {
			err = u.store.Close()
		}
	})
	return err
}

// LastError returns the most recent runtime store error, if any.
func (u *Uptime) LastError() (time.Time, error) {
	if u == nil {
		return time.Time{}, nil
	}
	u.errMu.RLock()
	defer u.errMu.RUnlock()
	return u.lastErrAt, u.lastErr
}

// Handler returns a Fiber-native handler for the dashboard and JSON API.
func (u *Uptime) Handler() fiber.Handler {
	return func(c fiber.Ctx) error {
		if u == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "uptime unavailable")
		}
		if u.config.Next != nil && u.config.Next(c) {
			return c.Next()
		}

		path := c.Path()
		uiPath := u.config.UI.Path
		route := ""
		switch path {
		case uiPath, uiPath + "/":
			route = "dashboard"
		case uiPath + "/api/status":
			route = "status"
		default:
			if strings.HasPrefix(path, uiPath+"/") {
				return fiber.ErrNotFound
			}
			return c.Next()
		}

		method := c.Method()
		if method != fiber.MethodGet && method != fiber.MethodHead {
			c.Set(headerAllow, "GET, HEAD")
			return fiber.ErrMethodNotAllowed
		}

		switch route {
		case "dashboard":
			return u.serveDashboard(c)
		case "status":
			return u.serveStatusJSON(c)
		default:
			return fiber.ErrNotFound
		}
	}
}

func (u *Uptime) serveStatusJSON(c fiber.Ctx) error {
	c.Set(headerCacheControl, "no-store")
	c.Set(headerXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)

	status, err := u.Snapshot(c.Context())
	if err != nil {
		fiberlog.Errorf("uptime: status unavailable: %v", err)
		return fiber.NewError(fiber.StatusInternalServerError, "uptime status unavailable")
	}
	if c.Method() == fiber.MethodHead {
		c.Status(fiber.StatusOK)
		return nil
	}
	return c.Status(fiber.StatusOK).JSON(&status)
}

func (u *Uptime) serveDashboard(c fiber.Ctx) error {
	c.Set(headerCacheControl, "no-store")
	c.Set(headerXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)

	status, err := u.Snapshot(c.Context())
	if err != nil {
		fiberlog.Errorf("uptime: dashboard unavailable: %v", err)
		return fiber.NewError(fiber.StatusInternalServerError, "uptime dashboard unavailable")
	}
	html, err := renderDashboardHTML(u.config, status)
	if err != nil {
		fiberlog.Errorf("uptime: dashboard render failed: %v", err)
		return fiber.NewError(fiber.StatusInternalServerError, "uptime dashboard render failed")
	}
	if c.Method() == fiber.MethodHead {
		c.Status(fiber.StatusOK)
		return nil
	}
	return c.Status(fiber.StatusOK).SendString(html)
}

func (u *Uptime) loop() {
	defer close(u.done)

	var wg sync.WaitGroup
	for _, target := range u.targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.recordLoop(target)
		}()
	}

	<-u.ctx.Done()
	wg.Wait()
}

func (u *Uptime) recordLoop(target recordTarget) {
	if target.probe != nil {
		_ = u.recordTarget(u.ctx, target, time.Now())
	}

	ticker := time.NewTicker(target.interval)
	defer ticker.Stop()

	for {
		select {
		case <-u.ctx.Done():
			return
		case now := <-ticker.C:
			_ = u.recordTarget(u.ctx, target, now)
		}
	}
}

func (u *Uptime) writeHeartbeat(ctx context.Context, now time.Time) error {
	if len(u.targets) == 0 {
		return nil
	}
	return u.recordTarget(ctx, u.targets[0], now)
}

func (u *Uptime) recordTarget(ctx context.Context, target recordTarget, now time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if target.probe != nil && !u.probeEndpoint(ctx, target.probe) {
		if err := u.runMaintenance(ctx, now, false); err != nil {
			return err
		}
		u.clearLastError()
		return nil
	}
	if err := u.writeTargetHeartbeat(ctx, target, now); err != nil {
		return err
	}
	return u.runMaintenance(ctx, now, false)
}

func (u *Uptime) writeTargetHeartbeat(ctx context.Context, target recordTarget, now time.Time) error {
	heartbeat := storage.Heartbeat{
		ServiceID:  target.service.ID,
		InstanceID: target.instance.ID,
		Day:        dayOf(now, u.config.Timezone),
		Slot:       slotOf(now, target.interval, u.config.Timezone),
		SeenAt:     now,
	}
	if err := u.store.WriteHeartbeat(ctx, heartbeat); err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: write heartbeat: %v", err)
		return err
	}
	u.clearLastError()
	return nil
}

func (u *Uptime) probeEndpoint(ctx context.Context, probe *endpointProbe) bool {
	probeCtx, cancel := context.WithTimeout(ctx, probe.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, probe.method, probe.url, nil)
	if err != nil {
		return false
	}
	for key, value := range probe.headers {
		req.Header.Set(key, value)
	}

	client := u.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	statusCode := resp.StatusCode
	_ = resp.Body.Close()
	return probe.statusAllowed(statusCode)
}

func (p *endpointProbe) statusAllowed(statusCode int) bool {
	if len(p.expectedStatusCodes) == 0 {
		return statusCode >= 200 && statusCode < 400
	}
	_, ok := p.expectedStatusCodes[statusCode]
	return ok
}

func (u *Uptime) runMaintenance(ctx context.Context, now time.Time, force bool) error {
	today := dayOf(now, u.config.Timezone)

	u.maintenanceMu.Lock()
	defer u.maintenanceMu.Unlock()

	if !force && u.lastMaintenanceDay == today && now.Sub(u.lastMaintenance) < maintenanceInterval {
		return nil
	}

	expected := func(day string) int {
		return expectedSlotsForDay(day, u.config.SampleInterval, u.config.Timezone)
	}
	services, err := u.serviceRollupMetadata(ctx)
	if err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: list services for maintenance: %v", err)
		return err
	}
	expectedForService := func(serviceID, day string) int {
		service, ok := services[serviceID]
		if !ok {
			return expected(day)
		}
		return expectedSlotsForServiceDay(day, service.createdAt, service.interval, u.config.Timezone)
	}
	if err := u.store.RollupDaily(ctx, storage.RollupOptions{
		BeforeDay:                  today,
		ExpectedSlotsForDay:        expected,
		ExpectedSlotsForServiceDay: expectedForService,
	}); err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: rollup daily: %v", err)
		return err
	}

	dailyBefore := addDays(today, -u.config.RetentionDays, u.config.Timezone)
	samplesBefore := addDays(today, -1, u.config.Timezone)
	if err := u.store.Cleanup(ctx, storage.CleanupOptions{
		DailyBeforeDay:   dailyBefore,
		SamplesBeforeDay: samplesBefore,
	}); err != nil {
		u.setLastError(err)
		fiberlog.Errorf("uptime: cleanup: %v", err)
		return err
	}

	u.lastMaintenance = now
	u.lastMaintenanceDay = today
	return nil
}

type serviceRollupMetadata struct {
	createdAt time.Time
	interval  time.Duration
}

func (u *Uptime) serviceRollupMetadata(ctx context.Context) (map[string]serviceRollupMetadata, error) {
	services, err := u.store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	metadata := make(map[string]serviceRollupMetadata, len(services))
	for _, service := range services {
		metadata[service.ID] = serviceRollupMetadata{
			createdAt: service.CreatedAt,
			interval:  u.serviceSampleInterval(service),
		}
	}
	return metadata, nil
}

func (u *Uptime) setLastError(err error) {
	if err == nil {
		return
	}
	u.errMu.Lock()
	u.lastErr = err
	u.lastErrAt = time.Now()
	u.errMu.Unlock()
}

func (u *Uptime) clearLastError() {
	u.errMu.Lock()
	u.lastErr = nil
	u.lastErrAt = time.Time{}
	u.errMu.Unlock()
}

func instanceIDForNode(nodeID int64) int64 {
	host, _ := os.Hostname()
	h := fnv.New64a()
	_, _ = h.Write([]byte(host))
	var buf [32]byte
	binary.LittleEndian.PutUint64(buf[0:8], uint64(time.Now().UnixNano()))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(os.Getpid()))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(nodeID))
	if _, err := rand.Read(buf[24:32]); err != nil {
		binary.LittleEndian.PutUint64(buf[24:32], uint64(time.Now().UnixNano()<<7))
	}
	_, _ = h.Write(buf[:])
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

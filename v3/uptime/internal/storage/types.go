package storage

import (
	"context"
	"time"
)

// Store persists uptime state. Implementations must be safe for concurrent use.
type Store interface {
	Init(ctx context.Context) error

	UpsertService(ctx context.Context, service Service) error
	UpsertInstance(ctx context.Context, instance Instance) error

	WriteHeartbeat(ctx context.Context, heartbeat Heartbeat) error

	RollupDaily(ctx context.Context, options RollupOptions) error
	Cleanup(ctx context.Context, options CleanupOptions) error

	ListServices(ctx context.Context) ([]Service, error)
	QueryDaily(ctx context.Context, options QueryDailyOptions) ([]DailyStatus, error)
	QueryTodaySamples(ctx context.Context, options QueryTodaySamplesOptions) ([]TodaySampleStatus, error)

	Close() error
}

// Service is the logical service identity shown on the dashboard.
type Service struct {
	ID             string
	Name           string
	Description    string
	CreatedAt      time.Time
	LastSeenAt     time.Time
	SampleInterval time.Duration
}

// Instance describes one process lifetime of a service.
type Instance struct {
	ID         int64
	ServiceID  string
	Hostname   string
	PID        int
	StartedAt  time.Time
	LastSeenAt time.Time
}

// Heartbeat records that one instance was alive during a day slot.
type Heartbeat struct {
	ServiceID  string
	InstanceID int64
	Day        string
	Slot       int64
	SeenAt     time.Time
}

// DailyStatus is a finalized service-level day snapshot.
type DailyStatus struct {
	ServiceID     string
	Day           string
	UpSlots       int
	ExpectedSlots int
	UptimeRate    float64
	Finalized     bool
}

// TodaySampleStatus is the current raw service-level summary for one day.
type TodaySampleStatus struct {
	ServiceID string
	Day       string
	UpSlots   int
}

type RollupOptions struct {
	BeforeDay                  string
	ExpectedSlotsForDay        func(day string) int
	ExpectedSlotsForServiceDay func(serviceID, day string) int
}

type CleanupOptions struct {
	DailyBeforeDay   string
	SamplesBeforeDay string
}

type QueryDailyOptions struct {
	FromDay string
	ToDay   string
}

type QueryTodaySamplesOptions struct {
	Day string
}

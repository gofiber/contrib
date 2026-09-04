// Package storage defines the persistence contract for Fiber Uptime backends.
//
// All day strings are local calendar dates formatted as YYYY-MM-DD in the
// timezone configured for Uptime. This representation sorts chronologically
// when compared lexicographically.
package storage

import (
	"context"
	"time"
)

// Store persists uptime state. Implementations must be safe for concurrent use.
//
// Store does not define resource initialization or shutdown. A store must be
// ready for use before it is passed to uptime.New, and its caller owns its
// lifecycle.
//
// Implementations may optionally provide Name() string to customize the
// storage driver name exposed by uptime status responses.
type Store interface {
	// UpsertService creates or refreshes a logical service according to the
	// timestamp and metadata semantics documented by Service.
	UpsertService(ctx context.Context, service Service) error
	// UpsertInstance creates or refreshes one process instance according to the
	// timestamp and metadata semantics documented by Instance.
	UpsertInstance(ctx context.Context, instance Instance) error
	// WriteHeartbeat records one up slot and refreshes the referenced service and
	// instance last-seen timestamps monotonically.
	WriteHeartbeat(ctx context.Context, heartbeat Heartbeat) error

	// RollupDaily finalizes raw samples for days before the exclusive boundary.
	// Repeated or concurrent rollups must not overwrite a finalized daily row.
	RollupDaily(ctx context.Context, options RollupOptions) error
	// Cleanup removes expired daily and raw sample data according to the
	// exclusive boundaries in CleanupOptions.
	Cleanup(ctx context.Context, options CleanupOptions) error

	// ListServices returns all persisted services. Result ordering is unspecified.
	ListServices(ctx context.Context) ([]Service, error)
	// QueryDaily returns daily rows matching the requested services and day range.
	// Result ordering is unspecified.
	QueryDaily(ctx context.Context, options QueryDailyOptions) ([]DailyStatus, error)
	// QueryTodaySamples returns raw service-level summaries for one day. Result
	// ordering is unspecified.
	QueryTodaySamples(ctx context.Context, options QueryTodaySamplesOptions) ([]TodaySampleStatus, error)
}

// Service is the stable logical service identity shown on the dashboard.
type Service struct {
	// ID is the stable logical service identity.
	ID string
	// Name and Description are refreshable display metadata.
	Name        string
	Description string
	// CreatedAt is when the service was first observed. It remains stable across
	// later upserts.
	CreatedAt time.Time
	// LastSeenAt must advance monotonically and never move backwards.
	LastSeenAt time.Time
	// SampleInterval is refreshable service metadata.
	SampleInterval time.Duration
}

// Instance describes one process lifetime of a service.
type Instance struct {
	// ID identifies one process lifetime.
	ID int64
	// ServiceID, Hostname, and PID are refreshable instance metadata.
	ServiceID string
	Hostname  string
	PID       int
	// StartedAt is when this process instance started. It remains stable across
	// later upserts.
	StartedAt time.Time
	// LastSeenAt must advance monotonically and never move backwards.
	LastSeenAt time.Time
}

// Heartbeat records that one service was up during a day slot.
//
// Slot is the zero-based sample interval within Day. The tuple (ServiceID, Day,
// Slot) is the deduplication identity: repeated writes, including writes from
// different instances, count as exactly one service-level up slot. SeenAt
// refreshes the referenced service and instance last-seen timestamps
// monotonically.
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
	Finalized     bool
}

// TodaySampleStatus is the current raw service-level summary for one day.
type TodaySampleStatus struct {
	ServiceID string
	Day       string
	UpSlots   int
}

// ExpectedSlotsFunc returns the expected slot count for one service day.
type ExpectedSlotsFunc func(service Service, day string) int

// RollupOptions controls daily sample finalization.
type RollupOptions struct {
	// BeforeDay is an exclusive upper bound. Days less than BeforeDay are
	// eligible for rollup. An empty value performs no rollup.
	BeforeDay string
	// ExpectedSlots computes the expected slot count for a service day. A backend
	// may call it while RollupDaily is executing, but must not retain or invoke it
	// after RollupDaily returns. A nil callback means zero expected slots.
	ExpectedSlots ExpectedSlotsFunc
}

// CleanupOptions controls retention cleanup using exclusive day boundaries.
// Boundary days themselves are retained, and samples required by an
// unfinalized daily row must not be removed.
type CleanupOptions struct {
    // DailyBeforeDay is an exclusive upper bound for daily status cleanup.
    // Empty disables daily status cleanup.
    DailyBeforeDay string

    // SamplesBeforeDay is an exclusive upper bound for raw sample cleanup.
    // Empty disables raw sample cleanup.
    SamplesBeforeDay string
}

// QueryDailyOptions selects persisted daily status rows.
type QueryDailyOptions struct {
	// ServiceIDs selects services. Nil means all services; a non-nil empty slice
	// means no services.
	ServiceIDs []string
	// FromDay and ToDay are inclusive. Empty values leave that end unbounded.
	FromDay string
	ToDay   string
}

// QueryTodaySamplesOptions selects raw service-level summaries for one day.
type QueryTodaySamplesOptions struct {
    // ServiceIDs selects services. Nil means all services; a non-nil empty
    // slice means no services.
    ServiceIDs []string

    // Day is the local calendar day to query. Empty returns no rows.
    Day string
}

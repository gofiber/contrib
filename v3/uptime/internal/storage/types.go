package storage

import (
	"context"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
)

// Store persists uptime state. Implementations must be safe for concurrent use.
type Store interface {
	uptimestorage.Store

	Init(ctx context.Context) error
	Close() error
}

type Service = uptimestorage.Service
type Instance = uptimestorage.Instance
type Heartbeat = uptimestorage.Heartbeat
type DailyStatus = uptimestorage.DailyStatus
type TodaySampleStatus = uptimestorage.TodaySampleStatus
type RollupOptions = uptimestorage.RollupOptions
type CleanupOptions = uptimestorage.CleanupOptions
type QueryDailyOptions = uptimestorage.QueryDailyOptions
type QueryTodaySamplesOptions = uptimestorage.QueryTodaySamplesOptions

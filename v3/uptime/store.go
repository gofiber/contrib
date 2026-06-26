package uptime

import "github.com/gofiber/contrib/v3/uptime/internal/storage"

type Store = storage.Store
type Service = storage.Service
type Instance = storage.Instance
type Heartbeat = storage.Heartbeat
type DailyStatus = storage.DailyStatus
type TodaySampleStatus = storage.TodaySampleStatus
type RollupOptions = storage.RollupOptions
type CleanupOptions = storage.CleanupOptions
type QueryDailyOptions = storage.QueryDailyOptions
type QueryTodaySamplesOptions = storage.QueryTodaySamplesOptions
type AlertState = storage.AlertState
type AlertDecision = storage.AlertDecision
type AlertStateStore = storage.AlertStateStore
type SQLiteConfig = storage.SQLiteConfig
type SQLiteStore = storage.SQLiteStore
type PostgresConfig = storage.PostgresConfig
type TableNames = storage.TableNames
type PostgresStore = storage.PostgresStore

func NewSQLiteStore(config SQLiteConfig) *SQLiteStore {
	return storage.NewSQLiteStore(config)
}

func NewPostgresStore(config PostgresConfig) *PostgresStore {
	return storage.NewPostgresStore(config)
}

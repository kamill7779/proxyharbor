package cluster

import (
	"context"
	"log/slog"
	"time"

	"github.com/kamill7779/proxyharbor/internal/storage"
)

const GlobalMaintenanceLock = "global-maintenance"

type Runner struct {
	Store             storage.ClusterStore
	InstanceID        string
	Role              string
	Version           string
	ConfigFingerprint string
	StartedAt         time.Time
	HeartbeatInterval time.Duration
	LeaderLeaseTTL    time.Duration
	MaintenanceEvery  time.Duration
	MaintenanceLimit  int
	Logger            *slog.Logger
}

func (r Runner) Run(ctx context.Context) {
	if r.Store == nil || r.InstanceID == "" {
		return
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	heartbeatEvery := positiveDuration(r.HeartbeatInterval, 10*time.Second)
	maintenanceEvery := positiveDuration(r.MaintenanceEvery, 30*time.Second)
	leaderTTL := positiveDuration(r.LeaderLeaseTTL, 45*time.Second)
	limit := r.MaintenanceLimit
	if limit <= 0 {
		limit = 500
	}
	startedAt := r.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	r.tick(ctx, logger, startedAt, leaderTTL, limit)
	heartbeatTicker := time.NewTicker(heartbeatEvery)
	defer heartbeatTicker.Stop()
	maintenanceTicker := time.NewTicker(maintenanceEvery)
	defer maintenanceTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
			r.heartbeat(ctx, logger, startedAt)
		case <-maintenanceTicker.C:
			r.maintain(ctx, logger, leaderTTL, limit)
		}
	}
}

func (r Runner) tick(ctx context.Context, logger *slog.Logger, startedAt time.Time, leaderTTL time.Duration, limit int) {
	r.heartbeat(ctx, logger, startedAt)
	r.maintain(ctx, logger, leaderTTL, limit)
}

func (r Runner) heartbeat(ctx context.Context, logger *slog.Logger, startedAt time.Time) {
	err := r.Store.HeartbeatInstance(ctx, storage.InstanceHeartbeat{
		InstanceID:        r.InstanceID,
		Role:              r.Role,
		Version:           r.Version,
		ConfigFingerprint: r.ConfigFingerprint,
		StartedAt:         startedAt,
		LastSeenAt:        time.Now().UTC(),
	})
	if err != nil {
		logger.Warn("cluster heartbeat failed", "err", err)
	}
}

func (r Runner) maintain(ctx context.Context, logger *slog.Logger, leaderTTL time.Duration, limit int) {
	leader, err := r.Store.TryAcquireLock(ctx, GlobalMaintenanceLock, r.InstanceID, leaderTTL)
	if err != nil {
		logger.Warn("cluster leader acquisition failed", "err", err)
		return
	}
	if !leader {
		return
	}
	deleted, err := r.Store.DeleteExpiredLeasesBatch(ctx, time.Now().UTC(), limit)
	if err != nil {
		logger.Warn("expired lease cleanup failed", "err", err)
		return
	}
	if deleted > 0 {
		logger.Info("expired leases cleaned", "count", deleted)
	}
}

func positiveDuration(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

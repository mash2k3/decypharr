package rclone

import (
	"context"
	"fmt"
	"time"
)

// RecoverMount attempts to recover a failed mount
func (m *Manager) RecoverMount(ctx context.Context) error {
	mountInfo := m.getMountInfo()

	if mountInfo == nil {
		return fmt.Errorf("no mount info available for recovery")
	}

	m.logger.Warn().Msg("Attempting to recover mount")

	// First try to unmount cleanly
	m.unmount(ctx)

	// Wait a moment
	time.Sleep(1 * time.Second)

	// Try to remount
	if err := m.Start(context.Background()); err != nil {
		return fmt.Errorf("failed to recover mount : %w", err)
	}

	m.logger.Info().Msg("Successfully recovered mount")
	return nil
}

// MonitorMounts continuously monitors mount health and attempts recovery
func (m *Manager) MonitorMounts(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Debug().Msg("Mount monitoring stopped")
			return
		case <-ticker.C:
			m.performMountHealthCheck()
		}
	}
}

// performMountHealthCheck checks and attempts to recover unhealthy mounts
func (m *Manager) performMountHealthCheck() {
	if err := m.client.CheckMountHealth(context.Background(), FSName); err != nil {
		m.logger.Warn().Err(err).Msg("Mount health check failed, attempting recovery")

		// Mark mount as unhealthy
		mountInfo := m.getMountInfo()
		if mountInfo == nil {
			return
		}
		mountInfo.Error = "Health check failed"
		mountInfo.Mounted = false
		m.info.Store(mountInfo)

		// Attempt recovery
		go func() {
			if err := m.RecoverMount(m.ctx); err != nil {
				m.logger.Error().Msg("Failed to recover mount")
			}
		}()
	}
}

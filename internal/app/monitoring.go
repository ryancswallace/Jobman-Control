package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type monitoringPublisher interface {
	PublishMonitoringEvents(context.Context, int) (int, error)
	PruneMonitoringEvents(context.Context, int) (int, error)
}

func runMonitoringPublisher(ctx context.Context, logger *slog.Logger, publisher monitoringPublisher, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := publisher.PublishMonitoringEvents(cycle, 256); err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "monitoring publication failed")
		}
		if _, err := publisher.PruneMonitoringEvents(cycle, 1000); err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "monitoring feed retention failed")
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

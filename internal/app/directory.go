package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

type directoryStore interface {
	DirectoryRecoveryEpoch(context.Context) (int64, error)
	ApplyDirectorySnapshot(context.Context, domain.DirectorySnapshot) error
	RecordDirectoryFailure(context.Context, string, int64) error
}

func runDirectory(ctx context.Context, logger *slog.Logger, store directoryStore, config directory.Config) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	reader := directory.Reader{Config: config}
	for {
		reconcileDirectory(ctx, logger, store, reader.Read, config.Mapping.SourceID, config.Mapping.Revision)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reconcileDirectory(ctx context.Context, logger *slog.Logger, store directoryStore, read func(context.Context, int64) (domain.DirectorySnapshot, error), sourceID string, revision int64) {
	cycle, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	epoch, err := store.DirectoryRecoveryEpoch(cycle)
	var snapshot domain.DirectorySnapshot
	if err == nil {
		snapshot, err = read(cycle, epoch)
	}
	if err == nil {
		err = store.ApplyDirectorySnapshot(cycle, snapshot)
	}
	if err != nil {
		// Only bounded diagnostic codes enter logs, never raw LDAP errors/DNs.
		logger.WarnContext(ctx, "Directory verification failed; prior proof will expire", "source-id", sourceID, "error-code", "verification_failed")
		failureContext, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		if failureErr := store.RecordDirectoryFailure(failureContext, sourceID, revision); failureErr != nil {
			logger.WarnContext(ctx, "Directory failure state could not be recorded", "source-id", sourceID)
		}
		return
	}
	if snapshot.IgnoredDirectMembers > 0 {
		logger.WarnContext(ctx, "Directory direct members lack an approved user mapping", "source-id", sourceID, "unmapped-members", snapshot.IgnoredDirectMembers)
	}
}

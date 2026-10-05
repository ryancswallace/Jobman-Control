package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type directoryStoreStub struct {
	applied, failed int
	epoch           int64
	err             error
}

func (s *directoryStoreStub) DirectoryRecoveryEpoch(context.Context) (int64, error) {
	return s.epoch, s.err
}

func (s *directoryStoreStub) ApplyDirectorySnapshot(_ context.Context, snapshot domain.DirectorySnapshot) error {
	s.applied++
	if snapshot.RecoveryEpoch != s.epoch {
		return errors.New("wrong recovery fence")
	}
	return nil
}

func (s *directoryStoreStub) RecordDirectoryFailure(context.Context, string, int64) error {
	s.failed++
	return nil
}

func TestDirectoryCyclePreservesFailureBoundary(t *testing.T) {
	t.Parallel()
	for _, failure := range []bool{false, true} {
		store := &directoryStoreStub{epoch: 7}
		var output bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&output, nil))
		read := func(ctx context.Context, epoch int64) (domain.DirectorySnapshot, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("directory read has no deadline")
			}
			if failure {
				return domain.DirectorySnapshot{}, errors.New("secret-value and sensitive DN")
			}
			return domain.DirectorySnapshot{RecoveryEpoch: epoch, IgnoredDirectMembers: 1}, nil
		}
		reconcileDirectory(t.Context(), logger, store, read, "synthetic-source", 1)
		if failure && (store.applied != 0 || store.failed != 1) {
			t.Fatalf("failed read was applied: %#v", store)
		}
		if !failure && (store.applied != 1 || store.failed != 0) {
			t.Fatalf("complete read not applied: %#v", store)
		}
		if strings.Contains(output.String(), "secret-value") {
			t.Fatal("raw directory error leaked to logs")
		}
	}
}

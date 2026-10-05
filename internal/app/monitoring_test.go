package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type monitoringPublisherStub struct {
	t                 *testing.T
	cancel            context.CancelFunc
	published, pruned int
}

func (p *monitoringPublisherStub) PublishMonitoringEvents(ctx context.Context, limit int) (int, error) {
	p.published++
	if _, ok := ctx.Deadline(); !ok || limit != 256 {
		p.t.Fatal("publication cycle not bounded")
	}
	return 0, errors.New("private error details")
}

func (p *monitoringPublisherStub) PruneMonitoringEvents(_ context.Context, limit int) (int, error) {
	p.pruned++
	if limit != 1000 {
		p.t.Fatal("retention unbounded")
	}
	p.cancel()
	return 0, context.Canceled
}

func TestMonitoringPublisherBoundedCycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stub := &monitoringPublisherStub{t: t, cancel: cancel}
	var output bytes.Buffer
	runMonitoringPublisher(ctx, slog.New(slog.NewJSONHandler(&output, nil)), stub, time.Hour)
	if stub.published != 1 || stub.pruned != 1 || strings.Contains(output.String(), "private error") || !strings.Contains(output.String(), "monitoring publication failed") {
		t.Fatalf("cycle=%#v", stub)
	}
}

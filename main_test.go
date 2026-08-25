package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestExecuteVersionAndUsage(t *testing.T) {
	t.Parallel()
	runner := func(context.Context, *slog.Logger) error {
		t.Fatal("service runner called")

		return nil
	}
	for _, argument := range []string{"--version", "version"} {
		var stdout, stderr bytes.Buffer
		if status := execute([]string{argument}, &stdout, &stderr, runner); status != 0 ||
			!strings.HasPrefix(stdout.String(), "jobman-control ") || stderr.Len() != 0 {
			t.Fatalf("execute(%q) = %d, stdout %q, stderr %q", argument, status, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if status := execute([]string{"unexpected"}, &stdout, &stderr, runner); status != 2 ||
		!strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("execute(unexpected) = %d, stderr %q", status, stderr.String())
	}
}

func TestExecuteRunsServiceAndReportsSafeError(t *testing.T) {
	t.Parallel()
	want := errors.New("injected startup failure")
	var stdout, stderr bytes.Buffer
	status := execute(nil, &stdout, &stderr, func(context.Context, *slog.Logger) error {
		return want
	})
	if status != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), want.Error()) {
		t.Fatalf("execute() = %d, stdout %q, stderr %q", status, stdout.String(), stderr.String())
	}
}

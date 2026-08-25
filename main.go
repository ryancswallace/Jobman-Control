package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryancswallace/jobman-control/internal/app"
	"github.com/ryancswallace/jobman-control/internal/buildinfo"
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, app.Run))
}

type serviceRunner func(context.Context, *slog.Logger) error

func execute(arguments []string, stdout, stderr io.Writer, runService serviceRunner) int {
	if len(arguments) == 1 && (arguments[0] == "--version" || arguments[0] == "version") {
		_, _ = fmt.Fprintf(stdout, "jobman-control %s\n", buildinfo.Display())

		return 0
	}
	if len(arguments) != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: jobman-control [--version]")

		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := runService(ctx, logger); err != nil {
		_, _ = fmt.Fprintf(stderr, "jobman-control: %v\n", err)

		return 1
	}

	return 0
}

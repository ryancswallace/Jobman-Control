package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryancswallace/jobman-control/internal/app"
	"github.com/ryancswallace/jobman-control/internal/buildinfo"
	"github.com/ryancswallace/jobman-control/internal/config"
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr, app.RunFile))
}

type serviceRunner func(context.Context, *slog.Logger, string) error

func execute(arguments []string, stdout, stderr io.Writer, runService serviceRunner) int {
	if len(arguments) > 0 && arguments[0] == "config" {
		return configCommand(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "resources" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return resourcesCommand(ctx, arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && (arguments[0] == "migrate" || arguments[0] == "recovery") {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return operatorCommand(ctx, arguments, stdout, stderr)
	}
	if len(arguments) == 1 && (arguments[0] == "--version" || arguments[0] == "version") {
		_, _ = fmt.Fprintf(stdout, "jobman-control %s\n", buildinfo.Display())

		return 0
	}
	flags := flag.NewFlagSet("jobman-control", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "explicit JSON configuration file")
	check := flags.Bool("check-config", false, "validate without opening credentials or contacting services")
	show := flags.Bool("show-config", false, "show redacted effective configuration and origins")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || ((*check || *show) && *path == "") {
		_, _ = fmt.Fprintln(stderr, "usage: jobman-control [--version] [--config FILE [--check-config | --show-config]] | migrate | recovery")
		return 2
	}
	if *check || *show {
		inspection, err := config.InspectFile(*path)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "jobman-control: %v\n", err)
			return 1
		}
		if *show {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			if err = encoder.Encode(inspection); err != nil {
				return 1
			}
		} else {
			_, _ = fmt.Fprintln(stdout, "configuration valid (credential and service checks deferred)")
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := runService(ctx, logger, *path); err != nil {
		_, _ = fmt.Fprintf(stderr, "jobman-control: %v\n", err)

		return 1
	}

	return 0
}

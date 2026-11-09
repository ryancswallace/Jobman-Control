package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/config"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

var changeReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

type operatorRequest struct {
	action    string
	urlFile   string
	reference string
	expected  int64
	next      int64
	timeout   time.Duration
}

func parseOperatorRequest(arguments []string) (operatorRequest, error) {
	if len(arguments) == 0 {
		return operatorRequest{}, errors.New("provide an operator action")
	}
	request := operatorRequest{action: arguments[0]}
	arguments = arguments[1:]
	if request.action == "recovery" {
		if len(arguments) == 0 {
			return request, errors.New("recovery requires status, hold or release")
		}
		request.action, arguments = arguments[0], arguments[1:]
		if request.action != "status" && request.action != "hold" && request.action != "release" {
			return request, errors.New("recovery requires status, hold or release")
		}
	}
	flags := flag.NewFlagSet(request.action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&request.urlFile, "database-url-file", "", "protected PostgreSQL URI file")
	flags.DurationVar(&request.timeout, "timeout", 2*time.Minute, "operator command deadline")
	switch request.action {
	case "migrate", "status":
	case "hold", "release":
		flags.Int64Var(&request.expected, "expected-epoch", 0, "current database epoch")
		flags.StringVar(&request.reference, "reason", "", "change-control reference")
		if request.action == "hold" {
			flags.Int64Var(&request.next, "new-epoch", 0, "new externally fenced epoch")
		}
	default:
		return request, errors.New("unknown operator action")
	}
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || request.urlFile == "" || request.timeout <= 0 || request.timeout > 30*time.Minute {
		return request, errors.New("provide --database-url-file and a deadline of at most 30 minutes")
	}
	if request.action == "hold" || request.action == "release" {
		if request.expected < 1 || !changeReference.MatchString(request.reference) {
			return request, errors.New("provide a positive --expected-epoch and a change reference in --reason")
		}
		if request.action == "hold" && request.next <= request.expected {
			return request, errors.New("--new-epoch must exceed --expected-epoch")
		}
	}
	return request, nil
}

func operatorCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	request, err := parseOperatorRequest(arguments)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jobman-control: %v\n", err)
		return 2
	}
	url, err := config.ReadDatabaseURLFile(request.urlFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "jobman-control: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, request.timeout)
	defer cancel()
	pool, err := postgres.Open(ctx, url)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "jobman-control: could not connect to the operator database")
		return 1
	}
	defer pool.Close()
	result, err := request.run(ctx, pool)
	if errors.Is(err, domain.ErrConflict) {
		_, _ = fmt.Fprintln(stderr, "jobman-control: recovery epoch or hold changed. Inspect status before retrying.")
		return 1
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "jobman-control: operator action failed. Inspect database authorization and state before retrying.")
		return 1
	}
	if err = json.NewEncoder(stdout).Encode(result); err != nil {
		_, _ = fmt.Fprintln(stderr, "jobman-control: could not write the operator result")
		return 1
	}
	return 0
}

func (request operatorRequest) run(ctx context.Context, pool *pgxpool.Pool) (any, error) {
	if request.action == "migrate" {
		if err := postgres.Migrate(ctx, pool); err != nil {
			return nil, err
		}
		return map[string]bool{"migrated": true}, nil
	}
	if err := postgres.CheckMigrations(ctx, pool); err != nil {
		return nil, err
	}
	switch request.action {
	case "hold":
		return postgres.SetRecovery(ctx, pool, request.expected, request.next, true, request.reference)
	case "release":
		return postgres.SetRecovery(ctx, pool, request.expected, request.expected, false, request.reference)
	default:
		return postgres.RecoveryStatus(ctx, pool)
	}
}

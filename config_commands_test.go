package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestAuxiliaryCommandsNeverStartService(t *testing.T) {
	t.Parallel()
	runner := func(context.Context, *slog.Logger, string) error {
		t.Fatal("offline command started service")
		return nil
	}
	for _, kind := range []string{"directory", "delegation", "resources"} {
		var stdout, stderr bytes.Buffer
		if status := execute([]string{"config", "validate", "--kind", kind, "etc/jobman-control/" + kind + ".example.json"}, &stdout, &stderr, runner); status != 0 {
			t.Fatalf("%s validation failed: %s", kind, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if status := execute([]string{"resources", "plan", "--file", "etc/jobman-control/resources.example.json"}, &stdout, &stderr, runner); status != 0 {
		t.Fatalf("offline plan failed: %s", stderr.String())
	}
	for _, private := range []string{"example.invalid", "00000000-0000-4000-8000", "maxActiveJobs"} {
		if strings.Contains(stdout.String(), private) {
			t.Fatal("offline plan exposed deployment content")
		}
	}
	if !strings.Contains(stdout.String(), `"liveVerified":false`) {
		t.Fatal("offline plan claimed live verification")
	}
	stdout.Reset()
	stderr.Reset()
	if status := execute([]string{"resources", "apply", "--file", "etc/jobman-control/resources.example.json"}, &stdout, &stderr, runner); status != 2 {
		t.Fatal("apply accepted missing credential/digest")
	}
}

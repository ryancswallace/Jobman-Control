package postgres

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDecodeJobExecution(t *testing.T) {
	t.Parallel()
	expected := domain.JobExecution{Command: domain.JobCommand{Executable: "/bin/sh", Args: []string{"-c", "printf '%s' \"$1\"", "", "a b", "line\nnext", "é"}}, WorkingDirectory: "/synthetic/work"}
	raw, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	actual, reason := decodeJobExecution(raw)
	if reason != "" || !reflect.DeepEqual(actual, &expected) {
		t.Fatal("argument boundaries or contents changed")
	}
	for _, test := range []struct{ name, document, reason string }{
		{"no arguments", `{"command":{"executable":"true"},"workingDirectory":"."}`, ""},
		{"empty arguments", `{"command":{"executable":"true","args":[]},"workingDirectory":"."}`, ""},
		{"null argument", `{"command":{"executable":"true","args":[null]},"workingDirectory":"."}`, "unsupported"},
		{"null among arguments", `{"command":{"executable":"true","args":["",null,"literal"]},"workingDirectory":"."}`, "unsupported"},
		{"null arguments", `{"command":{"executable":"true","args":null},"workingDirectory":"."}`, "unsupported"},
		{"missing command", `{"workingDirectory":"."}`, "missing"},
		{"missing directory", `{"command":{"executable":"true"}}`, "missing"},
		{"null fields", `{"command":null,"workingDirectory":null}`, "missing"},
		{"shell form", `{"command":{"shell":{"capability":"sh","script":"true"}},"workingDirectory":"."}`, "unsupported"},
		{"unknown field", `{"command":{"executable":"true","secret":"synthetic"},"workingDirectory":"."}`, "unsupported"},
		{"wrong type", `{"command":{"executable":"true","args":[1]},"workingDirectory":"."}`, "unsupported"},
		{"nul argument", `{"command":{"executable":"true","args":["\u0000"]},"workingDirectory":"."}`, "unsupported"},
		{"empty executable", `{"command":{"executable":""},"workingDirectory":"."}`, "unsupported"},
		{"trailing JSON", string(raw) + `{}`, "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason := decodeJobExecution([]byte(test.document))
			if reason != test.reason || (got == nil) != (reason != "") {
				t.Fatalf("unexpected availability %q", reason)
			}
			if got != nil && got.Command.Args == nil {
				t.Fatal("args must serialize as an array")
			}
		})
	}
}

func TestDecodeJobExecutionBounds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*domain.JobExecution)
	}{
		{"executable", func(e *domain.JobExecution) {
			e.Command.Executable = strings.Repeat("x", maximumJobExecutionStringBytes+1)
		}},
		{"directory", func(e *domain.JobExecution) {
			e.WorkingDirectory = strings.Repeat("x", maximumJobExecutionStringBytes+1)
		}},
		{"argument", func(e *domain.JobExecution) {
			e.Command.Args = []string{strings.Repeat("x", maximumJobExecutionStringBytes+1)}
		}},
		{"argument count", func(e *domain.JobExecution) { e.Command.Args = make([]string, maximumJobCommandArgs+1) }},
		{"total bytes", func(e *domain.JobExecution) {
			for range 33 {
				e.Command.Args = append(e.Command.Args, strings.Repeat("x", maximumJobExecutionStringBytes))
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := domain.JobExecution{Command: domain.JobCommand{Executable: "true", Args: []string{}}, WorkingDirectory: "."}
			test.change(&e)
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if got, reason := decodeJobExecution(raw); got != nil || reason != "too_large" {
				t.Fatalf("bounds not enforced: %q", reason)
			}
		})
	}
	// PostgreSQL text does not HTML-escape. Enforce the bound again on Go's wire
	// representation so escaping cannot inflate the response beyond its contract.
	raw := `{"command":{"executable":"true","args":[` + strings.Repeat(`"`+strings.Repeat("<", 60000)+`",`, 5) + `"` + strings.Repeat("<", 60000) + `"]},"workingDirectory":"."}`
	if got, reason := decodeJobExecution([]byte(raw)); got != nil || reason != "too_large" {
		t.Fatalf("escaped response bound not enforced: %q", reason)
	}
}

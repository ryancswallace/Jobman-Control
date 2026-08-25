package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckLinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "README.md"), "[docs](docs/guide.md)\n```md\n[ignored](missing.md)\n```\n")
	mustWrite(t, filepath.Join(root, "docs", "guide.md"), "[root](/README.md#top)\n[remote](https://example.com)\n")
	problems, err := check(root)
	if err != nil || len(problems) != 0 {
		t.Fatalf("check(valid) = %v, %v", problems, err)
	}
	mustWrite(t, filepath.Join(root, "README.md"), "[missing](docs/missing.md)\n")
	problems, err = check(root)
	if err != nil || len(problems) != 1 {
		t.Fatalf("check(broken) = %v, %v", problems, err)
	}
}

func TestExecuteAndDestinationParsing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "README.md"), "[valid](guide.md)\n")
	mustWrite(t, filepath.Join(root, "guide.md"), "guide\n")
	var stdout, stderr bytes.Buffer
	if status := execute([]string{"-root", root}, &stdout, &stderr); status != 0 ||
		!strings.Contains(stdout.String(), "links are consistent") {
		t.Fatalf("execute(valid) = %d, stdout %q, stderr %q", status, stdout.String(), stderr.String())
	}
	for _, arguments := range [][]string{{"-unknown"}, {"positional"}, {"-root", filepath.Join(root, "missing")}} {
		stderr.Reset()
		if status := execute(arguments, &stdout, &stderr); status != 2 {
			t.Errorf("execute(%q) = %d", arguments, status)
		}
	}
	if got := markdownDestination("<guide.md> title"); got != "guide.md" {
		t.Fatalf("markdownDestination() = %q", got)
	}
	if resolved, local := resolveDestination("root", "source", "https://example.com"); local || resolved != "" {
		t.Fatalf("resolveDestination(remote) = %q, %t", resolved, local)
	}
}

func mustWrite(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

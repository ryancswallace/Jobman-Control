package devel_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGoVersionUpdatePreservesCanonicalModuleAndIsIdempotent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("maintenance runs on a Unix runner")
	}
	directory := t.TempDir()
	paths := []string{"go.version", "go.mod", ".golangci.yml", "Dockerfile", ".devcontainer/Dockerfile", ".devcontainer/devcontainer.json", ".devcontainer/README.md"}
	original := map[string]string{}
	for _, path := range paths {
		contents := readReleaseFile(t, "../"+path)
		original[path] = contents
		destination := filepath.Join(directory, path)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs("updates/go-vers.sh")
	if err != nil {
		t.Fatal(err)
	}
	for run := range 2 {
		cmd := exec.CommandContext(t.Context(), "sh", script)
		cmd.Dir = directory
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GO_VERS=" + strings.TrimSpace(original["go.version"])}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run %d: %v: %s", run, err, output)
		}
		for _, path := range paths {
			contents, err := os.ReadFile(filepath.Join(directory, path))
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != original[path] {
				t.Errorf("run %d changed synchronized %s", run, path)
			}
		}
	}
}

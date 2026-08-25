package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunGeneratesFormula(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	manifest := filepath.Join(directory, "jobman-control_1.2.3_checksums.txt")
	contents := strings.Repeat("a", 64) + "  jobman-control_1.2.3_darwin_amd64.tar.gz\n" +
		strings.Repeat("b", 64) + "  jobman-control_1.2.3_darwin_arm64.tar.gz\n"
	if err := os.WriteFile(manifest, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "Formula", "jobman-control.rb")
	if err := run(manifest, output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	formula, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"class JobmanControl", "1.2.3", strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if !bytes.Contains(formula, []byte(expected)) {
			t.Errorf("formula does not contain %q", expected)
		}
	}
}

func TestReadFormulaDataRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	if _, err := readFormulaData("wrong-name.txt"); err == nil {
		t.Fatal("readFormulaData(wrong name) error = nil")
	}
	directory := t.TempDir()
	manifest := filepath.Join(directory, "jobman-control_1.2.3_checksums.txt")
	if err := os.WriteFile(manifest, []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFormulaData(manifest); err == nil {
		t.Fatal("readFormulaData(missing archives) error = nil")
	}
}

func TestExecuteUsageAndFailure(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if status := execute(nil, &stderr); status != 1 {
		t.Fatalf("execute(missing) = %d", status)
	}
	if status := execute([]string{"extra"}, &stderr); status != 2 {
		t.Fatalf("execute(positional) = %d", status)
	}
}

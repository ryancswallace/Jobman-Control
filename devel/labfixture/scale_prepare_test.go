package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestScalePreparationStopsBeforeInputsOnPendingOrUnsafeRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private source operator helper")
	}
	for _, marker := range []string{scalePending, scaleComplete, ".directory-acceptance-example.json", diagnosticReceiptName} {
		t.Run(marker, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root, output := filepath.Join(base, "source"), filepath.Join(base, "output")
			for _, path := range []string{root, output} {
				if err = os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err = writeDiagnosticJSON(root, marker, map[string]bool{"synthetic": true}); err != nil {
				t.Fatal(err)
			}
			if err = prepareScale(t.Context(), root, "absent-input", "absent-database", "", output, primaryProfile()); err == nil {
				t.Fatal("pending source operation accepted")
			}
			sourceEntries, err := os.ReadDir(root)
			if err != nil || len(sourceEntries) != 1 {
				t.Fatal("pending source state changed")
			}
			outputEntries, err := os.ReadDir(output)
			if err != nil || len(outputEntries) != 0 {
				t.Fatal("preflight wrote an output")
			}
			if err = prepareScale(t.Context(), root, "absent-input", "absent-database", "", root, primaryProfile()); err == nil {
				t.Fatal("overlapping output accepted")
			}
		})
	}
}

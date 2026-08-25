// Command contractsync copies or verifies the pre-release Jobman protocol snapshot.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const checksumFilename = "checksums.txt"

var snapshotFiles = []string{
	"codec.go",
	"agent_contracts.go",
	"doc.go",
	"normalize.go",
	"schema.go",
	"types.go",
	"validate.go",
	"schema/job-request-v1alpha1.schema.json",
	"schema/collection-request-v1alpha1.schema.json",
	"schema/graph-request-v1alpha1.schema.json",
	"schema/workload-v1alpha1.schema.json",
	"schema/effective-execution-v1alpha1.schema.json",
	"schema/agent-assignment-v1alpha1.schema.json",
	"schema/agent-acceptance-v1alpha1.schema.json",
	"schema/launch-authorization-v1alpha1.schema.json",
	"schema/execution-event-v1alpha1.schema.json",
	"schema/desired-action-v1alpha1.schema.json",
	"schema/action-acknowledgement-v1alpha1.schema.json",
	"conformance/invalid/agent-assignment-bad-digest.json",
	"conformance/invalid/job-request-bad-digest.json",
	"conformance/invalid/workload-duplicate-secret.json",
	"conformance/invalid/workload-unknown-field.json",
	"conformance/manifest.json",
	"conformance/valid/job-request-minimal.json",
	"conformance/valid/effective-execution-minimal.json",
	"conformance/valid/agent-assignment-minimal.json",
	"conformance/valid/workload-full.json",
	"conformance/valid/workload-minimal.json",
}

func main() {
	source := flag.String("source", "", "Jobman protocol source directory")
	destination := flag.String(
		"destination",
		"contracts/jobman/v1alpha1",
		"snapshot destination directory",
	)
	check := flag.Bool("check", false, "verify the snapshot without changing it")
	flag.Parse()

	if err := run(*source, *destination, *check); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "contractsync: %v\n", err)
		os.Exit(1)
	}
}

func run(source, destination string, check bool) error {
	if destination == "" {
		return errors.New("destination is required")
	}
	files := append([]string(nil), snapshotFiles...)
	sort.Strings(files)
	if !check {
		if source == "" {
			return errors.New("source is required when synchronizing")
		}
		for _, name := range files {
			sourceName := sourceName(source, name)
			contents, err := os.ReadFile(sourceName)
			if err != nil {
				return fmt.Errorf("read source %q: %w", name, err)
			}
			if writeErr := writeAtomic(filepath.Join(destination, filepath.FromSlash(name)), contents, 0o644); writeErr != nil {
				return writeErr
			}
		}
	}

	checksums, err := snapshotChecksums(destination, files)
	if err != nil {
		return err
	}
	checksumPath := filepath.Join(destination, checksumFilename)
	if check {
		expected, readErr := os.ReadFile(checksumPath)
		if readErr != nil {
			return fmt.Errorf("read snapshot checksums: %w", readErr)
		}
		if !bytes.Equal(expected, checksums) {
			return errors.New("snapshot checksums are stale")
		}
		if source != "" {
			if compareErr := compareSource(source, destination, files); compareErr != nil {
				return compareErr
			}
		}

		return nil
	}

	return writeAtomic(checksumPath, checksums, 0o644)
}

func sourceName(source, snapshotName string) string {
	name := snapshotName
	if strings.HasPrefix(name, "conformance/") {
		name = "testdata/" + name
	}

	return filepath.Join(source, filepath.FromSlash(name))
}

func snapshotChecksums(destination string, files []string) ([]byte, error) {
	var checksums strings.Builder
	for _, name := range files {
		contents, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("read snapshot %q: %w", name, err)
		}
		sum := sha256.Sum256(contents)
		checksums.WriteString(hex.EncodeToString(sum[:]))
		checksums.WriteString("  ")
		checksums.WriteString(name)
		checksums.WriteByte('\n')
	}

	return []byte(checksums.String()), nil
}

func compareSource(source, destination string, files []string) error {
	for _, name := range files {
		sourceContents, err := os.ReadFile(sourceName(source, name))
		if err != nil {
			return fmt.Errorf("read source %q: %w", name, err)
		}
		snapshotContents, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("read snapshot %q: %w", name, err)
		}
		if !bytes.Equal(sourceContents, snapshotContents) {
			return fmt.Errorf("snapshot differs from source: %q", name)
		}
	}

	return nil
}

func writeAtomic(name string, contents []byte, permissions fs.FileMode) (resultErr error) {
	directory := filepath.Dir(name)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".contractsync-*")
	if err != nil {
		return fmt.Errorf("create temporary snapshot: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		removeErr := os.Remove(temporaryName)
		if removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove temporary snapshot: %w", removeErr))
		}
	}()
	if err = temporary.Chmod(permissions); err != nil {
		closeErr := temporary.Close()

		return errors.Join(fmt.Errorf("set snapshot permissions: %w", err), closeErr)
	}
	if _, err = temporary.Write(contents); err != nil {
		closeErr := temporary.Close()

		return errors.Join(fmt.Errorf("write snapshot: %w", err), closeErr)
	}
	if err = temporary.Sync(); err != nil {
		closeErr := temporary.Close()

		return errors.Join(fmt.Errorf("sync snapshot: %w", err), closeErr)
	}
	if err = temporary.Close(); err != nil {
		return fmt.Errorf("close snapshot: %w", err)
	}
	if err = os.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("replace snapshot: %w", err)
	}

	return nil
}

// Command homebrewformula generates a Jobman Control formula from a release
// checksum manifest.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

var (
	versionRE = regexp.MustCompile(`^jobman-control_(\d+\.\d+\.\d+)_checksums\.txt$`)
	digestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type formulaData struct {
	Version      string
	AMD64Archive string
	AMD64Digest  string
	ARM64Archive string
	ARM64Digest  string
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stderr))
}

func execute(arguments []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("homebrewformula", flag.ContinueOnError)
	flags.SetOutput(stderr)
	checksums := flags.String("checksums", "", "release checksum manifest")
	output := flags.String("output", "", "formula output path")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return 2
	}
	if err := run(*checksums, *output); err != nil {
		_, _ = fmt.Fprintf(stderr, "generate Homebrew formula: %v\n", err)

		return 1
	}

	return 0
}

func run(checksums, output string) error {
	if checksums == "" || output == "" {
		return errors.New("-checksums and -output are required")
	}
	data, err := readFormulaData(checksums)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	file, err := os.Create(output) // #nosec G304 -- caller selects generated output.
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	if err = formulaTemplate.Execute(file, data); err != nil {
		return errors.Join(fmt.Errorf("render formula: %w", err), file.Close())
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}

	return nil
}

func readFormulaData(name string) (formulaData, error) {
	match := versionRE.FindStringSubmatch(filepath.Base(name))
	if match == nil {
		return formulaData{}, fmt.Errorf("invalid checksum manifest name %q", filepath.Base(name))
	}
	data := formulaData{
		Version:      match[1],
		AMD64Archive: "jobman-control_" + match[1] + "_darwin_amd64.tar.gz",
		ARM64Archive: "jobman-control_" + match[1] + "_darwin_arm64.tar.gz",
	}
	file, err := os.Open(name) // #nosec G304 -- caller selects checksum input.
	if err != nil {
		return formulaData{}, fmt.Errorf("open checksums: %w", err)
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || !digestRE.MatchString(fields[0]) {
			continue
		}
		switch fields[1] {
		case data.AMD64Archive:
			data.AMD64Digest = fields[0]
		case data.ARM64Archive:
			data.ARM64Digest = fields[0]
		}
	}
	resultErr := scanner.Err()
	resultErr = errors.Join(resultErr, file.Close())
	if resultErr != nil {
		return formulaData{}, fmt.Errorf("read checksums: %w", resultErr)
	}
	if data.AMD64Digest == "" || data.ARM64Digest == "" {
		return formulaData{}, errors.New("checksum manifest is missing a macOS release archive")
	}

	return data, nil
}

var formulaTemplate = template.Must(template.New("formula").Parse(`# typed: strict
# frozen_string_literal: true

class JobmanControl < Formula
  desc "Shared PostgreSQL-backed control plane for Jobman"
  homepage "https://github.com/ryancswallace/jobman-control"
  license "MIT"

  on_macos do
    on_intel do
      url "https://github.com/ryancswallace/jobman-control/releases/download/v{{ .Version }}/{{ .AMD64Archive }}"
      sha256 "{{ .AMD64Digest }}"
    end
    on_arm do
      url "https://github.com/ryancswallace/jobman-control/releases/download/v{{ .Version }}/{{ .ARM64Archive }}"
      sha256 "{{ .ARM64Digest }}"
    end
  end

  def install
    bin.install "jobman-control"
    doc.install "README.md", "CHANGELOG.md", "SECURITY.md", "SUPPORT.md"
    (doc/"guides").install Dir["docs/*.md"]
  end

  test do
    assert_match "jobman-control {{ .Version }}", shell_output("#{bin}/jobman-control --version")
  end
end
`))

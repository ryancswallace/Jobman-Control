// Command docscheck verifies repository-relative Markdown links.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	inlineLink    = regexp.MustCompile(`!?\[[^]]*\]\(([^)]+)\)`)
	referenceLink = regexp.MustCompile(`^\s*\[[^]]+\]:\s*(\S+)`)
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

func execute(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("docscheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository root to check")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "docscheck does not accept positional arguments")

		return 2
	}
	problems, err := check(*root)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return 2
	}
	if len(problems) != 0 {
		for _, problem := range problems {
			_, _ = fmt.Fprintln(stderr, problem)
		}

		return 1
	}
	_, _ = fmt.Fprintln(stdout, "documentation links are consistent")

	return 0
}

func check(root string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	var files []string
	err = filepath.WalkDir(absRoot, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && name != absRoot && skippedDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			files = append(files, name)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk documentation: %w", err)
	}
	sort.Strings(files)
	var problems []string
	for _, name := range files {
		fileProblems, checkErr := checkFile(absRoot, name)
		if checkErr != nil {
			return nil, checkErr
		}
		problems = append(problems, fileProblems...)
	}

	return problems, nil
}

func skippedDirectory(name string) bool {
	switch name {
	case ".git", "bin", "dist", "vendor":
		return true
	default:
		return false
	}
}

func checkFile(root, name string) ([]string, error) {
	// #nosec G304 -- name comes from WalkDir beneath the selected repository root.
	content, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	relative, err := filepath.Rel(root, name)
	if err != nil {
		return nil, fmt.Errorf("relativize %s: %w", name, err)
	}

	return checkContent(root, filepath.Dir(name), filepath.ToSlash(relative), content)
}

func checkContent(root, sourceDirectory, relative string, content []byte) ([]string, error) {
	var problems []string
	scanner := bufio.NewScanner(bytes.NewReader(content))
	lineNumber := 0
	inFence := false
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, destination := range destinations(line) {
			resolved, local := resolveDestination(root, sourceDirectory, destination)
			if !local {
				continue
			}
			if _, statErr := os.Stat(resolved); statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					problems = append(problems, fmt.Sprintf(
						"%s:%d: relative link %q does not resolve", relative, lineNumber, destination,
					))
					continue
				}

				return nil, fmt.Errorf("inspect link target %s: %w", resolved, statErr)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", relative, err)
	}

	return problems, nil
}

func destinations(line string) []string {
	var result []string
	for _, match := range inlineLink.FindAllStringSubmatch(line, -1) {
		if len(match) == 2 {
			result = append(result, markdownDestination(match[1]))
		}
	}
	if match := referenceLink.FindStringSubmatch(line); len(match) == 2 {
		result = append(result, markdownDestination(match[1]))
	}

	return result
}

func markdownDestination(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "<") {
		if end := strings.Index(value, ">"); end >= 0 {
			return value[1:end]
		}
	}
	if index := strings.IndexAny(value, " \t"); index >= 0 {
		return value[:index]
	}

	return value
}

func resolveDestination(root, sourceDirectory, destination string) (string, bool) {
	if destination == "" || strings.HasPrefix(destination, "#") || strings.HasPrefix(destination, "//") {
		return "", false
	}
	parsed, err := url.Parse(destination)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "", false
	}
	linkPath, err := url.PathUnescape(parsed.Path)
	if err != nil || linkPath == "" {
		return "", false
	}
	if path.IsAbs(linkPath) {
		return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(linkPath, "/"))), true
	}

	return filepath.Join(sourceDirectory, filepath.FromSlash(linkPath)), true
}

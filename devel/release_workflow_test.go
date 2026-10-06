package devel_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseWorkflowUsesTestedMain(t *testing.T) {
	t.Parallel()

	workflow := readReleaseFile(t, "../.github/workflows/release.yml")
	for _, required := range []string{
		"workflow_run:",
		"steps.source.outputs.has_release == 'true'",
		"cycjimmy/semantic-release-action@b12c8f6015dc215fe37bc154d4ad456dd3833c90",
		"Verify exact release-candidate workflows",
		`"Jobman contract source"`,
		"Generate and stage SLSA provenance",
		"run: ./devel/verify-publish-release.sh",
		`PROMOTE_LATEST: "true"`,
		"ref: ${{ needs.release.outputs.source_commit }}",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow is missing %q", required)
		}
	}
	if strings.Contains(workflow, "tags: [\"v*\"]") {
		t.Error("release workflow must not run independently on tag pushes")
	}
}

func TestReleaseRecoveryPublishesOnlyVerifiedDraft(t *testing.T) {
	t.Parallel()

	workflow := readReleaseFile(t, "../.github/workflows/publish-staged-release.yml")
	for _, required := range []string{
		"group: jobman-control-release",
		"Verify and publish retained draft",
		"run: ./devel/verify-publish-release.sh",
		`PROMOTE_LATEST: "false"`,
		`"Jobman contract source"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("staged-release workflow is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"id-token: write",
		"goreleaser/goreleaser-action",
		"gh release edit",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("staged-release workflow contains unsafe recovery operation %q", forbidden)
		}
	}
}

func TestReleaseVerificationUsesMainWorkflowIdentity(t *testing.T) {
	t.Parallel()

	const identity = "https://github.com/ryancswallace/Jobman-Control/.github/workflows/release.yml@refs/heads/main"
	const incorrectIdentity = "https://github.com/ryancswallace/jobman-control/.github/workflows/release.yml@refs/heads/main"
	for _, path := range []string{
		"../.github/workflows/release.yml",
		"../.github/workflows/publish-homebrew-formula.yml",
		"../.github/workflows/repair-latest.yml",
		"publish-cloudsmith-packages.sh",
		"verify-publish-release.sh",
		"../RELEASE.md",
	} {
		contents := readReleaseFile(t, path)
		if !strings.Contains(contents, identity) {
			t.Errorf("%s is missing release workflow identity %q", path, identity)
		}
		if strings.Contains(contents, incorrectIdentity) {
			t.Errorf("%s contains case-mismatched release workflow identity %q", path, incorrectIdentity)
		}
	}

	for _, path := range []string{
		"../.github/workflows/publish-homebrew-formula.yml",
		"publish-cloudsmith-packages.sh",
	} {
		contents := readReleaseFile(t, path)
		if !strings.Contains(contents, "--signer-workflow ryancswallace/Jobman-Control/.github/workflows/release.yml") {
			t.Errorf("%s is missing the canonical attestation signer workflow", path)
		}
		if strings.Contains(contents, "--source-ref \"refs/tags/") {
			t.Errorf("%s still verifies tag-triggered attestations", path)
		}
	}

	helper := readReleaseFile(t, "verify-publish-release.sh")
	for _, required := range []string{
		`[[ "$GITHUB_REPOSITORY" != "ryancswallace/Jobman-Control" ]]`,
		"--source-uri github.com/ryancswallace/Jobman-Control",
		"git+https://github.com/ryancswallace/Jobman-Control@refs/heads/main",
	} {
		if !strings.Contains(helper, required) {
			t.Errorf("release publication helper is missing canonical source identity %q", required)
		}
	}
	if strings.Contains(helper, `[[ "$GITHUB_REPOSITORY" != "ryancswallace/jobman-control" ]]`) {
		t.Error("release publication helper contains a case-mismatched repository guard")
	}
}

func TestReleasePublicationHelperIsExecutable(t *testing.T) {
	t.Parallel()

	info, err := os.Stat("verify-publish-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Error("release publication helper is not executable")
	}
}

func readReleaseFile(t *testing.T, path string) string {
	t.Helper()

	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

// cspell:ignore NOSYSTEM pipefail
// Exercise the workflow's actual local publication decision against a disposable
// Git index. An untracked first formula must be published just like an update.
func TestHomebrewPublicationDetectsFirstFormulaAndUpdates(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew publication runs on a Unix runner")
	}
	workflow := readReleaseFile(t, "../.github/workflows/publish-homebrew-formula.yml")
	_, proposal, ok := strings.Cut(workflow, "      - name: Propose verified formula\n")
	if !ok {
		t.Fatal("missing formula proposal step")
	}
	_, script, ok := strings.Cut(proposal, "        run: |\n")
	if !ok {
		t.Fatal("missing formula proposal script")
	}
	decision, _, ok := strings.Cut(script, "          branch=")
	if !ok {
		t.Fatal("missing proposal branch boundary")
	}
	for _, test := range []struct {
		name, previous string
		changed        bool
	}{
		{"first formula", "", true},
		{"new release", "old release\n", true},
		{"already published", "https://example.invalid/releases/download/v1.2.3/\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			tap := filepath.Join(directory, "homebrew-tap")
			generated := filepath.Join(directory, "generated-formula")
			for _, path := range []string{filepath.Join(tap, "Formula"), generated} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			environment := []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "RELEASE_TAG=v1.2.3"}
			runGit := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "git", args...)
				cmd.Dir, cmd.Env = tap, environment
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, output)
				}
				return string(output)
			}
			runGit("init", "--quiet")
			if test.previous != "" {
				if err := os.WriteFile(filepath.Join(tap, "Formula", "jobman-control.rb"), []byte(test.previous), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit("add", "Formula/jobman-control.rb")
			}
			runGit("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "initial fixture")
			if err := os.WriteFile(filepath.Join(generated, "jobman-control.rb"), []byte("https://example.invalid/releases/download/v1.2.3/\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-eu", "-o", "pipefail", "-c", decision+"\nprintf 'proposal-needed\n'\n")
			cmd.Dir, cmd.Env = directory, environment
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("publication decision: %v: %s", err, output)
			}
			if changed := strings.Contains(string(output), "proposal-needed"); changed != test.changed {
				t.Fatalf("proposal needed = %t, want %t; output: %s", changed, test.changed, output)
			}
			staged := strings.TrimSpace(runGit("diff", "--cached", "--name-only"))
			if test.changed && staged != "Formula/jobman-control.rb" {
				t.Fatalf("staged files = %q", staged)
			}
			if !test.changed && staged != "" {
				t.Fatalf("unchanged formula staged as %q", staged)
			}
		})
	}
}

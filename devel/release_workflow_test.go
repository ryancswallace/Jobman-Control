package devel_test

import (
	"os"
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

	const identity = "https://github.com/ryancswallace/jobman-control/.github/workflows/release.yml@refs/heads/main"
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
	}

	for _, path := range []string{
		"../.github/workflows/publish-homebrew-formula.yml",
		"publish-cloudsmith-packages.sh",
	} {
		contents := readReleaseFile(t, path)
		if strings.Contains(contents, "--source-ref \"refs/tags/") {
			t.Errorf("%s still verifies tag-triggered attestations", path)
		}
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

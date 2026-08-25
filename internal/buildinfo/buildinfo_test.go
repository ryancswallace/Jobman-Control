package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolveMetadata(t *testing.T) {
	t.Parallel()
	defaults := metadata{version: developmentVersion, commit: unknownMetadata, date: unknownMetadata}
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.time", Value: "2026-08-23T12:00:00Z"},
		},
	}
	want := metadata{
		version: "1.2.3", commit: "0123456789abcdef", date: "2026-08-23T12:00:00Z",
	}
	if got := resolveMetadata(defaults, info, true); got != want {
		t.Fatalf("resolveMetadata() = %+v, want %+v", got, want)
	}
	if got := resolveMetadata(defaults, nil, false); got != defaults {
		t.Fatalf("resolveMetadata(unavailable) = %+v, want %+v", got, defaults)
	}
}

func TestVersionFormatting(t *testing.T) {
	t.Parallel()
	if got := moduleVersion("v1.2.3"); got != "1.2.3" {
		t.Fatalf("moduleVersion() = %q", got)
	}
	if got := moduleVersion("(devel)"); got != "" {
		t.Fatalf("moduleVersion(devel) = %q", got)
	}
	if got := modifiedVersion("1.2.3+portable"); got != "1.2.3+portable.dirty" {
		t.Fatalf("modifiedVersion() = %q", got)
	}
	if got := formatDisplay("1.2.3", "0123456789abcdef"); got != "1.2.3 (0123456789ab)" {
		t.Fatalf("formatDisplay() = %q", got)
	}
}

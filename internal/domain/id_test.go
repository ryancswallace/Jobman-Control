package domain

import (
	"regexp"
	"testing"
)

func TestNewIDReturnsDistinctVersion4UUIDs(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	first, err := NewID()
	if err != nil {
		t.Fatalf("NewID() first error = %v", err)
	}
	second, err := NewID()
	if err != nil {
		t.Fatalf("NewID() second error = %v", err)
	}
	if !pattern.MatchString(first) || !pattern.MatchString(second) {
		t.Fatalf("NewID() returned %q and %q", first, second)
	}
	if first == second {
		t.Fatalf("NewID() returned duplicate %q", first)
	}
}

func TestIsID(t *testing.T) {
	t.Parallel()
	if !IsID("6ba7b810-9dad-11d1-80b4-00c04fd430c8") {
		t.Fatal("IsID() rejected a UUID")
	}
	if IsID("6BA7B810-9DAD-11D1-80B4-00C04FD430C8") || IsID("not-a-uuid") {
		t.Fatal("IsID() accepted a noncanonical identifier")
	}
}

func TestValidIdempotencyKey(t *testing.T) {
	t.Parallel()
	if !ValidIdempotencyKey("submission-001") {
		t.Fatal("ValidIdempotencyKey() rejected a normal key")
	}
	for _, invalid := range []string{"", "contains space", "contains\nnewline", string(make([]byte, 201))} {
		if ValidIdempotencyKey(invalid) {
			t.Fatalf("ValidIdempotencyKey(%q) = true", invalid)
		}
	}
}

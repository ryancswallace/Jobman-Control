package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicDelegationRegistryBoundaries(t *testing.T) {
	t.Parallel()
	for _, document := range []string{`{}`, `{"services":null}`, `{"services":[],"privateKey":"never-accepted"}`, `{"services":[]} {}`, `not-json`, strings.Repeat(" ", 1024*1024+1)} {
		if _, err := decodeDelegationKeys(strings.NewReader(document)); err == nil {
			t.Fatal("invalid public registry accepted")
		}
	}
	keys, err := decodeDelegationKeys(strings.NewReader(`{"services":[]}`))
	if err != nil || keys == nil || len(keys) != 0 {
		t.Fatalf("explicit disable-all registry=%v,%v", keys, err)
	}
	path := filepath.Join(t.TempDir(), "public-registry.json")
	if err = os.WriteFile(path, []byte(`{"services":[{"serviceId":"dashboard","keyId":"key-1","enabled":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err = LoadDelegationKeys(path)
	if err != nil || len(keys) != 1 || keys[0].ServiceID != "dashboard" {
		t.Fatalf("public file=%v,%v", keys, err)
	}
	if _, err = LoadDelegationKeys(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing public file accepted")
	}
}

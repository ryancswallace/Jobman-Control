package postgres

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestMonitoringCursorCanonicalBounds(t *testing.T) {
	t.Parallel()
	store := New(nil, []byte("0123456789abcdef0123456789abcdef"))
	cursor := monitoringCursor{Instance: "79000000-0000-4000-8000-000000000001", Epoch: 9007199254740993, Position: 9007199254740993, Scope: strings.Repeat("a", 43)}
	encoded := store.encodeMonitoringCursor(cursor)
	actual, err := store.decodeMonitoringCursor(encoded)
	if err != nil || actual != cursor {
		t.Fatalf("cursor=%#v,%v", actual, err)
	}
	for _, value := range []string{"", strings.Repeat("a", 1025), "bad", base64.RawURLEncoding.EncodeToString([]byte(`{"instance":"79000000-0000-4000-8000-000000000001","epoch":"1","scope":"` + cursor.Scope + `","position":"1","position":"2"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{}`))} {
		if _, err = store.decodeMonitoringCursor(value); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
	other := cursor
	other.Position = -1
	if _, err = store.decodeMonitoringCursor(store.encodeMonitoringCursor(other)); err == nil {
		t.Fatal("negative position")
	}
	// Even authenticated malformed or noncanonical JSON must remain invalid.
	for _, raw := range []string{
		`{"instance":"79000000-0000-4000-8000-000000000001","epoch":"1","scope":"` + cursor.Scope + `","position":"1","position":"2"}`,
		`{"instance":"79000000-0000-4000-8000-000000000001","epoch":"1","scope":"` + cursor.Scope + `","position":"1","extra":true}`,
		`{}`,
		`null`,
		`invalid`,
	} {
		value := base64.RawURLEncoding.EncodeToString([]byte(raw)) + "." + base64.RawURLEncoding.EncodeToString(store.monitoringCursorMAC([]byte(raw)))
		if _, err = store.decodeMonitoringCursor(value); !errors.Is(err, domain.ErrEventCursorInvalid) {
			t.Fatal("authenticated invalid JSON accepted")
		}
	}
	actor := domain.Principal{Delegation: &domain.DelegatedActor{ServiceID: "a", NamespaceIDs: []string{"b", "a"}}}
	scope := monitoringScope(actor)
	actor.Delegation.NamespaceIDs = []string{"a", "b"}
	if monitoringScope(actor) != scope {
		t.Fatal("scope depends on order")
	}
	actor.Delegation.ServiceID = "b"
	if monitoringScope(actor) == scope {
		t.Fatal("scope not bound to service")
	}
}

func TestMonitoringCursorAuthenticationAndKeyRotation(t *testing.T) {
	t.Parallel()
	store := New(nil, []byte("0123456789abcdef0123456789abcdef"))
	cursor := monitoringCursor{Instance: "79000000-0000-4000-8000-000000000001", Epoch: 1, Position: 7, Scope: strings.Repeat("a", 43)}
	encoded := store.encodeMonitoringCursor(cursor)
	payload, signature, _ := strings.Cut(encoded, ".")
	for _, field := range []string{"instance", "epoch", "scope", "position"} {
		t.Run(field, func(t *testing.T) {
			changed := cursor
			switch field {
			case "instance":
				changed.Instance = "79000000-0000-4000-8000-000000000002"
			case "epoch":
				changed.Epoch++
			case "scope":
				changed.Scope = strings.Repeat("b", 43)
			case "position":
				changed.Position++
			}
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			value := base64.RawURLEncoding.EncodeToString(raw) + "." + signature
			if _, err = store.decodeMonitoringCursor(value); !errors.Is(err, domain.ErrEventCursorInvalid) {
				t.Fatal("modified canonical cursor accepted without new signature")
			}
		})
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	wrongDomain := hmac.New(sha256.New, store.tokenKey)
	_, _ = wrongDomain.Write(raw)
	for _, value := range []string{
		payload,
		payload + "." + base64.RawURLEncoding.EncodeToString(wrongDomain.Sum(nil)),
		payload + "." + strings.Repeat("a", 43),
		encoded + ".extra",
		encoded + "=",
	} {
		if _, err = store.decodeMonitoringCursor(value); !errors.Is(err, domain.ErrEventCursorInvalid) {
			t.Fatal("unsigned, wrong-purpose or malformed signature accepted")
		}
	}
	replica := New(nil, store.tokenKey)
	if actual, replicaErr := replica.decodeMonitoringCursor(encoded); replicaErr != nil || actual != cursor {
		t.Fatal("same-key replica rejected authentic cursor")
	}
	rotated := New(nil, []byte("fedcba9876543210fedcba9876543210"))
	if _, err = rotated.decodeMonitoringCursor(encoded); !errors.Is(err, domain.ErrEventCursorInvalid) {
		t.Fatal("rotated key accepted prior cursor")
	}
	if actual, rotatedErr := rotated.decodeMonitoringCursor(rotated.encodeMonitoringCursor(cursor)); rotatedErr != nil || actual != cursor {
		t.Fatal("rotated key rejected new cursor")
	}
	missing := New(nil, nil)
	if missing.encodeMonitoringCursor(cursor) != "" {
		t.Fatal("missing key signed a cursor")
	}
	if _, err = missing.decodeMonitoringCursor(encoded); !errors.Is(err, domain.ErrEventCursorInvalid) {
		t.Fatal("missing key accepted a cursor")
	}
}

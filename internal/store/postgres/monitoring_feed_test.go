package postgres

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestMonitoringCursorCanonicalBounds(t *testing.T) {
	t.Parallel()
	cursor := monitoringCursor{Instance: "79000000-0000-4000-8000-000000000001", Epoch: 9007199254740993, Position: 9007199254740993, Scope: strings.Repeat("a", 43)}
	encoded := encodeMonitoringCursor(cursor)
	actual, err := decodeMonitoringCursor(encoded)
	if err != nil || actual != cursor {
		t.Fatalf("cursor=%#v,%v", actual, err)
	}
	for _, value := range []string{"", strings.Repeat("a", 1025), "bad", base64.RawURLEncoding.EncodeToString([]byte(`{"instance":"79000000-0000-4000-8000-000000000001","epoch":"1","scope":"` + cursor.Scope + `","position":"1","position":"2"}`)), base64.RawURLEncoding.EncodeToString([]byte(`{}`))} {
		if _, err = decodeMonitoringCursor(value); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
	other := cursor
	other.Position = -1
	if _, err = decodeMonitoringCursor(encodeMonitoringCursor(other)); err == nil {
		t.Fatal("negative position")
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

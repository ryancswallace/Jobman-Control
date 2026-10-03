package directory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const (
	testUserID  = "00112233-4455-6677-8899-aabbccddeeff"
	testGroupID = "11111111-2222-4333-8444-555555555555"
)

type scriptedSearch struct {
	calls []func(*ldap.SearchRequest) (*ldap.SearchResult, error)
}

func (s *scriptedSearch) Search(request *ldap.SearchRequest) (*ldap.SearchResult, error) {
	if len(s.calls) == 0 {
		return nil, errors.New("unexpected query")
	}
	call := s.calls[0]
	s.calls = s.calls[1:]
	return call(request)
}

func searchEntry(entry *ldap.Entry) func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
	return func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		result := &ldap.SearchResult{}
		if entry != nil {
			result.Entries = []*ldap.Entry{entry}
		}
		return result, nil
	}
}

func testEntry(t *testing.T, id, dn string, attributes map[string][]string) *ldap.Entry {
	t.Helper()
	guid, err := guidBytes(id)
	if err != nil {
		t.Fatal(err)
	}
	entry := ldap.NewEntry(dn, attributes)
	entry.Attributes = append(entry.Attributes, &ldap.EntryAttribute{Name: "objectGUID", ByteValues: [][]byte{guid}, Values: []string{string(guid)}})
	return entry
}

func testConfiguration() Config {
	return Config{URL: "ldaps://dc.example.test", BaseDN: "DC=example,DC=test", BindDN: "CN=reader,DC=example,DC=test", PasswordFile: "secret", CAFile: "root.pem", Mapping: domain.DirectoryMapping{SourceID: "corporate", Revision: 1, Namespaces: []string{"22222222-2222-4222-8222-222222222222"}, ApprovedTransitions: []string{"22222222-2222-4222-8222-222222222222"}, Bindings: []domain.DirectoryBinding{{GroupID: testGroupID, NamespaceID: "22222222-2222-4222-8222-222222222222", Role: domain.RoleViewer}}, Identities: []domain.DirectoryIdentity{{DirectoryID: testUserID, PrincipalID: "33333333-3333-4333-8333-333333333333", Issuer: "https://adfs.example.test/adfs", Subject: "preserved-subject", DisplayName: "Synthetic user", Aliases: []domain.DirectoryAlias{{Issuer: "https://adfs.example.test/adfs", Subject: "dashboard-subject"}}}}}}
}

func TestCompleteDirectMembershipSnapshot(t *testing.T) {
	t.Parallel()
	config := testConfiguration()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	userDN := "CN=Alice,OU=People,DC=example,DC=test"
	groupDN := "CN=Role,OU=Groups,DC=example,DC=test"
	user := testEntry(t, testUserID, userDN, map[string][]string{"uSNChanged": {"8"}, "userAccountControl": {"512"}, "accountExpires": {"0"}})
	first := testEntry(t, testGroupID, groupDN, map[string][]string{"uSNChanged": {"42"}, "member;range=0-1": {"cn=ALICE,ou=People,dc=example,dc=test", "CN=Nested,OU=Groups,DC=example,DC=test"}})
	last := testEntry(t, testGroupID, groupDN, map[string][]string{"uSNChanged": {"42"}, "member;range=2-*": {"CN=Unmapped,OU=People,DC=example,DC=test"}})
	final := testEntry(t, testGroupID, groupDN, map[string][]string{"uSNChanged": {"42"}})
	connection := &scriptedSearch{calls: []func(*ldap.SearchRequest) (*ldap.SearchResult, error){searchEntry(user), searchEntry(first), func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		if r.Scope != ldap.ScopeBaseObject || !slices.Contains(r.Attributes, "member;range=2-1001") {
			t.Fatalf("unbounded/wrong range query=%#v", r)
		}
		return searchEntry(last)(r)
	}, searchEntry(final), searchEntry(user)}}
	snapshot, err := readSnapshot(t.Context(), connection, config, 3, now)
	if err != nil || len(connection.calls) != 0 || snapshot.RecoveryEpoch != 3 || snapshot.IgnoredDirectMembers != 2 || !snapshot.Accounts[0].Enabled || !slices.Equal(snapshot.Groups[0].DirectoryIDs, []string{testUserID}) || !snapshot.VerifiedAt.Equal(now) {
		t.Fatalf("complete direct snapshot=%#v,%v", snapshot, err)
	}
	// Nested groups and unmapped/foreign users never trigger recursive queries.
	for _, changed := range []*ldap.Entry{
		testEntry(t, testUserID, userDN, map[string][]string{"uSNChanged": {"9"}}),
		testEntry(t, testUserID, "CN=Renamed,OU=People,DC=example,DC=test", map[string][]string{"uSNChanged": {"8"}}),
		nil,
	} {
		group := testEntry(t, testGroupID, groupDN, map[string][]string{"uSNChanged": {"42"}, "member": {userDN}})
		changing := &scriptedSearch{calls: []func(*ldap.SearchRequest) (*ldap.SearchResult, error){searchEntry(user), searchEntry(group), searchEntry(final), searchEntry(changed)}}
		if _, readErr := readSnapshot(t.Context(), changing, config, 3, now); readErr == nil {
			t.Fatal("account change during group reads accepted")
		}
	}
}

func TestDirectoryGroupCompleteness(t *testing.T) {
	t.Parallel()
	dn := "CN=Role,DC=example,DC=test"
	for _, test := range []struct {
		name    string
		entries []*ldap.Entry
		fail    bool
	}{
		{"deleted", []*ldap.Entry{nil}, false},
		{"empty", []*ldap.Entry{testEntry(t, testGroupID, dn, map[string][]string{"uSNChanged": {"1"}}), testEntry(t, testGroupID, dn, map[string][]string{"uSNChanged": {"1"}})}, false},
		{"changed", []*ldap.Entry{testEntry(t, testGroupID, dn, map[string][]string{"uSNChanged": {"1"}, "member;range=0-0": {"CN=A,DC=example,DC=test"}}), testEntry(t, testGroupID, dn, map[string][]string{"uSNChanged": {"2"}, "member;range=1-*": {}})}, true},
		{"missing-range", []*ldap.Entry{testEntry(t, testGroupID, dn, map[string][]string{"uSNChanged": {"1"}, "member;range=0-0": {"CN=A,DC=example,DC=test"}}), testEntry(t, testGroupID, dn, map[string][]string{"uSNChanged": {"1"}})}, true},
		{"deleted-final", []*ldap.Entry{testEntry(t, testGroupID, dn, map[string][]string{"uSNChanged": {"1"}}), nil}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &scriptedSearch{}
			for _, entry := range test.entries {
				connection.calls = append(connection.calls, searchEntry(entry))
			}
			_, _, err := readGroup(connection, "DC=example,DC=test", testGroupID)
			if (err != nil) != test.fail {
				t.Fatalf("group completeness error=%v", err)
			}
		})
	}
}

func TestIncompleteLDAPResultNeverBecomesEmpty(t *testing.T) {
	t.Parallel()
	for _, answer := range []struct {
		result *ldap.SearchResult
		err    error
	}{{nil, errors.New("sensitive distinguished name")}, {&ldap.SearchResult{Referrals: []string{"ldap://other-domain"}}, nil}, {&ldap.SearchResult{Entries: []*ldap.Entry{{}, {}}}, nil}} {
		connection := &scriptedSearch{calls: []func(*ldap.SearchRequest) (*ldap.SearchResult, error){func(*ldap.SearchRequest) (*ldap.SearchResult, error) { return answer.result, answer.err }}}
		_, err := findObject(connection, "DC=example,DC=test", testUserID, "(&(objectClass=user)", nil)
		if err == nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("unsafe incomplete LDAP result: %v", err)
		}
	}
}

func TestMemberRangeValidation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"member;range=1-2", "member;range=0-8", "member;range=x-*", "member;range=0--1"} {
		entry := ldap.NewEntry("", map[string][]string{name: {"one", "two"}})
		if _, _, err := memberRange(entry, 0); err == nil {
			t.Errorf("accepted malformed range %s", name)
		}
	}
	entry := ldap.NewEntry("", map[string][]string{"member": {}, "member;range=0-*": {"one"}})
	values, last, err := memberRange(entry, 0)
	if err != nil || !last || len(values) != 1 {
		t.Fatalf("AD empty plain attribute plus terminal range=%v,%v,%v", values, last, err)
	}
}

func TestDirectoryEligibilityAndGUID(t *testing.T) {
	t.Parallel()
	bytes, err := guidBytes(testUserID)
	if err != nil || hex.EncodeToString(bytes) != "33221100554477668899aabbccddeeff" {
		t.Fatalf("AD GUID byte order=%x,%v", bytes, err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		flags, expires string
		want           bool
		bad            bool
	}{{"512", "0", true, false}, {"514", "0", false, false}, {"512", "9223372036854775807", true, false}, {"512", fmt.Sprint((now.Unix() + 11644473600 - 1) * 10000000), false, false}, {"512", fmt.Sprint((now.Unix() + 11644473600 + 1) * 10000000), true, false}, {"", "0", false, true}, {"512", "", false, true}} {
		entry := ldap.NewEntry("", map[string][]string{"userAccountControl": {test.flags}, "accountExpires": {test.expires}})
		got, readErr := eligible(entry, now)
		if got != test.want || (readErr != nil) != test.bad {
			t.Fatalf("eligibility=%v,%v for%#v", got, readErr, test)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = readSnapshot(ctx, &scriptedSearch{}, testConfiguration(), 1, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestDirectoryConfiguration(t *testing.T) {
	t.Parallel()
	config := testConfiguration()
	if err := Validate(config); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"ldap://dc.example.test", "ldaps://user:password@dc.example.test", "ldaps://dc.example.test/?x=1", "ldaps://dc.example.test/#fragment"} {
		invalid := config
		invalid.URL = endpoint
		if Validate(invalid) == nil {
			t.Errorf("accepted unsafe endpoint %s", endpoint)
		}
	}
	duplicate := config
	duplicate.Mapping.Bindings = append(slices.Clone(config.Mapping.Bindings), config.Mapping.Bindings[0])
	if Validate(duplicate) == nil {
		t.Fatal("accepted one group in multiple roles")
	}
	duplicate = config
	duplicate.Mapping.Identities = append(slices.Clone(config.Mapping.Identities), config.Mapping.Identities[0])
	if Validate(duplicate) == nil {
		t.Fatal("accepted duplicate identity")
	}
	path := filepath.Join(t.TempDir(), "directory.json")
	if err := os.WriteFile(path, []byte(`{"privateKey":"rejected"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted unknown config field")
	}
}

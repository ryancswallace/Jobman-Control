package directory

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const maximumDirectMembers = 100000

type searcher interface {
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
}

// Reader reads one complete source snapshot from a single verified directory.
type Reader struct{ Config Config }

// Read establishes authenticated LDAPS with cancellation and bounded read time.
// Raw LDAP errors are not returned because they can contain DNs and credentials.
func (reader Reader) Read(ctx context.Context, epoch int64) (domain.DirectorySnapshot, error) {
	if err := Validate(reader.Config); err != nil {
		return domain.DirectorySnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	started := time.Now().UTC()
	tlsConfig, password, err := credentials(reader.Config)
	if err != nil {
		return domain.DirectorySnapshot{}, err
	}
	endpoint, err := url.Parse(reader.Config.URL)
	if err != nil {
		return domain.DirectorySnapshot{}, errors.New("directory endpoint is invalid")
	}
	port := endpoint.Port()
	if port == "" {
		port = "636"
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: tlsConfig}
	transport, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Hostname(), port))
	if err != nil {
		return domain.DirectorySnapshot{}, errors.New("directory TLS connection failed")
	}
	connection := ldap.NewConn(&boundedLDAPConnection{Conn: transport, remaining: maximumDirectoryBytes}, true)
	connection.Start()
	defer func() { _ = connection.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	connection.SetTimeout(5 * time.Second)
	if err = connection.Bind(reader.Config.BindDN, password); err != nil {
		return domain.DirectorySnapshot{}, errors.New("directory authenticated bind failed")
	}
	return readSnapshot(ctx, connection, reader.Config, epoch, started)
}

func readSnapshot(ctx context.Context, connection searcher, config Config, epoch int64, started time.Time) (domain.DirectorySnapshot, error) {
	digest, err := Digest(config.Mapping)
	if err != nil {
		return domain.DirectorySnapshot{}, err
	}
	result := domain.DirectorySnapshot{SourceID: config.Mapping.SourceID, Revision: config.Mapping.Revision, Digest: digest, RecoveryEpoch: epoch, VerifiedAt: started, Accounts: make([]domain.DirectoryAccountObservation, 0, len(config.Mapping.Identities)), Groups: make([]domain.DirectoryGroupObservation, 0, len(config.Mapping.Bindings))}
	usersByDN := map[string]string{}
	userVersions := map[string]string{}
	userNames := map[string]string{}
	for _, identity := range config.Mapping.Identities {
		if contextErr := ctx.Err(); contextErr != nil {
			return domain.DirectorySnapshot{}, contextErr
		}
		entry, queryErr := findObject(connection, config.BaseDN, identity.DirectoryID, "(&(objectClass=user)(objectCategory=person)", []string{"objectGUID", "uSNChanged", "userAccountControl", "accountExpires"})
		if queryErr != nil {
			return domain.DirectorySnapshot{}, queryErr
		}
		account := domain.DirectoryAccountObservation{DirectoryID: identity.DirectoryID, Exists: entry != nil}
		if entry != nil {
			version := entry.GetEqualFoldAttributeValue("uSNChanged")
			if _, versionErr := strconv.ParseUint(version, 10, 64); versionErr != nil {
				return domain.DirectorySnapshot{}, errors.New("directory account version is missing")
			}
			userVersions[identity.DirectoryID] = version
			account.Enabled, err = eligible(entry, started)
			if err != nil {
				return domain.DirectorySnapshot{}, err
			}
			dnKey, keyErr := distinguishedNameKey(entry.DN)
			if keyErr != nil {
				return domain.DirectorySnapshot{}, keyErr
			}
			if _, duplicate := usersByDN[dnKey]; duplicate {
				return domain.DirectorySnapshot{}, errors.New("directory identities share a distinguished name")
			}
			usersByDN[dnKey] = identity.DirectoryID
			userNames[identity.DirectoryID] = dnKey
		}
		result.Accounts = append(result.Accounts, account)
	}
	totalMembers := 0
	for _, binding := range config.Mapping.Bindings {
		if contextErr := ctx.Err(); contextErr != nil {
			return domain.DirectorySnapshot{}, contextErr
		}
		members, exists, readErr := readGroup(connection, config.BaseDN, binding.GroupID)
		if readErr != nil {
			return domain.DirectorySnapshot{}, readErr
		}
		totalMembers += len(members)
		if totalMembers > maximumDirectMembers {
			return domain.DirectorySnapshot{}, errors.New("directory snapshot exceeds direct membership bound")
		}
		group := domain.DirectoryGroupObservation{GroupID: binding.GroupID, Exists: exists, DirectoryIDs: []string{}}
		for _, member := range members {
			key, keyErr := distinguishedNameKey(member)
			if keyErr != nil {
				return domain.DirectorySnapshot{}, keyErr
			}
			if user := usersByDN[key]; user != "" {
				group.DirectoryIDs = append(group.DirectoryIDs, user)
			} else {
				result.IgnoredDirectMembers++
			}
		}
		slices.Sort(group.DirectoryIDs)
		group.DirectoryIDs = slices.Compact(group.DirectoryIDs)
		result.Groups = append(result.Groups, group)
	}
	// A user rename/DN reuse during membership reads must not associate another
	// user's membership with an earlier DN. Verify each object/version again.
	for _, identity := range config.Mapping.Identities {
		if contextErr := ctx.Err(); contextErr != nil {
			return domain.DirectorySnapshot{}, contextErr
		}
		final, queryErr := findObject(connection, config.BaseDN, identity.DirectoryID, "(&(objectClass=user)(objectCategory=person)", []string{"objectGUID", "uSNChanged"})
		if queryErr != nil {
			return domain.DirectorySnapshot{}, queryErr
		}
		if final == nil {
			if userVersions[identity.DirectoryID] != "" {
				return domain.DirectorySnapshot{}, errors.New("directory account changed during verification")
			}
			continue
		}
		key, keyErr := distinguishedNameKey(final.DN)
		if keyErr != nil || key != userNames[identity.DirectoryID] || final.GetEqualFoldAttributeValue("uSNChanged") != userVersions[identity.DirectoryID] {
			return domain.DirectorySnapshot{}, errors.New("directory account changed during verification")
		}
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return domain.DirectorySnapshot{}, contextErr
	}
	return result, nil
}

func findObject(connection searcher, base, id, prefix string, attributes []string) (*ldap.Entry, error) {
	guid, err := guidBytes(id)
	if err != nil {
		return nil, err
	}
	filter := prefix + "(objectGUID=" + ldap.EscapeFilter(string(guid)) + "))"
	request := ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 5, false, filter, attributes, nil)
	request.EnforceSizeLimit = true
	result, err := connection.Search(request)
	if err != nil || result == nil || len(result.Referrals) > 0 || len(result.Entries) > 1 {
		return nil, errors.New("directory object query is incomplete")
	}
	if len(result.Entries) == 0 {
		return nil, nil
	}
	entry := result.Entries[0]
	if !slices.Equal(entry.GetEqualFoldRawAttributeValue("objectGUID"), guid) {
		return nil, errors.New("directory object identity changed")
	}
	parsed, err := ldap.ParseDN(entry.DN)
	if err != nil {
		return nil, errors.New("directory object distinguished name is invalid")
	}
	root, err := ldap.ParseDN(base)
	if err != nil || (!root.EqualFold(parsed) && !root.AncestorOfFold(parsed)) {
		return nil, errors.New("directory object is outside configured base")
	}
	return entry, nil
}

func readGroup(connection searcher, base, id string) (values []string, exists bool, readErr error) {
	entry, err := findObject(connection, base, id, "(&(objectClass=group)", []string{"objectGUID", "uSNChanged", "member;range=0-999"})
	if err != nil {
		return nil, false, err
	}
	if entry == nil {
		return []string{}, false, nil
	}
	version := entry.GetEqualFoldAttributeValue("uSNChanged")
	if _, err = strconv.ParseUint(version, 10, 64); err != nil {
		return nil, false, errors.New("directory group version is missing")
	}
	guid := entry.GetEqualFoldRawAttributeValue("objectGUID")
	members := []string{}
	next := 0
	for {
		values, last, rangeErr := memberRange(entry, next)
		if rangeErr != nil {
			return nil, false, rangeErr
		}
		members = append(members, values...)
		if len(members) > maximumDirectMembers {
			return nil, false, errors.New("directory group exceeds direct membership bound")
		}
		if last {
			break
		}
		next += len(values)
		request := ldap.NewSearchRequest(entry.DN, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 5, false, "(objectClass=group)", []string{"objectGUID", "uSNChanged", fmt.Sprintf("member;range=%d-%d", next, next+999)}, nil)
		request.EnforceSizeLimit = true
		result, queryErr := connection.Search(request)
		if queryErr != nil || result == nil || len(result.Referrals) > 0 || len(result.Entries) != 1 {
			return nil, false, errors.New("directory group range query is incomplete")
		}
		entry = result.Entries[0]
		if entry.GetEqualFoldAttributeValue("uSNChanged") != version || !slices.Equal(entry.GetEqualFoldRawAttributeValue("objectGUID"), guid) {
			return nil, false, errors.New("directory group changed during range retrieval")
		}
	}
	// Verify the same group after every range; a partial changing snapshot must
	// never revoke grants or refresh authorization evidence.
	final, err := findObject(connection, base, id, "(&(objectClass=group)", []string{"objectGUID", "uSNChanged"})
	if err != nil || final == nil || final.GetEqualFoldAttributeValue("uSNChanged") != version {
		return nil, false, errors.New("directory group changed during verification")
	}
	return members, true, nil
}

func memberRange(entry *ldap.Entry, start int) (values []string, terminal bool, readErr error) {
	var found *ldap.EntryAttribute
	for _, attribute := range entry.Attributes {
		name := strings.ToLower(attribute.Name)
		if name == "member" || strings.HasPrefix(name, "member;range=") {
			// AD can return an empty plain member attribute alongside a range.
			if name == "member" && len(attribute.Values) == 0 {
				continue
			}
			if found != nil {
				return nil, false, errors.New("directory returned ambiguous member ranges")
			}
			found = attribute
		}
	}
	if found == nil {
		if start == 0 {
			return []string{}, true, nil
		}
		return nil, false, errors.New("directory member range is missing")
	}
	name := strings.ToLower(found.Name)
	if name == "member" {
		if start != 0 {
			return nil, false, errors.New("directory member range restarted")
		}
		return found.Values, true, nil
	}
	parts := strings.Split(strings.TrimPrefix(name, "member;range="), "-")
	if len(parts) != 2 {
		return nil, false, errors.New("directory member range is malformed")
	}
	first, err := strconv.Atoi(parts[0])
	if err != nil || first != start {
		return nil, false, errors.New("directory member range has a gap")
	}
	if parts[1] == "*" {
		return found.Values, true, nil
	}
	last, err := strconv.Atoi(parts[1])
	if err != nil || last < first || last-first+1 != len(found.Values) || len(found.Values) == 0 {
		return nil, false, errors.New("directory member range is incomplete")
	}
	return found.Values, false, nil
}

func eligible(entry *ldap.Entry, now time.Time) (bool, error) {
	flags, err := strconv.ParseUint(entry.GetEqualFoldAttributeValue("userAccountControl"), 10, 32)
	if err != nil {
		return false, errors.New("directory account eligibility is missing")
	}
	if flags&2 != 0 {
		return false, nil
	}
	expires := entry.GetEqualFoldAttributeValue("accountExpires")
	if expires == "" {
		return false, errors.New("directory account expiry evidence is missing")
	}
	value, err := strconv.ParseUint(expires, 10, 64)
	if err != nil {
		return false, errors.New("directory account expiry is invalid")
	}
	if value == 0 || value == 0x7fffffffffffffff {
		return true, nil
	}
	// AD FILETIME counts 100ns intervals since 1601; compare without overflowing.
	seconds := value / 10000000
	if seconds < 11644473600 {
		return false, nil
	}
	unix := now.Unix()
	if unix < 0 {
		return false, errors.New("directory verification clock is invalid")
	}
	return seconds-11644473600 > uint64(unix), nil
}

func guidBytes(value string) ([]byte, error) {
	if !domain.IsID(value) {
		return nil, errors.New("directory object ID is invalid")
	}
	bytes, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil {
		return nil, errors.New("directory object ID is invalid")
	}
	binary.LittleEndian.PutUint32(bytes[:4], binary.BigEndian.Uint32(bytes[:4]))
	binary.LittleEndian.PutUint16(bytes[4:6], binary.BigEndian.Uint16(bytes[4:6]))
	binary.LittleEndian.PutUint16(bytes[6:8], binary.BigEndian.Uint16(bytes[6:8]))
	return bytes, nil
}

func distinguishedNameKey(value string) (string, error) {
	dn, err := ldap.ParseDN(value)
	if err != nil || len(dn.RDNs) == 0 {
		return "", errors.New("directory member distinguished name is invalid")
	}
	var key strings.Builder
	for _, rdn := range dn.RDNs {
		attributes := make([]string, 0, len(rdn.Attributes))
		for _, attribute := range rdn.Attributes {
			a, b := strings.ToLower(attribute.Type), strings.ToLower(attribute.Value)
			attributes = append(attributes, fmt.Sprintf("%d:%s%d:%s", len(a), a, len(b), b))
		}
		slices.Sort(attributes)
		_, _ = fmt.Fprintf(&key, "%d:", len(attributes))
		for _, attribute := range attributes {
			key.WriteString(attribute)
		}
	}
	return key.String(), nil
}

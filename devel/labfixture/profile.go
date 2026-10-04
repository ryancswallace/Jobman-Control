package main

import (
	"errors"
	"path/filepath"
)

// fixtureProfile is an immutable allowlist, not arbitrary database/listener input.
// The zero/default selector retains the original profile and generated documents.
type fixtureProfile struct {
	name, database, source, audience, baseDN, bindDN                       string
	apiPort, ldapPort, groupPrefix, deployment                             string
	apiService, apiKey, workerService, workerKey, brokerService, brokerKey string
	notificationUser                                                       int
}

func primaryProfile() fixtureProfile {
	return fixtureProfile{database: fixtureDatabase, source: fixtureSource, audience: fixtureAudience, baseDN: fixtureBaseDN, bindDN: fixtureBindDN, apiPort: "18443", ldapPort: "18636", groupPrefix: "72000000", deployment: diagnosticDeployment, apiService: "dashboard-lab", apiKey: "synthetic-lab-v1", brokerService: "dashboard-log-broker-lab", brokerKey: "synthetic-broker-v1"}
}

func secondaryProfile() fixtureProfile {
	return fixtureProfile{name: "secondary-v1", database: "jobman_dashboard_control_secondary", source: "synthetic-dashboard-lab-secondary", audience: "urn:jobman:dashboard-lab-secondary:control", baseDN: "DC=dashboard-secondary,DC=lab,DC=test", bindDN: "CN=reader,DC=dashboard-secondary,DC=lab,DC=test", apiPort: "28443", ldapPort: "28636", groupPrefix: "73000000", deployment: "72000000-0000-4000-8000-000000000002", apiService: "dashboard-api-lab-secondary", apiKey: "secondary-api-v1", workerService: "dashboard-worker-lab-secondary", workerKey: "secondary-worker-v1", brokerService: "dashboard-log-broker-lab-secondary", brokerKey: "secondary-broker-v1", notificationUser: 1}
}

func selectProfile(name string) (fixtureProfile, error) {
	switch name {
	case "", "primary":
		return primaryProfile(), nil
	case "secondary-v1":
		return secondaryProfile(), nil
	default:
		return fixtureProfile{}, errors.New("unknown synthetic fixture profile")
	}
}

func (p fixtureProfile) validate() error {
	expected, e := selectProfile(p.name)
	if e != nil || p != expected {
		return errors.New("invalid fixed fixture profile")
	}
	return nil
}
func (p fixtureProfile) secondary() bool { return p.name == "secondary-v1" }
func (p fixtureProfile) matchesDatabase(name string) bool {
	return p.validate() == nil && name == p.database
}

func (p fixtureProfile) matchesInfo(info fixtureInfo) bool {
	return p.validate() == nil && info.Profile == p.name && info.DelegationAudience == p.audience
}

func (p fixtureProfile) members(users []fixtureUser, namespace, role int) []string {
	members := []string{}
	if !p.secondary() {
		if namespace == 0 && role < 2 || namespace == 1 && role == 3 {
			members = append(members, users[0].DirectoryID)
		}
		if namespace == 0 && role == 0 {
			members = append(members, users[1].DirectoryID)
		}
		return members
	}
	if namespace == 0 && role == 0 {
		return []string{users[0].DirectoryID, users[1].DirectoryID}
	}
	if namespace == 0 && role == 1 || namespace == 1 && role == 3 {
		return []string{users[1].DirectoryID}
	}
	return members
}

func separateFixtureDirectories(root, directoryRoot string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(directoryRoot) || filepath.Clean(root) != root || filepath.Clean(directoryRoot) != directoryRoot || root == "/" || directoryRoot == "/" {
		return false
	}
	rel, e := filepath.Rel(root, directoryRoot)
	back, b := filepath.Rel(directoryRoot, root)
	return e == nil && b == nil && rel != "." && back != "." && isOutside(rel) && isOutside(back)
}

func isOutside(relative string) bool {
	return relative == ".." || len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)
}

package main

import (
	"fmt"
	"path/filepath"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
)

func directoryProfile(root string, profile fixtureProfile, input fixtureInput, info fixtureInfo) (directory.Config, fixtureState) {
	config := directory.Config{URL: "ldaps://127.0.0.1:" + profile.ldapPort, BaseDN: profile.baseDN, BindDN: profile.bindDN, PasswordFile: filepath.Join(root, "directory-password"), CAFile: filepath.Join(root, "fixture-ca.crt"), Mapping: domain.DirectoryMapping{SourceID: profile.source, Revision: 1, Identities: info.Identities}}
	state := fixtureState{Revision: 1}
	for _, user := range input.Users {
		state.Users = append(state.Users, stateUser{DirectoryID: user.DirectoryID, Enabled: true})
	}
	roles := []string{domain.RoleViewer, domain.RoleSubmitter, domain.RoleOperator, domain.RoleNamespaceAdmin}
	for index, namespace := range info.Namespaces {
		config.Mapping.Namespaces = append(config.Mapping.Namespaces, namespace.ID)
		config.Mapping.ApprovedTransitions = append(config.Mapping.ApprovedTransitions, namespace.ID)
		for roleIndex, role := range roles {
			id := fmt.Sprintf("%s-0000-4000-8000-%012d", profile.groupPrefix, index*4+roleIndex+1)
			config.Mapping.Bindings = append(config.Mapping.Bindings, domain.DirectoryBinding{GroupID: id, NamespaceID: namespace.ID, Role: role})
			members := profile.members(input.Users, index, roleIndex)
			state.Groups = append(state.Groups, stateGroup{ID: id, Members: members})
		}
	}
	return config, state
}

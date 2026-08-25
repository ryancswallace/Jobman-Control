package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	portableNamePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$`)
	digestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	logStorePattern        = regexp.MustCompile(`^[a-z]([a-z0-9._-]{0,62}[a-z0-9])?$`)
	awsRegionPattern       = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z0-9-]+-\d+$`)
	parallelClusterPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,59}$`)
)

// ValidName reports whether value is a portable Jobman resource name.
func ValidName(value string) bool {
	return portableNamePattern.MatchString(value)
}

// ValidateEnrollmentRequest verifies the one-time token binding.
func ValidateEnrollmentRequest(value EnrollmentRequest) error {
	if value.Principal.Issuer == "" || value.Principal.Subject == "" || value.ExpectedUser == "" {
		return errors.New("enrollment principal and expected user are required")
	}
	if len(value.Principal.Issuer) > 512 || len(value.Principal.Subject) > 512 ||
		len(value.ExpectedUser) > 512 || strings.ContainsRune(value.ExpectedUser, 0) {
		return errors.New("enrollment identity field is invalid")
	}
	if value.Lifetime < time.Minute || value.Lifetime > time.Hour {
		return errors.New("enrollment lifetime must be between one minute and one hour")
	}

	return nil
}

// ValidateAgentRegistration verifies bounded enrollment facts.
func ValidateAgentRegistration(value AgentRegistration) error {
	if !IsID(value.TargetGenerationID) {
		return errors.New("agent registration identity is invalid")
	}
	if len(value.ProtocolVersions) == 0 || len(value.ProtocolVersions) > 32 ||
		!slices.IsSorted(value.ProtocolVersions) {
		return errors.New("protocol versions must be bounded and sorted")
	}
	for index, version := range value.ProtocolVersions {
		if version == "" || len(version) > 128 || (index > 0 && version == value.ProtocolVersions[index-1]) {
			return errors.New("protocol version is invalid or duplicated")
		}
	}
	if !slices.Contains(value.ProtocolVersions, "jobman/v1alpha1") {
		return errors.New("agent does not support jobman/v1alpha1")
	}
	if !digestPattern.MatchString(value.RequestDigest) {
		return errors.New("agent registration digest is invalid")
	}

	return validateAgentFacts(
		value.AgentVersion, value.OperatingSystem, value.Architecture,
		value.Hostname, value.ExecutionUser, value.ExecutionBackends,
		value.Runtimes, value.Capabilities,
	)
}

// ValidateAgentCapabilities verifies one bounded target-side capability
// observation before it crosses the repository boundary.
func ValidateAgentCapabilities(value AgentCapabilities) error {
	if !IsID(value.AgentID) || value.ObservedAt.IsZero() || value.ObservedAt.Location() != time.UTC ||
		!digestPattern.MatchString(value.DocumentDigest) {
		return errors.New("agent capability identity is invalid")
	}

	return validateAgentFacts(
		value.AgentVersion, value.OperatingSystem, value.Architecture,
		value.Hostname, value.ExecutionUser, value.ExecutionBackends,
		value.Runtimes, value.Capabilities,
	)
}

func validateAgentFacts(
	agentVersion, operatingSystem, architecture, hostname, executionUser string,
	executionBackends, runtimes, capabilities []string,
) error {
	if agentVersion == "" || hostname == "" || executionUser == "" ||
		!ValidName(operatingSystem) || !ValidName(architecture) {
		return errors.New("agent capability facts are invalid")
	}
	if len(agentVersion) > 128 || len(hostname) > 255 || len(executionUser) > 512 {
		return errors.New("agent capability field is too long")
	}
	sets := []struct {
		name    string
		values  []string
		allowed []string
	}{
		{name: "execution backend", values: executionBackends, allowed: []string{"subprocess", "slurm"}},
		{name: "runtime", values: runtimes, allowed: []string{"native", "container"}},
		{name: "capability", values: capabilities},
	}
	for _, set := range sets {
		required := set.name != "capability"
		if err := validateSortedSet(set.name, set.values, set.allowed, required); err != nil {
			return err
		}
	}

	return nil
}

// ValidateTargetStateChange verifies an optimistic target lifecycle request.
func ValidateTargetStateChange(value TargetStateChange) error {
	if value.ExpectedRevision < 1 {
		return errors.New("target state change revision must be positive")
	}
	if !slices.Contains([]string{"active", "draining", "disabled", "retired"}, value.State) {
		return errors.New("target state is invalid")
	}

	return nil
}

// ValidateTargetGenerationChange verifies optimistic generation rollover.
func ValidateTargetGenerationChange(value TargetGenerationChange) error {
	if value.ExpectedRevision < 1 {
		return errors.New("target generation revision must be positive")
	}

	return ValidateTargetSpec(value.Spec)
}

// ValidRole reports whether value is a built-in namespace role.
func ValidRole(value string) bool {
	return slices.Contains([]string{RoleViewer, RoleSubmitter, RoleOperator, RoleNamespaceAdmin}, value)
}

// ValidateTargetSpec verifies administrator target policy before persistence.
func ValidateTargetSpec(value TargetSpec) error {
	if !ValidName(value.Name) {
		return errors.New("target name is invalid")
	}
	if !slices.Contains([]string{"host", "slurm"}, value.Kind) {
		return errors.New("target kind is invalid")
	}
	if !slices.Contains([]string{"subprocess", "slurm"}, value.ExecutionBackend) {
		return errors.New("execution backend is invalid")
	}
	if (value.Kind == "host") != (value.ExecutionBackend == "subprocess") {
		return errors.New("target kind and execution backend are incompatible")
	}
	provider := value.Provider
	if provider.Kind == "" {
		provider.Kind = "on-prem"
	}
	switch provider.Kind {
	case "on-prem":
		if provider.Region != "" || provider.ClusterName != "" {
			return errors.New("on-prem target provider cannot contain AWS settings")
		}
	case "aws-parallelcluster":
		if value.Kind != "slurm" || value.ExecutionBackend != "slurm" ||
			!awsRegionPattern.MatchString(provider.Region) ||
			!parallelClusterPattern.MatchString(provider.ClusterName) {
			return errors.New("AWS ParallelCluster provider settings are invalid")
		}
	default:
		return errors.New("target provider is unsupported")
	}
	if err := validateSortedSet("runtime", value.Runtimes, []string{"native", "container"}, true); err != nil {
		return err
	}
	if err := validateSortedSet("operating system", value.OperatingSystems, nil, false); err != nil {
		return err
	}
	if err := validateSortedSet("architecture", value.Architectures, nil, false); err != nil {
		return err
	}
	if err := validateSortedSet("capability", value.Capabilities, nil, false); err != nil {
		return err
	}
	if (value.LogStoreName == "") != (value.LogStoreVersion == 0) {
		return errors.New("log store name and version must be configured together")
	}
	if value.LogStoreName != "" &&
		(!logStorePattern.MatchString(value.LogStoreName) || value.LogStoreVersion < 1) {
		return errors.New("log store mapping is invalid")
	}
	if len(value.ArtifactStores) > 64 || !slices.IsSortedFunc(
		value.ArtifactStores,
		func(left, right ArtifactStoreSpec) int { return strings.Compare(left.Name, right.Name) },
	) {
		return errors.New("artifact store mappings must be bounded and sorted")
	}
	for index, store := range value.ArtifactStores {
		if !logStorePattern.MatchString(store.Name) || store.Version < 1 ||
			(index > 0 && store.Name == value.ArtifactStores[index-1].Name) {
			return errors.New("artifact store mapping is invalid or duplicated")
		}
	}
	if value.Kind == "host" && len(value.Partitions) != 0 {
		return errors.New("host targets cannot contain partitions")
	}
	seen := make(map[string]struct{}, len(value.Partitions))
	defaultCount := 0
	for _, partition := range value.Partitions {
		if !ValidName(partition.Name) {
			return fmt.Errorf("partition name %q is invalid", partition.Name)
		}
		if _, exists := seen[partition.Name]; exists {
			return fmt.Errorf("partition %q is duplicated", partition.Name)
		}
		seen[partition.Name] = struct{}{}
		if partition.IsDefault {
			defaultCount++
		}
	}
	if defaultCount > 1 {
		return errors.New("target has more than one default partition")
	}

	return nil
}

// NormalizeTargetProvider supplies the compatibility default used by targets
// created before provider metadata was introduced.
func NormalizeTargetProvider(value TargetProvider) TargetProvider {
	if value.Kind == "" {
		value.Kind = "on-prem"
	}

	return value
}

func validateSortedSet(field string, values, allowed []string, required bool) error {
	if required && len(values) == 0 {
		return fmt.Errorf("at least one %s is required", field)
	}
	if len(values) > 1024 || !slices.IsSorted(values) {
		return fmt.Errorf("%ss must be bounded and sorted", field)
	}
	for index, value := range values {
		if !ValidName(value) {
			return fmt.Errorf("%s %q is invalid", field, value)
		}
		if index > 0 && value == values[index-1] {
			return fmt.Errorf("%s %q is duplicated", field, value)
		}
		if allowed != nil && !slices.Contains(allowed, value) {
			return fmt.Errorf("%s %q is unsupported", field, value)
		}
	}

	return nil
}

// ValidateMembershipGrant verifies stable identity and role fields.
func ValidateMembershipGrant(value MembershipGrant) error {
	if value.Issuer == "" || value.Subject == "" || value.DisplayName == "" || !ValidRole(value.Role) {
		return errors.New("membership identity and role are invalid")
	}
	for _, item := range []string{value.Issuer, value.Subject, value.DisplayName} {
		if len(item) > 512 || strings.ContainsRune(item, 0) {
			return errors.New("membership identity field is invalid")
		}
	}

	return nil
}

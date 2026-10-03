package domain

import (
	"slices"
	"time"
)

// Capabilities describe operations, never an ordering of roles. Owner-dependent
// capabilities still require the resource owner check in the repository.
const (
	CapabilityNamespaceRead       = "namespace.read"
	CapabilityJobsRead            = "jobs.read"
	CapabilityGroupsRead          = "groups.read"
	CapabilityTargetsRead         = "targets.read"
	CapabilityLogsRead            = "logs.read"
	CapabilityArtifactsRead       = "artifacts.read"
	CapabilityEvidenceRead        = "evidence.read"
	CapabilityReportsRead         = "reports.read"
	CapabilityDiagnosisRequest    = "diagnosis.request"
	CapabilityPolicyRead          = "policy.read"
	CapabilityJobsSubmit          = "jobs.submit"
	CapabilityJobsCancelOwn       = "jobs.cancel.own"
	CapabilityJobsCancelAny       = "jobs.cancel.any"
	CapabilityEnrollmentCreateOwn = "enrollment.create.own"
	CapabilityEnrollmentCreateAny = "enrollment.create.any"
	CapabilityTargetsOperate      = "targets.operate"
	CapabilityTargetsManage       = "targets.manage"
	CapabilityMembershipsManage   = "memberships.manage"
	CapabilityPolicyManage        = "policy.manage"
	CapabilityAuditRead           = "audit.read"
)

// EffectiveCapabilities returns the sorted, distinct union for known roles.
// Unknown roles confer no authority. Returned slices never alias shared state.
func EffectiveCapabilities(roles []string) []string {
	capabilities := make([]string, 0)
	for _, role := range roles {
		if !ValidRole(role) {
			continue
		}
		capabilities = append(capabilities, CapabilityNamespaceRead, CapabilityJobsRead,
			CapabilityGroupsRead, CapabilityTargetsRead, CapabilityLogsRead,
			CapabilityArtifactsRead, CapabilityEvidenceRead, CapabilityReportsRead,
			CapabilityDiagnosisRequest, CapabilityPolicyRead, CapabilityJobsCancelOwn)
		if role == RoleSubmitter || role == RoleOperator || role == RoleNamespaceAdmin {
			capabilities = append(capabilities, CapabilityJobsSubmit, CapabilityEnrollmentCreateOwn)
		}
		if role == RoleOperator || role == RoleNamespaceAdmin {
			capabilities = append(capabilities, CapabilityJobsCancelAny, CapabilityTargetsOperate, CapabilityAuditRead)
		}
		if role == RoleNamespaceAdmin {
			capabilities = append(capabilities, CapabilityEnrollmentCreateAny, CapabilityTargetsManage,
				CapabilityMembershipsManage, CapabilityPolicyManage)
		}
	}
	slices.Sort(capabilities)
	return slices.Compact(capabilities)
}

// NamespaceAccess is a current authorization snapshot, not a reusable grant.
type NamespaceAccess struct {
	AuthorizationCheckedAt  *time.Time `json:"authorizationCheckedAt,omitempty"`
	LastDirectoryVerifiedAt *time.Time `json:"lastDirectoryVerifiedAt,omitempty"`
	AuthorizationExpiresAt  *time.Time `json:"authorizationExpiresAt,omitempty"`
	AuthorizationStatus     string     `json:"authorizationStatus,omitempty"`
	ID                      string     `json:"id"`
	Name                    string     `json:"name"`
	Roles                   []string   `json:"roles"`
	Capabilities            []string   `json:"capabilities"`
	AuthorizationVersion    string     `json:"authorizationVersion"`
}

// PrincipalAccess is bounded current-principal discovery.
type PrincipalAccess struct {
	DirectoryID            string
	PrincipalID            string
	Principal              Principal
	DisplayName            string
	AuthorizationCheckedAt time.Time
	Namespaces             []NamespaceAccess
	NextNamespaceID        string
}

// ContributingGrant is an independently revocable namespace role contribution.
// Legacy grants are maintained by the original membership PUT operation.
type ContributingGrant struct {
	ID          string     `json:"id"`
	Namespace   string     `json:"namespace"`
	PrincipalID string     `json:"principalId"`
	Issuer      string     `json:"issuer"`
	Subject     string     `json:"subject"`
	DisplayName string     `json:"displayName"`
	Role        string     `json:"role"`
	Provenance  string     `json:"provenance"`
	CreatedAt   time.Time  `json:"createdAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

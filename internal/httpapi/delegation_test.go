package httpapi

import (
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestDelegationRouteAllowlist(t *testing.T) {
	t.Parallel()
	for route, capability := range map[string]string{
		"GET /v1/me":                                                   domain.CapabilityNamespaceRead,
		"GET /v1/namespaces/{namespace}/summary":                       domain.CapabilityJobsRead,
		"GET /v1/namespaces/{namespace}/jobs/{jobID}":                  domain.CapabilityJobsRead,
		"GET /v1/namespaces/{namespace}/graphs/{graphID}/neighborhood": domain.CapabilityGroupsRead,
		"GET /v1/namespaces/{namespace}/jobs/{jobID}/logs":             domain.CapabilityLogsRead,
		"GET /v1/namespaces/{namespace}/targets/{target}":              domain.CapabilityTargetsRead,
		"GET /v1/namespaces/{namespace}/jobs/{jobID}/artifacts":        domain.CapabilityArtifactsRead,
	} {
		if got := delegationRouteOperation(route); got != capability {
			t.Fatalf("%s=%s", route, got)
		}
	}
	for _, route := range []string{"POST /v1/namespaces/{namespace}/jobs", "POST /v1/namespaces/{namespace}/jobs/{jobID}/cancel", "PUT /v1/namespaces/{namespace}/memberships", "GET /v1/namespaces/{namespace}/audit", "GET /v1/namespaces/{namespace}/policy", "GET /v1/namespaces/{namespace}/jobs/{jobID}/future", "GET /v1/events", "/v1/me", "GET /v1/proxy"} {
		if got := delegationRouteOperation(route); got != "" {
			t.Fatalf("unlisted route %s=%s", route, got)
		}
	}
}

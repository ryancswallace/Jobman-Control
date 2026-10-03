package httpapi

import "github.com/ryancswallace/jobman-control/internal/domain"

// Match registered ServeMux patterns, never a client-selected capability or
// URL prefix. New routes are denied until explicitly reviewed and added here.
func delegationRouteOperation(pattern string) string {
	switch pattern {
	case "GET /v1/me":
		return domain.CapabilityNamespaceRead
	case "GET /v1/namespaces/{namespace}/summary", "GET /v1/namespaces/{namespace}/jobs", "GET /v1/namespaces/{namespace}/jobs/{jobID}":
		return domain.CapabilityJobsRead
	case "GET /v1/namespaces/{namespace}/collections", "GET /v1/namespaces/{namespace}/collections/{collectionID}", "GET /v1/namespaces/{namespace}/collections/{collectionID}/summary", "GET /v1/namespaces/{namespace}/collections/{collectionID}/items", "GET /v1/namespaces/{namespace}/graphs", "GET /v1/namespaces/{namespace}/graphs/{graphID}", "GET /v1/namespaces/{namespace}/graphs/{graphID}/summary", "GET /v1/namespaces/{namespace}/graphs/{graphID}/nodes", "GET /v1/namespaces/{namespace}/graphs/{graphID}/dependencies", "GET /v1/namespaces/{namespace}/graphs/{graphID}/neighborhood":
		return domain.CapabilityGroupsRead
	case "GET /v1/namespaces/{namespace}/targets", "GET /v1/namespaces/{namespace}/targets/{target}":
		return domain.CapabilityTargetsRead
	case "GET /v1/namespaces/{namespace}/jobs/{jobID}/logs", "GET /v1/namespaces/{namespace}/jobs/{jobID}/log-chunks":
		return domain.CapabilityLogsRead
	case "GET /v1/namespaces/{namespace}/jobs/{jobID}/artifacts", "GET /v1/namespaces/{namespace}/jobs/{jobID}/artifact-metadata":
		return domain.CapabilityArtifactsRead
	default:
		return ""
	}
}

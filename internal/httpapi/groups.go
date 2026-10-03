package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

type collectionSummaryResponse struct {
	Metadata collectionMetadata `json:"metadata"`
	Spec     collectionSpec     `json:"spec"`
	Status   collectionStatus   `json:"status"`
}
type graphSummaryResponse struct {
	Metadata graphMetadata `json:"metadata"`
	Spec     graphSpec     `json:"spec"`
	Status   graphStatus   `json:"status"`
}
type graphNodeResponse struct {
	Index            int                     `json:"index"`
	Name             string                  `json:"name"`
	Disposition      string                  `json:"disposition,omitempty"`
	DependencyCounts domain.DependencyCounts `json:"dependencyCounts"`
	Job              jobResponse             `json:"job"`
}

func collectionSummaryDocument(group domain.Collection) collectionSummaryResponse {
	document := newCollectionResponse(group)
	return collectionSummaryResponse{Metadata: document.Metadata, Spec: document.Spec, Status: document.Status}
}

func graphSummaryDocument(group domain.Graph) graphSummaryResponse {
	document := newGraphResponse(group)
	return graphSummaryResponse{Metadata: document.Metadata, Spec: document.Spec, Status: document.Status}
}

func graphNodeDocuments(nodes []domain.GraphNodeSnapshot) []graphNodeResponse {
	result := make([]graphNodeResponse, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, graphNodeResponse{Index: node.Index, Name: node.Name, Disposition: node.Disposition, DependencyCounts: node.Dependencies, Job: newJobResponse(node.Job)})
	}
	return result
}

func knownGroupQuery(query url.Values, names ...string) error {
	for name, values := range query {
		if !slices.Contains(names, name) || len(values) != 1 || values[0] == "" {
			return errors.New("group query is invalid")
		}
	}
	return nil
}

func boundedGroupInt(query url.Values, name string, fallback, minimum, maximum int) (int, error) {
	if !query.Has(name) {
		return fallback, nil
	}
	value, err := strconv.Atoi(query.Get(name))
	if err != nil || value < minimum || value > maximum {
		return 0, errors.New("group query integer is out of range")
	}
	return value, nil
}

func readGroupListOptions(request *http.Request, collections bool) (domain.GroupListOptions, error) {
	query, parseErr := url.ParseQuery(request.URL.RawQuery)
	if parseErr != nil {
		return domain.GroupListOptions{}, parseErr
	}
	names := []string{"limit", "pageToken", "createdBefore"}
	if collections {
		names = append(names, "arrayMode")
	}
	result := domain.GroupListOptions{}
	if err := knownGroupQuery(query, names...); err != nil {
		return result, err
	}
	var err error
	result.Limit, err = boundedGroupInt(query, "limit", 50, 1, 200)
	if err != nil {
		return result, err
	}
	if query.Has("pageToken") {
		cursor, decodeErr := decodeJobPageToken(query.Get("pageToken"))
		if decodeErr != nil {
			return result, decodeErr
		}
		result.Before = &cursor
	}
	if query.Has("createdBefore") {
		value, parseErr := time.Parse(time.RFC3339Nano, query.Get("createdBefore"))
		if parseErr != nil {
			return result, parseErr
		}
		value = value.UTC()
		result.CreatedBefore = &value
	}
	result.ArrayMode = query.Get("arrayMode")
	if !slices.Contains([]string{"", "individual", "slurm-array"}, result.ArrayMode) {
		return result, errors.New("array mode is invalid")
	}
	return result, nil
}

func groupQueryError(writer http.ResponseWriter) {
	writeError(writer, http.StatusBadRequest, "invalid_query", "group query or resource ID is invalid")
}

func (service *api) listCollections(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	options, err := readGroupListOptions(request, true)
	if err != nil {
		groupQueryError(writer)
		return
	}
	page, err := service.repository.ListCollections(request.Context(), principal, request.PathValue("namespace"), options)
	if err != nil {
		service.writeRepositoryError(writer, request, "list collections", err)
		return
	}
	items := make([]collectionSummaryResponse, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, collectionSummaryDocument(item))
	}
	result := map[string]any{"apiVersion": apiVersion, "kind": "CollectionList", "asOf": page.AsOf, "total": strconv.Itoa(page.Total), "items": items}
	writeGroupCatalog(writer, result, options.CreatedBefore, page.NextCursor)
}

func (service *api) listGraphs(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	options, err := readGroupListOptions(request, false)
	if err != nil {
		groupQueryError(writer)
		return
	}
	page, err := service.repository.ListGraphs(request.Context(), principal, request.PathValue("namespace"), options)
	if err != nil {
		service.writeRepositoryError(writer, request, "list graphs", err)
		return
	}
	items := make([]graphSummaryResponse, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, graphSummaryDocument(item))
	}
	result := map[string]any{"apiVersion": apiVersion, "kind": "GraphList", "asOf": page.AsOf, "total": strconv.Itoa(page.Total), "items": items}
	writeGroupCatalog(writer, result, options.CreatedBefore, page.NextCursor)
}

func writeGroupCatalog(writer http.ResponseWriter, result map[string]any, createdBefore *time.Time, cursor *domain.JobCursor) {
	if createdBefore != nil {
		result["createdBefore"] = createdBefore
	}
	if cursor != nil {
		token, err := encodeJobPageToken(*cursor)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "internal_error", "could not encode group cursor")
			return
		}
		result["nextPageToken"] = token
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *api) collectionSummary(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	id := request.PathValue("collectionID")
	if !domain.IsID(id) || request.URL.RawQuery != "" {
		groupQueryError(writer)
		return
	}
	result, err := service.repository.CollectionSummary(request.Context(), principal, request.PathValue("namespace"), id)
	if err != nil {
		service.writeRepositoryError(writer, request, "get collection summary", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": apiVersion, "kind": "CollectionSummary", "asOf": result.AsOf, "summary": collectionSummaryDocument(result.Collection)})
}

func (service *api) graphSummary(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	id := request.PathValue("graphID")
	if !domain.IsID(id) || request.URL.RawQuery != "" {
		groupQueryError(writer)
		return
	}
	result, err := service.repository.GraphSummary(request.Context(), principal, request.PathValue("namespace"), id)
	if err != nil {
		service.writeRepositoryError(writer, request, "get graph summary", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": apiVersion, "kind": "GraphSummary", "asOf": result.AsOf, "summary": graphSummaryDocument(result.Graph)})
}

func readGroupItemPage(request *http.Request) (after, limit int, err error) {
	query, parseErr := url.ParseQuery(request.URL.RawQuery)
	if parseErr != nil {
		return 0, 0, parseErr
	}
	if queryErr := knownGroupQuery(query, "limit", "afterIndex"); queryErr != nil {
		return 0, 0, queryErr
	}
	limit, err = boundedGroupInt(query, "limit", 50, 1, 200)
	if err != nil {
		return 0, 0, err
	}
	after, err = boundedGroupInt(query, "afterIndex", -1, -1, 9999)
	return after, limit, err
}

func (service *api) collectionItems(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	id := request.PathValue("collectionID")
	after, limit, err := readGroupItemPage(request)
	if !domain.IsID(id) || err != nil {
		groupQueryError(writer)
		return
	}
	page, err := service.repository.ListCollectionItems(request.Context(), principal, request.PathValue("namespace"), id, after, limit)
	if err != nil {
		service.writeRepositoryError(writer, request, "list collection items", err)
		return
	}
	items := make([]collectionItem, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, collectionItem{Index: item.Index, ArrayTaskIndex: item.ArrayTaskIndex, Name: item.Name, Job: newJobResponse(item.Job)})
	}
	result := map[string]any{"apiVersion": apiVersion, "kind": "CollectionItemList", "asOf": page.AsOf, "total": strconv.Itoa(page.Total), "items": items}
	if page.NextIndex != nil {
		result["nextAfterIndex"] = *page.NextIndex
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *api) graphNodes(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	id := request.PathValue("graphID")
	after, limit, err := readGroupItemPage(request)
	if !domain.IsID(id) || err != nil {
		groupQueryError(writer)
		return
	}
	page, err := service.repository.ListGraphNodes(request.Context(), principal, request.PathValue("namespace"), id, after, limit)
	if err != nil {
		service.writeRepositoryError(writer, request, "list graph nodes", err)
		return
	}
	result := map[string]any{"apiVersion": apiVersion, "kind": "GraphNodeList", "asOf": page.AsOf, "total": strconv.Itoa(page.Total), "items": graphNodeDocuments(page.Items)}
	if page.NextIndex != nil {
		result["nextAfterIndex"] = *page.NextIndex
	}
	writeJSON(writer, http.StatusOK, result)
}

func readGraphEdgeOptions(request *http.Request) (domain.GraphEdgeOptions, error) {
	result := domain.GraphEdgeOptions{}
	query, parseErr := url.ParseQuery(request.URL.RawQuery)
	if parseErr != nil {
		return result, parseErr
	}
	if err := knownGroupQuery(query, "limit", "nodeId", "direction", "pageToken"); err != nil {
		return result, err
	}
	var err error
	result.Limit, err = boundedGroupInt(query, "limit", 100, 1, 500)
	if err != nil {
		return result, err
	}
	result.NodeID, result.Direction = query.Get("nodeId"), query.Get("direction")
	if (result.NodeID != "" && !domain.IsID(result.NodeID)) || !slices.Contains([]string{"", "incoming", "outgoing"}, result.Direction) || (result.Direction != "" && result.NodeID == "") {
		return result, errors.New("dependency direction or node is invalid")
	}
	if query.Has("pageToken") {
		value := query.Get("pageToken")
		if len(value) > 256 {
			return result, errors.New("dependency cursor is too long")
		}
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(value)
		if decodeErr != nil {
			return result, decodeErr
		}
		parts := strings.Split(string(decoded), "/")
		if len(parts) != 2 || !domain.IsID(parts[0]) || !domain.IsID(parts[1]) {
			return result, errors.New("dependency cursor is invalid")
		}
		result.AfterFromID, result.AfterToID = parts[0], parts[1]
	}
	return result, nil
}

func (service *api) graphDependencies(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	id := request.PathValue("graphID")
	options, err := readGraphEdgeOptions(request)
	if !domain.IsID(id) || err != nil {
		groupQueryError(writer)
		return
	}
	page, err := service.repository.ListGraphEdges(request.Context(), principal, request.PathValue("namespace"), id, options)
	if err != nil {
		service.writeRepositoryError(writer, request, "list graph dependencies", err)
		return
	}
	result := map[string]any{"apiVersion": apiVersion, "kind": "GraphDependencyList", "asOf": page.AsOf, "total": strconv.Itoa(page.Total), "items": page.Items}
	if page.NextFromID != "" {
		result["nextPageToken"] = base64.RawURLEncoding.EncodeToString([]byte(page.NextFromID + "/" + page.NextToID))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *api) graphNeighborhood(writer http.ResponseWriter, request *http.Request, principal domain.Principal) {
	id := request.PathValue("graphID")
	query, parseErr := url.ParseQuery(request.URL.RawQuery)
	if parseErr != nil {
		groupQueryError(writer)
		return
	}
	nodeID := query.Get("nodeId")
	if !domain.IsID(id) || !domain.IsID(nodeID) || knownGroupQuery(query, "nodeId", "maxNodes", "maxEdges") != nil {
		groupQueryError(writer)
		return
	}
	nodes, err := boundedGroupInt(query, "maxNodes", 50, 1, 200)
	if err != nil {
		groupQueryError(writer)
		return
	}
	edges, err := boundedGroupInt(query, "maxEdges", 100, 1, 500)
	if err != nil {
		groupQueryError(writer)
		return
	}
	result, err := service.repository.GraphNeighborhood(request.Context(), principal, request.PathValue("namespace"), id, nodeID, nodes, edges)
	if err != nil {
		service.writeRepositoryError(writer, request, "get graph neighborhood", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": apiVersion, "kind": "GraphNeighborhood", "asOf": result.AsOf, "centerId": result.CenterID, "nodes": graphNodeDocuments(result.Nodes), "edges": result.Edges, "totalNodes": result.TotalNodes, "totalEdges": result.TotalEdges, "omittedNodes": result.OmittedNodes, "omittedEdges": result.OmittedEdges})
}

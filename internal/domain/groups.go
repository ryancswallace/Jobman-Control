package domain

import "time"

// GroupListOptions selects a bounded creation-ordered catalog page.
type GroupListOptions struct {
	Limit         int
	Before        *JobCursor
	CreatedBefore *time.Time
	ArrayMode     string
}

// ResourcePage is a bounded snapshot page whose counts may cover the full group.
type ResourcePage[T any] struct {
	Items      []T
	Total      int
	AsOf       time.Time
	NextCursor *JobCursor
	NextIndex  *int
}

// CollectionSnapshot excludes children from its complete aggregate summary.
type (
	CollectionSnapshot struct {
		Collection Collection
		AsOf       time.Time
	}
	// GraphSnapshot excludes nodes from its complete aggregate summary.
	GraphSnapshot struct {
		Graph Graph
		AsOf  time.Time
	}
)

// DependencyCounts describes all incoming source predicates for one node.
type DependencyCounts struct {
	Total       int `json:"total"`
	Satisfied   int `json:"satisfied"`
	Waiting     int `json:"waiting"`
	Unsatisfied int `json:"unsatisfied"`
}

// GraphNodeSnapshot is a bounded node result without unbounded dependency arrays.
type GraphNodeSnapshot struct {
	Index        int
	Name         string
	Disposition  string
	Job          Job
	Dependencies DependencyCounts
}

// GraphEdgeSnapshot contains source-computed satisfaction of an immutable edge.
type GraphEdgeSnapshot struct {
	From            string   `json:"from"`
	To              string   `json:"to"`
	FromJobID       string   `json:"fromJobId"`
	ToJobID         string   `json:"toJobId"`
	Predicate       string   `json:"predicate"`
	Outcomes        []string `json:"outcomes"`
	UpstreamPhase   string   `json:"upstreamPhase"`
	UpstreamOutcome string   `json:"upstreamOutcome,omitempty"`
	State           string   `json:"state"`
}

// GraphEdgeOptions restricts one bounded edge page, optionally around one node.
type GraphEdgeOptions struct {
	Limit       int
	NodeID      string
	Direction   string
	AfterFromID string
	AfterToID   string
}

// GraphEdgePage keeps graph identity and query snapshot separate from its cursor.
type GraphEdgePage struct {
	Items      []GraphEdgeSnapshot
	Total      int
	AsOf       time.Time
	NextFromID string
	NextToID   string
}

// GraphNeighborhood is a bounded induced view around a selected node. Full
// neighborhood counts disclose omissions without downloading all nodes/edges.
type GraphNeighborhood struct {
	CenterID     string
	Nodes        []GraphNodeSnapshot
	Edges        []GraphEdgeSnapshot
	AsOf         time.Time
	TotalNodes   int
	TotalEdges   int
	OmittedNodes int
	OmittedEdges int
}

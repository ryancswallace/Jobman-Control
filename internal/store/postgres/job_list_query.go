package postgres

// Both paths apply identical job-column filters before their final page order.
// Confidence is separate because it depends on the latest execution projection.
const jobListFilters = `
 AND ($4::text = '' OR j.phase = $4 OR ($4 = 'active' AND j.phase <> 'terminal') OR ($4 = 'awaiting' AND j.phase IN ('accepted','assigning','accepted_execution')))
 AND ($5::timestamptz IS NULL OR (j.created_at, j.id) < ($5, $6::uuid))
 AND ($8::text = '' OR j.outcome = $8)
 AND (NULLIF($9, '')::uuid IS NULL OR (NOT j.imported AND j.owner_principal_id = NULLIF($9, '')::uuid))
 AND ($10::timestamptz IS NULL OR j.completed_at >= $10)
 AND ($11::timestamptz IS NULL OR j.completed_at < $11)
 AND ($12::timestamptz IS NULL OR j.created_at <= $12)
 AND (NULLIF($13, '')::uuid IS NULL OR j.id = NULLIF($13, '')::uuid)
`

const jobListFullQuery = jobSelect + `
 JOIN authorized_memberships AS m ON m.namespace_id = n.id
 JOIN principals AS p ON p.id = m.principal_id
 WHERE p.issuer = $1 AND p.subject = $2 AND n.name = $3
` + jobListFilters + `
 AND ($14::text = '' OR current_execution.observation_confidence = $14
   OR ($14 = 'attention' AND j.phase <> 'terminal' AND current_execution.observation_confidence IN ('stale','uncertain','lost')))
 ORDER BY j.created_at DESC, j.id DESC LIMIT $7
`

// Freeze the singleton authorized namespace before touching jobs. Otherwise a
// many-namespace membership join can scan each authorized namespace and discard
// the unrelated rows only after enrichment. Choose the ordered IDs before any
// owner/target/execution joins, then materialize at most Limit+1 job records so
// the planner cannot move per-job execution probes ahead of the page boundary.
const jobListCandidateQuery = `
 WITH list_scope AS MATERIALIZED (
   SELECT n.id FROM namespaces AS n
   JOIN authorized_memberships AS m ON m.namespace_id=n.id
   JOIN principals AS p ON p.id=m.principal_id
   WHERE p.issuer=$1 AND p.subject=$2 AND n.name=$3
 ), list_candidates AS MATERIALIZED (
   SELECT j.id FROM jobs AS j
   WHERE j.namespace_id=(SELECT id FROM list_scope) AND $14::text=''
` + jobListFilters + `
   ORDER BY j.created_at DESC, j.id DESC LIMIT $7
 ), list_page AS MATERIALIZED (
   SELECT j.id, j.namespace_id, j.owner_principal_id, j.name, j.labels, j.phase,
     j.desired_state, j.outcome, j.placement_target, j.placement_partition,
     j.workload_digest, j.request_digest, j.revision, j.created_at, j.updated_at,
     j.target_id, j.target_generation_id, j.imported, j.started_at,
     j.started_recorded_at, j.started_provenance, j.completed_at,
     j.completed_recorded_at, j.completed_provenance, j.collection_id,
     j.collection_index, j.graph_id, j.graph_index, j.graph_disposition
   FROM jobs AS j JOIN list_candidates AS candidate ON candidate.id=j.id
   WHERE j.namespace_id=(SELECT id FROM list_scope)
 )
` + jobSelectColumns + ` FROM list_page AS j` + jobSelectJoins + `
 ORDER BY j.created_at DESC, j.id DESC
`

func jobListQuery(confidence string) string {
	if confidence == "" {
		return jobListCandidateQuery
	}
	return jobListFullQuery
}

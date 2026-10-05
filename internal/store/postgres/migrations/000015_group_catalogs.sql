-- Creation-key catalogs stay bounded independently of group size.
CREATE INDEX collections_namespace_creation_idx ON collections(namespace_id,created_at DESC,id DESC);
CREATE INDEX graphs_namespace_creation_idx ON graphs(namespace_id,created_at DESC,id DESC);
-- Existing primary key starts with graph/upstream. Reverse lookup is also used
-- for one-hop neighborhoods and incoming dependency summaries.
CREATE INDEX graph_edges_downstream_idx ON graph_edges(graph_id,downstream_job_id,upstream_job_id);

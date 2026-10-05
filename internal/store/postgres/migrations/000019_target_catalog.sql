CREATE INDEX targets_catalog_idx ON targets(namespace_id,created_at DESC,id DESC);
CREATE INDEX partitions_catalog_idx ON partitions(target_generation_id,name COLLATE "C") WHERE state!='retired';

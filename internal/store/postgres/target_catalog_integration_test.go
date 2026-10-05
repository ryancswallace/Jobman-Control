package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func TestTargetCatalogIntegration(t *testing.T) {
	databaseURL := os.Getenv("JOBMAN_CONTROL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("JOBMAN_CONTROL_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	pool := newIntegrationPool(ctx, t, databaseURL)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := New(pool, []byte("0123456789abcdef0123456789abcdef"))
	principal := domain.Principal{Issuer: "test-issuer", Subject: "target-catalog"}
	for _, namespace := range []string{"research", "other"} {
		if err := store.EnsureDevelopmentIdentity(ctx, domain.DevelopmentIdentity{Principal: principal, DisplayName: "Synthetic catalog reader", Namespace: namespace}); err != nil {
			t.Fatal(err)
		}
	}
	spec := domain.TargetSpec{Name: "source-host", Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}, LogStoreName: "synthetic-store", LogStoreVersion: 9007199254740993}
	created, err := store.CreateTarget(ctx, principal, "research", "catalog-host", fmt.Sprintf("sha256:%064x", 1), spec)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateTarget(ctx, principal, "other", "catalog-other", fmt.Sprintf("sha256:%064x", 2), spec)
	if err != nil {
		t.Fatal(err)
	}
	// Populate beyond the legacy 1000-row cap, sharing creation timestamps to
	// exercise the UUID tie-break. This fixture inserts only valid target facts.
	if _, err = pool.Exec(ctx, `INSERT INTO targets(id,namespace_id,name,kind) SELECT gen_random_uuid(),namespace_id,'bulk-'||number::text,'host' FROM targets CROSS JOIN generate_series(1,1004) number WHERE id=$1`, created.Value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO target_generations(id,namespace_id,target_id,generation,execution_backend,control_transport,runtimes,operating_systems,architectures,capabilities,provider)
 SELECT gen_random_uuid(),namespace_id,id,1,'subprocess','agent-api',ARRAY['native'],ARRAY['linux'],ARRAY['amd64'],ARRAY[]::text[],'{"kind":"on-prem"}'::jsonb FROM targets WHERE current_generation_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE targets t SET current_generation_id=g.id FROM target_generations g WHERE t.id=g.target_id AND t.current_generation_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	first, err := store.ListTargetCatalog(ctx, principal, "research", domain.TargetCatalogOptions{Limit: 200})
	if err != nil || first.Total != 1005 || len(first.Items) != 200 || first.NextCursor == nil || first.AsOf.IsZero() || first.NamespaceID == "" || first.AuthorizationVersion < 1 || first.RecoveryEpoch < 1 {
		t.Fatalf("first complete target catalog=%#v,%v", first, err)
	}
	spec.Name = "later-host"
	if _, err = store.CreateTarget(ctx, principal, "research", "catalog-later", fmt.Sprintf("sha256:%064x", 3), spec); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var previous *domain.JobCursor
	page := first
	for pages := 0; pages < 10; pages++ {
		if page.Total != 1005 {
			t.Fatalf("creation cutoff changed full count: %d", page.Total)
		}
		for _, item := range page.Items {
			if seen[item.ID] || item.ID == other.Value.ID || item.Name == "later-host" {
				t.Fatal("catalog repeated or leaked a target")
			}
			if previous != nil && (item.CreatedAt.After(previous.CreatedAt) || item.CreatedAt.Equal(previous.CreatedAt) && item.ID >= previous.ID) {
				t.Fatal("catalog lost descending creation/UUID order")
			}
			seen[item.ID] = true
			previous = &domain.JobCursor{CreatedAt: item.CreatedAt, ID: item.ID}
		}
		if page.NextCursor == nil {
			break
		}
		page, err = store.ListTargetCatalog(ctx, principal, "research", domain.TargetCatalogOptions{Limit: 200, CreatedBefore: &first.CreatedBefore, Before: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 1005 {
		t.Fatalf("bounded catalog omitted targets beyond legacy cap: %d", len(seen))
	}
	snapshot, err := store.GetTargetSnapshot(ctx, principal, "research", created.Value.ID)
	if err != nil || snapshot.Target.Generation.LogStore == nil || snapshot.Target.Generation.LogStore.Version != 9007199254740993 || snapshot.Target.Generation.ID != created.Value.GenerationID || snapshot.AsOf.IsZero() {
		t.Fatalf("target snapshot=%#v,%v", snapshot, err)
	}
	if _, err = store.GetTargetSnapshot(ctx, principal, "research", other.Value.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-namespace target UUID leaked: %v", err)
	}
	spec.Name = "source-host"
	generation, err := store.CreateTargetGeneration(ctx, principal, "research", spec.Name, "catalog-generation", fmt.Sprintf("sha256:%064x", 4), domain.TargetGenerationChange{Spec: spec, ExpectedRevision: created.Value.Revision})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.GetTargetSnapshot(ctx, principal, "research", created.Value.ID)
	if err != nil || current.Target.Generation.ID != generation.Value.GenerationID || current.Target.Generation.Number != 2 || !current.Target.CreatedAt.Equal(snapshot.Target.CreatedAt) {
		t.Fatalf("target update corrupted immutable catalog order: %#v,%v", current, err)
	}

	// Valid, large capability sets force byte-limited pages independently of the
	// count limit. Cursors must stop at the last emitted target, not the last row
	// selected by SQL, or subsequent pages would silently omit configurations.
	if _, err = pool.Exec(ctx, `WITH selected AS (SELECT current_generation_id FROM targets WHERE namespace_id=$1 AND name LIKE 'bulk-%' ORDER BY created_at DESC,id DESC LIMIT 20)
 UPDATE target_generations SET capabilities=ARRAY(SELECT repeat('a',120)||number::text FROM generate_series(1,1024) number ORDER BY repeat('a',120)||number::text)
 WHERE id IN (SELECT current_generation_id FROM selected)`, first.NamespaceID); err != nil {
		t.Fatal(err)
	}
	largePage, err := store.ListTargetCatalog(ctx, principal, "research", domain.TargetCatalogOptions{Limit: 200})
	if err != nil || largePage.NextCursor == nil || len(largePage.Items) >= 200 || largePage.Total != 1006 {
		t.Fatalf("byte-limited catalog=%d items,%v", len(largePage.Items), err)
	}
	body, err := json.Marshal(largePage.Items)
	if err != nil || len(body) > maximumTargetItemsJSON {
		t.Fatalf("target body exceeds encoded limit: %d,%v", len(body), err)
	}
	byteSeen := map[string]bool{}
	for pages := 0; pages < 10; pages++ {
		for _, item := range largePage.Items {
			if byteSeen[item.ID] {
				t.Fatal("byte-limited cursor repeated a target")
			}
			byteSeen[item.ID] = true
		}
		if largePage.NextCursor == nil {
			break
		}
		largePage, err = store.ListTargetCatalog(ctx, principal, "research", domain.TargetCatalogOptions{Limit: 200, CreatedBefore: &largePage.CreatedBefore, Before: largePage.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(byteSeen) != 1006 {
		t.Fatalf("byte-limited cursor omitted targets: %d", len(byteSeen))
	}
	// More than 200 scheduler partitions must be disclosed as a preview and
	// traversable without omitting punctuation-sensitive names or changing generations.
	slurmSpec := domain.TargetSpec{Name: "large-cluster", Kind: "slurm", ExecutionBackend: "slurm", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64"}}
	for index := 0; index < 402; index++ {
		slurmSpec.Partitions = append(slurmSpec.Partitions, domain.PartitionSpec{Name: fmt.Sprintf("part-%04d", index), IsDefault: index == 401})
	}
	slurmSpec.Partitions = append(slurmSpec.Partitions, domain.PartitionSpec{Name: "a-a"}, domain.PartitionSpec{Name: "a.a"}, domain.PartitionSpec{Name: "a_a"})
	cluster, err := store.CreateTarget(ctx, principal, "research", "large-cluster", fmt.Sprintf("sha256:%064x", 5), slurmSpec)
	if err != nil {
		t.Fatal(err)
	}
	clusterSnapshot, err := store.GetTargetSnapshot(ctx, principal, "research", cluster.Value.ID)
	if err != nil || len(clusterSnapshot.Target.Generation.Partitions) != 200 || clusterSnapshot.Target.Generation.PartitionCount != 405 || !clusterSnapshot.Target.Generation.PartitionsTruncated {
		t.Fatalf("large partition preview=%#v,%v", clusterSnapshot, err)
	}
	latest, err := store.ListTargetCatalog(ctx, principal, "research", domain.TargetCatalogOptions{Limit: 1})
	if err != nil || len(latest.Items) != 1 || latest.Items[0].ID != cluster.Value.ID || latest.Items[0].Generation.PartitionCount != 405 || !latest.Items[0].Generation.PartitionsTruncated || len(latest.Items[0].Generation.Partitions) != 200 {
		t.Fatalf("catalog partition preview=%#v,%v", latest, err)
	}
	partitionOptions := domain.TargetPartitionOptions{GenerationID: cluster.Value.GenerationID, Limit: 200}
	partitionNames := []string{}
	for pages := 0; pages < 4; pages++ {
		partitions, readErr := store.ListTargetPartitions(ctx, principal, "research", cluster.Value.ID, partitionOptions)
		if readErr != nil || partitions.Total != 405 || partitions.GenerationID != cluster.Value.GenerationID || partitions.TargetID != cluster.Value.ID || partitions.AsOf.IsZero() {
			t.Fatalf("partition page=%#v,%v", partitions, readErr)
		}
		for _, partition := range partitions.Items {
			if len(partitionNames) > 0 && partition.Name <= partitionNames[len(partitionNames)-1] {
				t.Fatal("partition cursor lost strict byte order")
			}
			partitionNames = append(partitionNames, partition.Name)
		}
		if partitions.NextName == "" {
			break
		}
		partitionOptions.AfterName = partitions.NextName
	}
	if len(partitionNames) != 405 || partitionNames[0] != "a-a" || partitionNames[1] != "a.a" || partitionNames[2] != "a_a" {
		t.Fatalf("incomplete partition traversal: %d", len(partitionNames))
	}
	if _, err = store.ListTargetPartitions(ctx, principal, "other", cluster.Value.ID, partitionOptions); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-namespace partitions exposed: %v", err)
	}
	slurmSpec.Partitions = []domain.PartitionSpec{{Name: "replacement", IsDefault: true}}
	updatedCluster, err := store.CreateTargetGeneration(ctx, principal, "research", slurmSpec.Name, "cluster-generation", fmt.Sprintf("sha256:%064x", 6), domain.TargetGenerationChange{Spec: slurmSpec, ExpectedRevision: cluster.Value.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListTargetPartitions(ctx, principal, "research", cluster.Value.ID, partitionOptions); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("partition cursor survived generation replacement: %v", err)
	}
	partitionOptions = domain.TargetPartitionOptions{GenerationID: updatedCluster.Value.GenerationID, Limit: 200}
	replacement, err := store.ListTargetPartitions(ctx, principal, "research", cluster.Value.ID, partitionOptions)
	if err != nil || replacement.Total != 1 || len(replacement.Items) != 1 || replacement.Items[0].Name != "replacement" || !replacement.Items[0].IsDefault || replacement.NextName != "" {
		t.Fatalf("current generation partitions=%#v,%v", replacement, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE membership_grants SET revoked_at=statement_timestamp() WHERE namespace_id=$1`, first.NamespaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListTargetCatalog(ctx, principal, "research", domain.TargetCatalogOptions{Limit: 200, CreatedBefore: &first.CreatedBefore, Before: first.NextCursor}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cursor conferred revoked access: %v", err)
	}
	if _, err = store.ListTargetPartitions(ctx, principal, "research", cluster.Value.ID, partitionOptions); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("partition cursor conferred revoked access: %v", err)
	}
	if _, err = store.GetTargetSnapshot(ctx, principal, "research", created.Value.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("detail survived revoked grant: %v", err)
	}
}

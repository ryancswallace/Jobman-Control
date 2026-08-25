package postgres

import (
	"strings"
	"testing"
)

func TestLoadMigrationsIsOrderedAndChecksummed(t *testing.T) {
	t.Parallel()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(migrations) != 12 || migrations[0].version != "000001_control_foundation.sql" ||
		migrations[1].version != "000002_targets_agents_assignments.sql" ||
		migrations[2].version != "000003_agent_execution.sql" ||
		migrations[3].version != "000004_shared_logs.sql" ||
		migrations[4].version != "000005_slurm_execution.sql" ||
		migrations[5].version != "000006_target_agent_lifecycle.sql" ||
		migrations[6].version != "000007_file_artifacts.sql" ||
		migrations[7].version != "000008_target_generations.sql" ||
		migrations[8].version != "000009_collections.sql" ||
		migrations[9].version != "000010_dependency_graphs.sql" ||
		migrations[10].version != "000011_production_controls.sql" ||
		migrations[11].version != "000012_completed_history_import.sql" {
		t.Fatalf("loadMigrations() = %#v", migrations)
	}
	for _, item := range migrations {
		if len(item.checksum) != 64 || strings.TrimSpace(item.contents) == "" {
			t.Fatalf("migration checksum or contents are invalid: %#v", item)
		}
	}
}

func TestCompareMigrations(t *testing.T) {
	t.Parallel()
	migrations := []migration{{version: "000001.sql", checksum: "one"}}
	tests := []struct {
		name    string
		applied map[string]string
		wantErr string
	}{
		{name: "current", applied: map[string]string{"000001.sql": "one"}},
		{name: "pending", applied: map[string]string{}, wantErr: "pending"},
		{name: "changed", applied: map[string]string{"000001.sql": "two"}, wantErr: "checksum"},
		{name: "newer database", applied: map[string]string{"000002.sql": "two"}, wantErr: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := compareMigrations(migrations, test.applied)
			if test.wantErr == "" && err != nil {
				t.Fatalf("compareMigrations() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("compareMigrations() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

package main

// Synthetic scale metadata is separate from execution acceptance. This helper
// admits pending jobs normally and imports history without runs or terminal
// execution events. It never installs directory mappings or verified grants.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

const scaleTarget = "dashboard-scale-no-executor"

type scaleInput struct {
	InstanceID string        `json:"instanceId"`
	Issuer     string        `json:"issuer"`
	HistoryAt  time.Time     `json:"historyAt"`
	Users      []fixtureUser `json:"users"`
}

type scaleNamespace struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ActiveJobID     string `json:"activeJobId"`
	ImportedJobID   string `json:"importedJobId"`
	ActiveJobs      int    `json:"activeJobs"`
	ImportedHistory int    `json:"importedHistory"`
}

type scaleSeedResult struct {
	Version       int                        `json:"version"`
	Synthetic     bool                       `json:"synthetic"`
	Mode          string                     `json:"mode"`
	DeploymentID  string                     `json:"deploymentId"`
	InstanceID    string                     `json:"instanceId"`
	RecoveryEpoch string                     `json:"recoveryEpoch"`
	PreparedAt    time.Time                  `json:"preparedAt"`
	HistoryAt     time.Time                  `json:"historyAt"`
	Identities    []domain.DirectoryIdentity `json:"identities"`
	Namespaces    []scaleNamespace           `json:"namespaces"`
}

func (input scaleInput) validate(now time.Time) error {
	if !domain.IsID(input.InstanceID) || input.Issuer != "https://oidc.lab.test:8443/realms/jobman-lab" || len(input.Users) != 25 || input.HistoryAt.IsZero() || !input.HistoryAt.Before(now.Add(-time.Minute)) || input.HistoryAt.Before(now.Add(-7*24*time.Hour)) {
		return errors.New("bounded synthetic scale input required")
	}
	subjects := map[string]bool{}
	for i, user := range input.Users {
		if user.DirectoryID != fmt.Sprintf("74000000-0000-4000-8000-%012d", i+1) || !domain.IsID(user.Subject) || subjects[user.Subject] || user.Name != fmt.Sprintf("Synthetic scale %02d", i+1) {
			return errors.New("distinct immutable scale identities required")
		}
		subjects[user.Subject] = true
	}
	return nil
}

func scaleNamespaceNames(profile fixtureProfile) []string {
	count := 10
	if profile.secondary() {
		count = 5
	}
	result := make([]string, count)
	for i := range result {
		result[i] = fmt.Sprintf("dashboard-scale-%02d", i+1)
	}
	return result
}

// Call only after a source-pinned private pending receipt and exclusive operator
// lock are durable. Any partial seed is retained; this function refuses a retry
// against existing namespace names or identities rather than altering old data.
func seedScale(ctx context.Context, pool *pgxpool.Pool, store *postgres.Store, input scaleInput, profile fixtureProfile) (scaleSeedResult, error) {
	result := scaleSeedResult{Version: 1, Synthetic: true, Mode: "imported-history-no-execution", DeploymentID: profile.deployment, InstanceID: input.InstanceID, RecoveryEpoch: "1", HistoryAt: input.HistoryAt}
	if profile.validate() != nil || input.validate(time.Now().UTC()) != nil {
		return result, errors.New("invalid scale seed input")
	}
	capabilities, err := store.Capabilities(ctx)
	if err != nil || capabilities.InstanceID != input.InstanceID || capabilities.RecoveryEpoch != "1" {
		return result, errors.New("scale source identity differs")
	}
	names := scaleNamespaceNames(profile)
	subjects := make([]string, len(input.Users))
	for i, user := range input.Users {
		subjects[i] = user.Subject
	}
	var collision bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM namespaces WHERE name=ANY($1::text[])) OR EXISTS(SELECT 1 FROM principals WHERE issuer=$2 AND subject=ANY($3::text[]))`, names, input.Issuer, subjects).Scan(&collision); err != nil || collision {
		return result, errors.New("scale seed refuses existing identities or namespaces")
	}
	// Only the new first namespace receives temporary bootstrap grants for all
	// new users. Ordinary directory adoption must remove these before exposure.
	for _, user := range input.Users {
		principal := domain.Principal{Issuer: input.Issuer, Subject: user.Subject}
		if err := store.EnsureBootstrapIdentity(ctx, domain.BootstrapIdentity{Principal: principal, DisplayName: user.Name, Namespace: names[0], Mode: "synthetic-dashboard-scale"}); err != nil {
			return result, err
		}
		access, accessErr := store.CurrentPrincipal(ctx, principal, "", 100)
		if accessErr != nil {
			return result, accessErr
		}
		result.Identities = append(result.Identities, domain.DirectoryIdentity{DirectoryID: user.DirectoryID, PrincipalID: access.PrincipalID, Issuer: input.Issuer, Subject: user.Subject, DisplayName: user.Name})
	}
	principal := domain.Principal{Issuer: input.Issuer, Subject: input.Users[0].Subject}
	for i, name := range names {
		if i > 0 {
			if err := store.EnsureBootstrapIdentity(ctx, domain.BootstrapIdentity{Principal: principal, DisplayName: input.Users[0].Name, Namespace: name, Mode: "synthetic-dashboard-scale"}); err != nil {
				return result, err
			}
		}
		ns, seedErr := seedScaleNamespace(ctx, pool, store, principal, name, input.HistoryAt)
		if seedErr != nil {
			return result, seedErr
		}
		result.Namespaces = append(result.Namespaces, ns)
	}
	result.PreparedAt = time.Now().UTC()
	return result, nil
}

func seedScaleNamespace(ctx context.Context, pool *pgxpool.Pool, store *postgres.Store, principal domain.Principal, name string, historyAt time.Time) (scaleNamespace, error) {
	ns := scaleNamespace{Name: name, ActiveJobs: 50, ImportedHistory: 10000}
	target, err := store.CreateTarget(ctx, principal, name, "scale-target-v1", fixtureDigest(name+"scale-target-v1"), domain.TargetSpec{Name: scaleTarget, Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, OperatingSystems: []string{"linux"}, Architectures: []string{"amd64", "arm64"}})
	if err != nil {
		return ns, err
	}
	// No enrollment token or execution agent is created for this target.
	for i := range 50 {
		request, requestErr := seedSubmission(name, fmt.Sprintf("scale-active-%02d", i+1), scaleTarget)
		if requestErr != nil {
			return ns, requestErr
		}
		created, submitErr := store.SubmitJob(ctx, principal, fmt.Sprintf("scale-active-%02d-v1", i+1), request)
		if submitErr != nil {
			return ns, submitErr
		}
		if i == 0 {
			ns.ActiveJobID = created.Job.ID
		}
	}
	// Copy validated admitted workload/placement metadata as explicitly imported
	// history. INSERT creates no execution or terminal-transition event. Never
	// UPDATE an admitted job into history: that would publish a transition.
	tag, err := pool.Exec(ctx, `INSERT INTO jobs(id,namespace_id,owner_principal_id,name,labels,phase,desired_state,placement_target,placement_partition,workload_digest,request_digest,request_document,target_id,target_generation_id,created_at,updated_at,imported,import_source,outcome,completed_at,completed_recorded_at,completed_provenance)
 SELECT gen_random_uuid(),j.namespace_id,j.owner_principal_id,j.name,j.labels,'terminal',j.desired_state,j.placement_target,j.placement_partition,j.workload_digest,j.request_digest,j.request_document,j.target_id,j.target_generation_id,$2::timestamptz-interval '2 hours'+(g/25)*interval '1 second',transaction_timestamp(),true,jsonb_build_object('store','sqlite','schema',1,'jobId','synthetic-scale-'||lpad(g::text,5,'0')),'success',$2::timestamptz,transaction_timestamp(),'history_import'
 FROM jobs j CROSS JOIN generate_series(1,10000) g WHERE j.id=$1::uuid AND NOT j.imported AND j.phase='accepted'`, ns.ActiveJobID, historyAt)
	if err != nil || tag.RowsAffected() != 10000 {
		return ns, errors.New("bounded scale history insert failed")
	}
	if err = pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE namespace_id=(SELECT namespace_id FROM jobs WHERE id=$1::uuid) AND imported ORDER BY id LIMIT 1`, ns.ActiveJobID).Scan(&ns.ImportedJobID); err != nil {
		return ns, errors.New("scale history sample is missing")
	}
	var total, active, imported, runs, executions, agents, events int
	err = pool.QueryRow(ctx, `SELECT n.id::text,count(j.id),count(*) FILTER(WHERE j.phase='accepted' AND NOT j.imported),count(*) FILTER(WHERE j.imported AND j.outcome='success' AND j.completed_at=$2 AND j.completed_provenance='history_import'),(SELECT count(*) FROM runs r WHERE r.job_id IN(SELECT id FROM jobs WHERE namespace_id=n.id)),(SELECT count(*) FROM executions e WHERE e.namespace_id=n.id),(SELECT count(*) FROM agents a WHERE a.target_generation_id=$3::uuid),(SELECT count(*) FROM outbox e WHERE e.namespace_id=n.id AND e.topic='monitoring.job_terminal.v1') FROM namespaces n JOIN jobs j ON j.namespace_id=n.id WHERE n.name=$1 GROUP BY n.id`, name, historyAt, target.Value.GenerationID).Scan(&ns.ID, &total, &active, &imported, &runs, &executions, &agents, &events)
	if err != nil || total != 10050 || active != 50 || imported != 10000 || runs != 0 || executions != 0 || agents != 0 || events != 0 {
		return ns, errors.New("scale source count or no-execution invariant failed")
	}
	return ns, nil
}

// Build additive drafts only. The Lab operator installs these after current-file
// comparisons and normal ConfigureDirectory/LDAPS reconciliation; no snapshot
// here is asserted as verified directory authority.
func scaleDirectoryDraft(config directory.Config, state fixtureState, seed scaleSeedResult, profile fixtureProfile) (directory.Config, fixtureState, error) {
	if profile.validate() != nil || config.Mapping.SourceID != profile.source || directory.Validate(config) != nil || validateState(state) != nil || len(seed.Identities) != 25 || len(seed.Namespaces) != len(scaleNamespaceNames(profile)) || seed.DeploymentID != profile.deployment {
		return directory.Config{}, fixtureState{}, errors.New("scale directory draft inputs differ")
	}
	// Deep copy so rejection never mutates the original slices.
	raw, err := json.Marshal(config)
	if err != nil {
		return directory.Config{}, fixtureState{}, err
	}
	var out directory.Config
	if json.Unmarshal(raw, &out) != nil {
		return out, state, errors.New("copy directory draft")
	}
	raw, err = json.Marshal(state)
	if err != nil {
		return directory.Config{}, fixtureState{}, err
	}
	var next fixtureState
	if json.Unmarshal(raw, &next) != nil {
		return out, next, errors.New("copy directory state")
	}
	out.Mapping.Revision++
	next.Revision++
	for _, identity := range seed.Identities {
		out.Mapping.Identities = append(out.Mapping.Identities, identity)
		next.Users = append(next.Users, stateUser{DirectoryID: identity.DirectoryID, Enabled: true})
	}
	members := make([]string, 0, 25)
	for _, identity := range seed.Identities {
		members = append(members, identity.DirectoryID)
	}
	for i, ns := range seed.Namespaces {
		if ns.Name != scaleNamespaceNames(profile)[i] || !domain.IsID(ns.ID) || slices.Contains(out.Mapping.Namespaces, ns.ID) {
			return out, next, errors.New("scale namespace draft collision")
		}
		sequence := 101 + i
		if profile.secondary() {
			sequence = 201 + i
		}
		group := fmt.Sprintf("78000000-0000-4000-8000-%012d", sequence)
		out.Mapping.Namespaces = append(out.Mapping.Namespaces, ns.ID)
		out.Mapping.ApprovedTransitions = append(out.Mapping.ApprovedTransitions, ns.ID)
		out.Mapping.Bindings = append(out.Mapping.Bindings, domain.DirectoryBinding{GroupID: group, NamespaceID: ns.ID, Role: domain.RoleViewer})
		next.Groups = append(next.Groups, stateGroup{ID: group, Members: slices.Clone(members)})
	}
	if directory.Validate(out) != nil || validateState(next) != nil {
		return out, next, errors.New("bounded additive scale directory is invalid")
	}
	return out, next, nil
}

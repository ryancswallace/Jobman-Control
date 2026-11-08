package resources

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

func fixture() Document {
	return Document{SchemaVersion: 1, Endpoint: "https://control.example.invalid", ExpectedInstanceID: "00000000-0000-4000-8000-000000000001", ExpectedRecoveryEpoch: "1", Resources: []Resource{{Kind: "NamespacePolicy", Namespace: "example", NamespaceID: "00000000-0000-4000-8000-000000000002", ExpectedRevision: 3, Policy: &Policy{MaxActiveJobs: 10, MaxQueuedJobs: 100, MaxCollectionItems: 100, MaxGraphNodes: 100, IdempotencyRetention: "24h", PublishedOutboxRetention: "24h"}}}}
}

func TestDocumentStrictAndBounded(t *testing.T) {
	t.Parallel()
	document := fixture()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	first, err := document.Digest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := decoded.Digest()
	if err != nil || first != second {
		t.Fatal("canonical digest changed")
	}
	for _, invalid := range []string{
		strings.Replace(string(data), `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
		strings.Replace(string(data), `"namespaceId"`, `"NamespaceId"`, 1),
		strings.Replace(string(data), `"policy":{`, `"policy":null,"ignored":{`, 1),
		strings.Replace(string(data), `"NamespacePolicy"`, `"Namespace"`, 1),
		strings.Replace(string(data), `"expectedRevision":3`, `"expectedRevision":0`, 1),
		strings.Replace(string(data), `"24h"`, `"24h0.5s"`, 1),
		strings.Replace(string(data), `"maxActiveJobs":10`, `"maxActiveJobs":2147483648`, 1),
		strings.Replace(string(data), `"expectedRecoveryEpoch":"1"`, `"expectedRecoveryEpoch":"01"`, 1),
		string(data) + ` {}`,
	} {
		if _, err := Decode(strings.NewReader(invalid)); err == nil {
			t.Fatal("unsafe document accepted")
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

type fakeControl struct {
	document                   Document
	policy                     Policy
	revision                   int64
	instance, namespace, epoch string
	writes                     int
	grantStatus                int
	keys                       []string
	target                     *Target
}

func (fake *fakeControl) client(t *testing.T) *Client {
	t.Helper()
	return &Client{endpoint: fake.document.Endpoint, token: "synthetic-token", http: &http.Client{Transport: transportFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Fatal("credential missing")
		}
		status := http.StatusOK
		var payload any
		switch request.URL.Path {
		case "/v1/capabilities":
			payload = map[string]any{"apiVersion": apiVersion, "kind": "ControlCapabilities", "capabilities": map[string]string{"instanceId": fake.instance, "recoveryEpoch": fake.epoch}}
		case "/v1/me":
			payload = map[string]any{"apiVersion": apiVersion, "kind": "CurrentPrincipal", "namespaces": []domain.NamespaceAccess{{ID: fake.namespace, Name: "example", Capabilities: []string{domain.CapabilityPolicyManage, domain.CapabilityMembershipsManage, domain.CapabilityTargetsManage}}}}
		case "/v1/namespaces/example/policy":
			if request.Method == http.MethodPut {
				if request.Header.Get("If-Match") != `"revision-3"` || !domain.ValidIdempotencyKey(request.Header.Get("Idempotency-Key")) {
					t.Fatal("mutation lacks revision/idempotency fence")
				}
				fake.keys = append(fake.keys, request.Header.Get("Idempotency-Key"))
				fake.writes++
				fake.policy = *fake.document.Resources[0].Policy
				fake.revision++
			}
			payload = map[string]any{"apiVersion": apiVersion, "kind": "NamespacePolicy", "metadata": map[string]any{"namespace": "example", "revision": fake.revision}, "spec": fake.policy}
		case "/v1/namespaces/example/targets/example-host":
			if fake.target == nil {
				status = http.StatusNotFound
			} else {
				payload = map[string]any{"apiVersion": apiVersion, "kind": "Target", "metadata": map[string]any{"namespace": "example", "name": "example-host", "id": "00000000-0000-4000-8000-000000000003", "generationId": "00000000-0000-4000-8000-000000000004", "revision": fake.revision}, "spec": fake.target, "status": map[string]string{"state": "active"}}
			}
		case "/v1/namespaces/example/targets", "/v1/namespaces/example/targets/example-host/generations":
			if request.Method != http.MethodPost || !domain.ValidIdempotencyKey(request.Header.Get("Idempotency-Key")) {
				t.Fatal("invalid target mutation")
			}
			if strings.HasSuffix(request.URL.Path, "/generations") && request.Header.Get("If-Match") != `"revision-3"` {
				t.Fatal("generation lacks revision fence")
			}
			var body struct {
				Kind string `json:"kind"`
				Spec Target `json:"spec"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Kind != fake.document.Resources[0].Kind || !equalJSON(body.Spec, fake.document.Resources[0].Target) {
				t.Fatal("target mutation differs from reviewed document")
			}
			fake.target = &body.Spec
			fake.writes++
			fake.revision++
		default:
			if strings.Contains(request.URL.Path, "/membership-grants/") && request.Method == http.MethodPut {
				fake.writes++
				fake.keys = append(fake.keys, request.Header.Get("Idempotency-Key"))
				status = fake.grantStatus
				payload = map[string]string{"private": "must not echo source responses"}
			} else {
				t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			}
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), Request: request}, nil
	})}}
}

func newFake() *fakeControl {
	document := fixture()
	policy := *document.Resources[0].Policy
	policy.MaxActiveJobs = 5
	return &fakeControl{document: document, policy: policy, revision: 3, instance: document.ExpectedInstanceID, namespace: document.Resources[0].NamespaceID, epoch: "1", grantStatus: http.StatusOK}
}

func TestApplyRevisionFencedAndRepeatSafe(t *testing.T) {
	t.Parallel()
	fake := newFake()
	client := fake.client(t)
	digest, err := fake.document.Digest()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.Apply(t.Context(), fake.document, digest)
	if err != nil || fake.writes != 1 || plan.Operations[0].Action != "applied" {
		t.Fatalf("apply failed: %v", err)
	}
	plan, err = client.Apply(t.Context(), fake.document, digest)
	if err != nil || fake.writes != 1 || plan.Operations[0].Action != "unchanged" {
		t.Fatalf("repeat was not harmless: %v", err)
	}
}

func TestApplyRejectsDriftBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*fakeControl){
		func(fake *fakeControl) { fake.instance = "00000000-0000-4000-8000-000000000099" },
		func(fake *fakeControl) { fake.namespace = "00000000-0000-4000-8000-000000000099" },
		func(fake *fakeControl) { fake.epoch = "2" },
		func(fake *fakeControl) { fake.revision = 4 },
	} {
		fake := newFake()
		change(fake)
		digest, err := fake.document.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fake.client(t).Apply(t.Context(), fake.document, digest); err == nil || fake.writes != 0 {
			t.Fatal("drift allowed a write")
		}
	}
	fake := newFake()
	if _, err := fake.client(t).Apply(t.Context(), fake.document, "sha256:missing"); err == nil || fake.writes != 0 {
		t.Fatal("digest without review accepted")
	}
}

func TestRevokedGrantFailureRemainsFailure(t *testing.T) {
	t.Parallel()
	fake := newFake()
	fake.document.Resources = []Resource{{Kind: "MembershipGrant", Namespace: "example", NamespaceID: fake.namespace, ID: "00000000-0000-4000-8000-000000000003", Grant: &Grant{Principal: Principal{Issuer: "https://identity.example.invalid", Subject: "example-user", DisplayName: "Example"}, Role: "viewer"}}}
	fake.grantStatus = http.StatusConflict
	digest, err := fake.document.Digest()
	if err != nil {
		t.Fatal(err)
	}
	client := fake.client(t)
	for range 2 {
		if _, err = client.Apply(t.Context(), fake.document, digest); err == nil || strings.Contains(err.Error(), "must not echo") {
			t.Fatal("revoked grant/error disclosure boundary failed")
		}
	}
	if len(fake.keys) != 2 || fake.keys[0] != fake.keys[1] {
		t.Fatal("retry changed idempotency identity")
	}
}

func TestTargetCreationGenerationAndRepeatedApply(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"Target", "TargetGeneration"} {
		fake := newFake()
		resource := Resource{Kind: kind, Namespace: "example", NamespaceID: fake.namespace, Name: "example-host", Target: &Target{Kind: "host", ExecutionBackend: "subprocess", Runtimes: []string{"native"}, Provider: domain.TargetProvider{Kind: "on-prem"}}}
		if kind == "TargetGeneration" {
			resource.ID = "00000000-0000-4000-8000-000000000003"
			resource.ExpectedGenerationID = "00000000-0000-4000-8000-000000000004"
			resource.ExpectedRevision = 3
			previous := *resource.Target
			previous.OperatingSystems = []string{"linux"}
			fake.target = &previous
		}
		fake.document.Resources = []Resource{resource}
		client := fake.client(t)
		digest, err := fake.document.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = client.Apply(t.Context(), fake.document, digest); err != nil {
			t.Fatal(err)
		}
		plan, err := client.Apply(t.Context(), fake.document, digest)
		if err != nil || fake.writes != 1 || plan.Operations[0].Action != "unchanged" {
			t.Fatalf("target replay failed: %v", err)
		}
	}
}

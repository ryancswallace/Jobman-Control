package resources

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ryancswallace/jobman-control/internal/domain"
)

const apiVersion = "jobman.control/v1alpha1"

// Client uses a protected credential and exact HTTPS origin without redirects
// or ambient proxy configuration. It never exposes error response bodies.
type Client struct {
	endpoint, token string
	http            *http.Client
}

// Plan is a content-free change summary bound to the complete desired document.
type Plan struct {
	Digest       string      `json:"digest"`
	LiveVerified bool        `json:"liveVerified"`
	Operations   []Operation `json:"operations"`
}

// Operation identifies a document entry without printing identity/policy data.
type Operation struct {
	Index  int    `json:"index"`
	Kind   string `json:"kind"`
	Action string `json:"action"`
}

// OfflinePlan describes intent only. It makes no assertion about current state.
func OfflinePlan(document Document, digest string) Plan {
	plan := Plan{Digest: digest, Operations: make([]Operation, 0, len(document.Resources))}
	for index, resource := range document.Resources {
		plan.Operations = append(plan.Operations, Operation{Index: index, Kind: resource.Kind, Action: "unverified"})
	}
	return plan
}

// NewClient reads bounded protected references only when live operation is requested.
func NewClient(endpoint, tokenFile, caFile string) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("resource endpoint must be an exact HTTPS origin")
	}
	data, err := readFile(tokenFile, 65536, true)
	if err != nil {
		return nil, errors.New("cannot read protected resource credential")
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	clear(data)
	if token == "" || strings.ContainsAny(token, " \t\n\r\x00") {
		return nil, errors.New("invalid resource credential")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("cannot load system trust roots")
	}
	if caFile != "" {
		ca, readErr := readFile(caFile, 1<<20, false)
		if readErr != nil || !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("cannot load resource trust roots")
		}
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, MaxIdleConns: 2, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), token: token, http: client}, nil
}

// Close drops idle connections and the client's credential reference.
func (client *Client) Close() { client.http.CloseIdleConnections(); client.token = "" }

func readFile(path string, limit int64, private bool) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || (private && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("invalid reference file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open reference file")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cannot read bounded reference file")
	}
	return data, nil
}

func (client *Client) request(ctx context.Context, method, path string, body any, revision int64, key string, destination any) (int, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, errors.New("cannot encode administrative request")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, client.endpoint+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, errors.New("cannot create administrative request")
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if revision > 0 {
		request.Header.Set("If-Match", `"revision-`+strconv.FormatInt(revision, 10)+`"`)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return 0, errors.New("administrative request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return response.StatusCode, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response.StatusCode, fmt.Errorf("administrative request returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if err != nil || len(data) > maximumBytes {
		return response.StatusCode, errors.New("administrative response exceeds bounds")
	}
	if destination != nil && json.Unmarshal(data, destination) != nil {
		return response.StatusCode, errors.New("administrative response is invalid")
	}
	return response.StatusCode, nil
}

func (client *Client) verifyIdentity(ctx context.Context, document Document) (map[string]domain.NamespaceAccess, error) {
	var capabilities struct {
		APIVersion   string                     `json:"apiVersion"`
		Kind         string                     `json:"kind"`
		Capabilities domain.ControlCapabilities `json:"capabilities"`
	}
	status, err := client.request(ctx, http.MethodGet, "/v1/capabilities", nil, 0, "", &capabilities)
	if err != nil || status != http.StatusOK || capabilities.APIVersion != apiVersion || capabilities.Kind != "ControlCapabilities" || capabilities.Capabilities.InstanceID != document.ExpectedInstanceID || capabilities.Capabilities.RecoveryEpoch != document.ExpectedRecoveryEpoch {
		return nil, errors.New("control instance or recovery identity changed")
	}
	result := map[string]domain.NamespaceAccess{}
	next := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		var access struct {
			APIVersion string                   `json:"apiVersion"`
			Kind       string                   `json:"kind"`
			Namespaces []domain.NamespaceAccess `json:"namespaces"`
			Next       string                   `json:"nextPageToken"`
		}
		path := "/v1/me?limit=200"
		if next != "" {
			path += "&pageToken=" + url.QueryEscape(next)
		}
		status, err = client.request(ctx, http.MethodGet, path, nil, 0, "", &access)
		if err != nil || status != http.StatusOK || access.APIVersion != apiVersion || access.Kind != "CurrentPrincipal" || len(access.Namespaces) > 200 {
			return nil, errors.New("namespace discovery failed")
		}
		for _, ns := range access.Namespaces {
			if _, duplicate := result[ns.Name]; duplicate {
				return nil, errors.New("namespace discovery is ambiguous")
			}
			result[ns.Name] = ns
		}
		if access.Next == "" {
			return result, nil
		}
		if len(access.Next) > 48 || seen[access.Next] {
			return nil, errors.New("namespace discovery is unbounded")
		}
		seen[access.Next] = true
		next = access.Next
	}
	return nil, errors.New("namespace discovery exceeds page limit")
}

func requiredCapability(kind string) string {
	switch kind {
	case "NamespacePolicy":
		return domain.CapabilityPolicyManage
	case "MembershipGrant":
		return domain.CapabilityMembershipsManage
	default:
		return domain.CapabilityTargetsManage
	}
}

// Plan verifies identities and readable current resources before any mutation.
// Membership grant conflicts are checked by the immutable grant API during apply.
func (client *Client) Plan(ctx context.Context, document Document) (Plan, error) {
	if err := document.Validate(); err != nil {
		return Plan{}, err
	}
	if strings.TrimRight(document.Endpoint, "/") != client.endpoint {
		return Plan{}, errors.New("resource client origin mismatch")
	}
	digest, err := document.Digest()
	if err != nil {
		return Plan{}, err
	}
	plan := OfflinePlan(document, digest)
	namespaces, err := client.verifyIdentity(ctx, document)
	if err != nil {
		return Plan{}, err
	}
	for index, resource := range document.Resources {
		ns, found := namespaces[resource.Namespace]
		if !found || ns.ID != resource.NamespaceID || !slices.Contains(ns.Capabilities, requiredCapability(resource.Kind)) {
			return Plan{}, errors.New("resource namespace identity or authority mismatch")
		}
		action, checkErr := client.checkResource(ctx, resource)
		if checkErr != nil {
			return Plan{}, checkErr
		}
		plan.Operations[index].Action = action
	}
	plan.LiveVerified = true
	return plan, nil
}

func (client *Client) checkResource(ctx context.Context, resource Resource) (string, error) {
	base := "/v1/namespaces/" + resource.Namespace
	if resource.Kind == "MembershipGrant" {
		return "ensure-grant", nil
	}
	if resource.Kind == "NamespacePolicy" {
		var response struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Namespace string `json:"namespace"`
				Revision  int64  `json:"revision"`
			} `json:"metadata"`
			Spec Policy `json:"spec"`
		}
		status, err := client.request(ctx, http.MethodGet, base+"/policy", nil, 0, "", &response)
		if err != nil || status != http.StatusOK || response.APIVersion != apiVersion || response.Kind != "NamespacePolicy" || response.Metadata.Namespace != resource.Namespace {
			return "", errors.New("cannot inspect namespace policy")
		}
		if samePolicy(response.Spec, *resource.Policy) {
			return "unchanged", nil
		}
		if response.Metadata.Revision != resource.ExpectedRevision {
			return "", errors.New("namespace policy revision changed")
		}
		return "update-policy", nil
	}
	var response struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Namespace    string `json:"namespace"`
			Name         string `json:"name"`
			ID           string `json:"id"`
			GenerationID string `json:"generationId"`
			Revision     int64  `json:"revision"`
		} `json:"metadata"`
		Spec   Target `json:"spec"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	status, err := client.request(ctx, http.MethodGet, base+"/targets/"+resource.Name, nil, 0, "", &response)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound && resource.Kind == "Target" {
		return "create-target", nil
	}
	if status != http.StatusOK || response.APIVersion != apiVersion || response.Kind != "Target" || response.Metadata.Namespace != resource.Namespace || response.Metadata.Name != resource.Name || response.Status.State != "active" {
		return "", errors.New("target is unavailable or not active")
	}
	if resource.Kind == "TargetGeneration" && response.Metadata.ID != resource.ID {
		return "", errors.New("target identity changed")
	}
	response.Spec.Provider = domain.NormalizeTargetProvider(response.Spec.Provider)
	desired := *resource.Target
	desired.Provider = domain.NormalizeTargetProvider(desired.Provider)
	if equalJSON(response.Spec, desired) {
		return "unchanged", nil
	}
	if resource.Kind == "Target" {
		return "", errors.New("existing target differs; an explicit generation change is required")
	}
	if response.Metadata.GenerationID != resource.ExpectedGenerationID || response.Metadata.Revision != resource.ExpectedRevision {
		return "", errors.New("target generation or revision changed")
	}
	return "new-generation", nil
}

func samePolicy(left, right Policy) bool {
	left.IdempotencyRetention = normalizedDuration(left.IdempotencyRetention)
	right.IdempotencyRetention = normalizedDuration(right.IdempotencyRetention)
	left.PublishedOutboxRetention = normalizedDuration(left.PublishedOutboxRetention)
	right.PublishedOutboxRetention = normalizedDuration(right.PublishedOutboxRetention)
	return left == right
}

func normalizedDuration(value string) string {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return "invalid"
	}
	return duration.String()
}

func equalJSON(left, right any) bool {
	a, err := json.Marshal(left)
	b, other := json.Marshal(right)
	return err == nil && other == nil && bytes.Equal(a, b)
}

// Apply checks the complete plan, then performs revision-checked API changes.
// It is deliberately not an atomic transaction across multiple resources.
func (client *Client) Apply(ctx context.Context, document Document, approvedDigest string) (Plan, error) {
	digest, err := document.Digest()
	if err != nil || digest != approvedDigest {
		return Plan{}, errors.New("reviewed resource digest differs")
	}
	plan, err := client.Plan(ctx, document)
	if err != nil {
		return Plan{}, err
	}
	for index, resource := range document.Resources {
		if plan.Operations[index].Action == "unchanged" {
			continue
		}
		// Revalidate instance/recovery and namespace identity before each write.
		namespaces, identityErr := client.verifyIdentity(ctx, document)
		namespace := namespaces[resource.Namespace]
		if identityErr != nil || namespace.ID != resource.NamespaceID || !slices.Contains(namespace.Capabilities, requiredCapability(resource.Kind)) {
			return plan, errors.New("resource authority changed during apply")
		}
		if err := client.applyResource(ctx, resource, digest+"-"+strconv.Itoa(index)); err != nil {
			return plan, err
		}
		plan.Operations[index].Action = "applied"
	}
	return plan, nil
}

func (client *Client) applyResource(ctx context.Context, resource Resource, key string) error {
	path := "/v1/namespaces/" + resource.Namespace
	method := http.MethodPut
	body := map[string]any{"apiVersion": apiVersion, "kind": resource.Kind}
	revision := resource.ExpectedRevision
	switch resource.Kind {
	case "NamespacePolicy":
		path += "/policy"
		body["spec"] = resource.Policy
	case "MembershipGrant":
		path += "/membership-grants/" + resource.ID
		body["spec"] = resource.Grant
	case "Target":
		method = http.MethodPost
		path += "/targets"
		body["metadata"] = map[string]string{"name": resource.Name}
		body["spec"] = resource.Target
	case "TargetGeneration":
		method = http.MethodPost
		path += "/targets/" + resource.Name + "/generations"
		body["spec"] = resource.Target
	default:
		return errors.New("unsupported administrative operation")
	}
	status, err := client.request(ctx, method, path, body, revision, key, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errors.New("administrative resource disappeared")
	}
	return nil
}

package jev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByteDeskAI/bytedesk-jev/contracts/decision"
	pluginsdk "github.com/ByteDeskAI/bytedesk-remote-gateway-plugin-sdk/v2"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/aidecision"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/bus"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/bus/memory"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/hostsettings"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/payloads"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/plugin"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/provideraccess"
)

// This fixture exercises typed transport and job orchestration, not production
// authorization. Native broker isolation is the host's separate acceptance gate.
type fakeHost struct {
	bus.Bus
	mu          sync.Mutex
	payload     map[string][]byte
	handles     map[string]payloads.Handle
	calls       map[string]int
	invocations []string
	response    []byte
	configured  bool
	pending     bool
	badOffset   bool
}

func setupPlugin(t *testing.T) (*Plugin, *fakeHost) {
	t.Helper()
	store := memory.NewStore()
	t.Cleanup(store.Close)
	identity := bus.Identity{PluginID: "jev", Generation: "g1", Role: bus.RolePlugin, Grants: bus.Grants{Publish: []bus.Pattern{">"}, Subscribe: []bus.Pattern{">"}, Request: []bus.Pattern{">"}, Serves: []bus.Pattern{">"}}}
	host := &fakeHost{Bus: store.Connect(identity), payload: map[string][]byte{}, handles: map[string]payloads.Handle{}, calls: map[string]int{}, response: []byte(validMixedResponse), configured: true}
	p := New()
	if err := plugin.Bind(p, plugin.Binding{Bus: host, Identity: identity, Caps: host.Capabilities()}); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := p.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return p, host
}

func decodeFixture[T interface{ Validate() error }](raw []byte) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, v.Validate()
}
func (h *fakeHost) Request(ctx context.Context, subject bus.Subject, raw []byte, opts ...bus.ReqOpt) (*bus.Msg, error) {
	if !strings.HasPrefix(string(subject), "cmd.gateway.") {
		return h.Bus.Request(ctx, subject, raw, opts...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls[string(subject)]++
	var result any
	var invocation struct {
		InvocationID string `json:"invocationId"`
	}
	_ = json.Unmarshal(raw, &invocation)
	if invocation.InvocationID != "" {
		h.invocations = append(h.invocations, invocation.InvocationID)
	}
	switch string(subject) {
	case hostsettings.CommandOwnerRead:
		slots := []string{}
		if h.configured {
			slots = append(slots, "apiKey")
		}
		result = hostsettings.OwnerReadResult{SectionID: "jev", ValuesJSON: `{}`, ConfiguredSecrets: slots}
	case provideraccess.CommandCredential:
		result = provideraccess.CredentialResult{Credential: provideraccess.CredentialHandle{ID: "credential", ProviderID: "jev", Generation: "g1", Slot: "apiKey"}}
	case provideraccess.CommandRevoke:
		result = provideraccess.RevokeResult{Revoked: true}
	case payloads.CommandCreate:
		req, err := decodeFixture[payloads.CreateRequest](raw)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("payload-%d", h.calls[string(subject)])
		handle := payloads.Handle{ID: id, ProviderID: "jev", ProviderGeneration: "g1", Purpose: req.Purpose, Size: req.Size, SHA256: req.SHA256, ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), State: payloads.StateUploading}
		h.handles[id] = handle
		result = payloads.CreateResult{Handle: handle}
	case payloads.CommandAppend:
		req, err := decodeFixture[payloads.AppendRequest](raw)
		if err != nil {
			return nil, err
		}
		if uint64(len(h.payload[req.HandleID])) != req.Offset {
			return nil, fmt.Errorf("fixture append offset")
		}
		part, _ := base64.StdEncoding.DecodeString(req.Data)
		h.payload[req.HandleID] = append(h.payload[req.HandleID], part...)
		offset := uint64(len(h.payload[req.HandleID]))
		if h.badOffset {
			offset++
		}
		result = payloads.AppendResult{NextOffset: offset}
	case payloads.CommandCommit:
		req, err := decodeFixture[payloads.CommitRequest](raw)
		if err != nil {
			return nil, err
		}
		handle := h.handles[req.HandleID]
		sum := sha256.Sum256(h.payload[req.HandleID])
		if uint64(len(h.payload[req.HandleID])) != handle.Size || hex.EncodeToString(sum[:]) != handle.SHA256 {
			return nil, fmt.Errorf("fixture digest mismatch")
		}
		handle.State = payloads.StateCommitted
		h.handles[req.HandleID] = handle
		result = payloads.CommitResult{Handle: handle}
	case payloads.CommandRead:
		req, err := decodeFixture[payloads.ReadRequest](raw)
		if err != nil {
			return nil, err
		}
		body := h.payload[req.HandleID]
		if req.Offset > uint64(len(body)) {
			return nil, fmt.Errorf("fixture read offset")
		}
		end := min(req.Offset+uint64(req.Limit), uint64(len(body)))
		result = payloads.ReadResult{Data: base64.StdEncoding.EncodeToString(body[req.Offset:end]), NextOffset: end, EOF: end == uint64(len(body))}
	case payloads.CommandRevoke:
		req, err := decodeFixture[payloads.RevokeRequest](raw)
		if err != nil {
			return nil, err
		}
		delete(h.payload, req.HandleID)
		delete(h.handles, req.HandleID)
		result = payloads.RevokeResult{Revoked: true}
	case provideraccess.CommandEgressStart:
		req, err := decodeFixture[provideraccess.EgressRequest](raw)
		if err != nil {
			return nil, err
		}
		if req.Operation == provideraccess.OperationEvaluate && h.handles[req.PayloadID].State != payloads.StateCommitted {
			return nil, fmt.Errorf("uncommitted request")
		}
		if req.Operation == provideraccess.OperationModels {
			h.payload["response"] = []byte(`{"models":[{"name":"jev-latest"}]}`)
		} else {
			h.payload["response"] = append([]byte(nil), h.response...)
		}
		job := h.egressJob()
		result = provideraccess.EgressStartResult{Job: job}
	case provideraccess.CommandEgressRead:
		result = provideraccess.EgressReadResult{Job: h.egressJob()}
	case provideraccess.CommandEgressCancel:
		job := h.egressJob()
		job.State = "cancelled"
		job.Result = nil
		result = provideraccess.EgressCancelResult{Job: job}
	default:
		return nil, bus.Fault{Code: bus.FaultUnavailable, Message: "fixture service absent"}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &bus.Msg{Headers: bus.ResolveReq(opts).Headers, Data: encoded}, nil
}
func (h *fakeHost) egressJob() provideraccess.EgressJob {
	job := provideraccess.EgressJob{ID: "egress", State: "completed", DeadlineAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), Result: &provideraccess.EgressResult{StatusCode: 200, PayloadID: "response"}}
	if h.pending {
		job.State = "running"
		job.Result = nil
	}
	return job
}

func waitJob(t *testing.T, p *Plugin, id string) aidecision.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, err := p.readDecision(context.Background(), aidecision.ProviderReadRequest{InvocationID: "invocation", JobID: id}, bus.Caller{})
		if err != nil {
			t.Fatal(err)
		}
		switch result.Job.State {
		case "queued", "running":
			time.Sleep(time.Millisecond)
		default:
			return result.Job
		}
	}
	t.Fatal("provider job did not finish")
	return aidecision.Job{}
}

func TestProviderServicesCompleteMixedBatchThroughScopedHost(t *testing.T) {
	p, h := setupPlugin(t)
	req := aidecision.ProviderStartRequest{InvocationID: "invocation", Batch: mixedBatch()}
	started, err := plugin.Call(context.Background(), h, decision.Start, req)
	if err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, p, started.Job.ID)
	if job.State != "completed" || job.Result == nil || len(job.Result.Answers) != 3 {
		t.Fatalf("job failed: %+v", job)
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	repeat, err := plugin.Call(context.Background(), h, decision.Start, req)
	if err != nil || repeat.Job.ID != job.ID {
		t.Fatalf("idempotency: %v %+v", err, repeat)
	}
	changed := req
	changed.Batch.Model = "jev-latest"
	if _, err := plugin.Call(context.Background(), h, decision.Start, changed); err == nil {
		t.Fatal("conflicting request accepted")
	}
	if _, err := p.readDecision(context.Background(), aidecision.ProviderReadRequest{InvocationID: "foreign", JobID: job.ID}, bus.Caller{}); err == nil {
		t.Fatal("cross-invocation read accepted")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls[provideraccess.CommandEgressStart] != 1 || h.calls[provideraccess.CommandRevoke] != 1 {
		t.Fatalf("unexpected execution/credential lifetime: %+v", h.calls)
	}
	for _, id := range h.invocations {
		if id != "invocation" {
			t.Fatalf("delegation scope lost: %s", id)
		}
	}
	if len(h.payload) != 0 {
		t.Fatalf("transient payloads retained: %d", len(h.payload))
	}
}

func TestProviderCancellationStopsHostEgressAndDoesNotReplay(t *testing.T) {
	p, h := setupPlugin(t)
	h.pending = true
	started, err := p.startDecision(context.Background(), aidecision.ProviderStartRequest{InvocationID: "invocation", Batch: mixedBatch()}, bus.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		h.mu.Lock()
		startedEgress := h.calls[provideraccess.CommandEgressStart] > 0
		h.mu.Unlock()
		if startedEgress {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no egress")
		}
		time.Sleep(time.Millisecond)
	}
	_, err = p.cancelDecision(context.Background(), aidecision.ProviderCancelRequest{InvocationID: "invocation", JobID: started.Job.ID}, bus.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	p.workers.Wait()
	if job := waitJob(t, p, started.Job.ID); job.State != "cancelled" || job.Result != nil {
		t.Fatalf("cancelled job changed: %+v", job)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls[provideraccess.CommandEgressStart] != 1 || h.calls[provideraccess.CommandEgressCancel] != 1 || h.calls[provideraccess.CommandRevoke] != 1 {
		t.Fatalf("cancellation did not clean up: %+v", h.calls)
	}
}

func TestHostBridgeRefusesBadAcknowledgementAndCleansUpload(t *testing.T) {
	_, h := setupPlugin(t)
	h.badOffset = true
	_, err := (hostBridge{bus: h}).upload(context.Background(), "invocation", payloads.PurposeProviderRequest, bytes.Repeat([]byte("x"), 30000))
	if err == nil {
		t.Fatal("bad upload acknowledgement accepted")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls[payloads.CommandRevoke] != 1 || h.calls[payloads.CommandCommit] != 0 {
		t.Fatalf("failed upload retained: %+v", h.calls)
	}
}

func TestMissingCredentialsAndInvalidResponseFailWithoutLeak(t *testing.T) {
	for _, which := range []string{"key", "response"} {
		t.Run(which, func(t *testing.T) {
			p, h := setupPlugin(t)
			if which == "key" {
				h.configured = false
			} else {
				h.response = []byte(`{"error":"private-upstream-body"}`)
			}
			started, err := p.startDecision(context.Background(), aidecision.ProviderStartRequest{InvocationID: "invocation", Batch: mixedBatch()}, bus.Caller{})
			if err != nil {
				t.Fatal(err)
			}
			job := waitJob(t, p, started.Job.ID)
			if job.State != "failed" || job.Failure == nil || strings.Contains(job.Failure.Message, "private-upstream-body") {
				t.Fatalf("failure: %+v", job)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if which == "key" && h.calls[provideraccess.CommandEgressStart] != 0 {
				t.Fatal("egress without configured key")
			}
		})
	}
}

func TestManifestGeneratedAndProviderMetadata(t *testing.T) {
	p, h := setupPlugin(t)
	raw, err := json.MarshalIndent(p.Manifest(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(golden)) {
		t.Fatal("plugin.json is stale; run go run ./cmd/manifest -out plugin.json")
	}
	if _, err := plugin.ParseManifest(golden); err != nil {
		t.Fatal(err)
	}
	declaredCoding := false
	for _, capability := range p.Manifest().Capabilities {
		if capability == pluginsdk.CapabilityProcessSupervised {
			declaredCoding = true
		}
	}
	if !declaredCoding {
		t.Fatal("coding playground must declare the supervised-process capability")
	}
	for _, need := range p.Manifest().Needs {
		if need == pluginsdk.CapabilityProcessSupervised {
			t.Fatal("supervised-process authority belongs in Capabilities, not Needs")
		}
	}
	for _, feature := range []string{pluginsdk.FeatureHostWorkloadAuth, pluginsdk.FeatureAIDecisionV1, pluginsdk.FeatureUIModuleMount} {
		if !strings.Contains(string(raw), feature) {
			t.Fatalf("missing feature %s", feature)
		}
	}
	services, err := h.Services().Discover(context.Background(), decision.Start.Descriptor().Name())
	if err != nil || len(services) != 1 {
		t.Fatalf("discovery: %v %+v", err, services)
	}
	if services[0].Metadata[plugin.MetadataContractSchema] != decision.Start.Descriptor().SchemaHash() || services[0].Endpoints[0].Point != "ai.decision" {
		t.Fatal("provider discovery lost contract")
	}
}

func TestHTTPRejectsMalformedInputAndNeverReturnsSecret(t *testing.T) {
	p, h := setupPlugin(t)
	for _, raw := range []string{`{`, `{"jobId":"a","jobId":"b"}`, `{"jobId":"a","secret":"must-not-leak"}`} {
		recorder := httptest.NewRecorder()
		p.Handler().ServeHTTP(recorder, httptest.NewRequest("POST", "/jev/api/decisions/read", strings.NewReader(raw)))
		if recorder.Code != 400 {
			t.Fatalf("malformed request: %d", recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	p.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/jev/api/status", nil))
	if recorder.Code != 200 || strings.Contains(recorder.Body.String(), "apiKey") {
		t.Fatalf("status exposed credential slot or failed: %d", recorder.Code)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls[aidecision.CommandRead] != 0 {
		t.Fatal("malformed request reached host")
	}
}

func TestModelsRefreshIsAsyncAndReturnsPinnedCatalog(t *testing.T) {
	p, h := setupPlugin(t)
	req := aidecision.ProviderModelsRequest{InvocationID: "invocation", Request: aidecision.ModelsRequest{ProviderID: "jev"}}
	result, err := p.decisionModels(context.Background(), req, bus.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Refreshing {
		t.Fatal("initial model refresh was not asynchronous")
	}
	p.workers.Wait()
	result, err = p.decisionModels(context.Background(), req, bus.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Refreshing || result.Failure != nil || len(result.Models) != 1 || result.Models[0].ID != DefaultModel {
		t.Fatalf("invalid model catalog: %+v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls[provideraccess.CommandEgressStart] != 1 {
		t.Fatal("cached model read repeated upstream call")
	}
}

func TestProviderCapacityPrunesOnlyExpiredFinishedJobs(t *testing.T) {
	p, _ := setupPlugin(t)
	p.mu.Lock()
	for i := range maxProviderJobs {
		id := fmt.Sprintf("old-%d", i)
		p.jobs[id] = &decisionJob{key: id, view: aidecision.Job{ID: id}}
		p.keys[id] = id
	}
	p.mu.Unlock()
	req := aidecision.ProviderStartRequest{InvocationID: "invocation", Batch: mixedBatch()}
	if _, err := p.startDecision(context.Background(), req, bus.Caller{}); err == nil {
		t.Fatal("provider exceeded job capacity")
	}
	p.mu.Lock()
	p.jobs["old-0"].finishedAt = time.Now().Add(-6 * time.Minute)
	p.mu.Unlock()
	result, err := p.startDecision(context.Background(), req, bus.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, p, result.Job.ID); job.State != "completed" {
		t.Fatalf("job after pruning: %+v", job)
	}
}

func TestRoutingDeadlineAndExpiredEgressDoNotReplay(t *testing.T) {
	p, h := setupPlugin(t)
	req := aidecision.ProviderStartRequest{InvocationID: "invocation", Batch: mixedBatch()}
	req.Batch.Purpose = aidecision.PurposeRouting
	result, err := p.startDecision(context.Background(), req, bus.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	created, _ := time.Parse(time.RFC3339Nano, result.Job.CreatedAt)
	deadline, _ := time.Parse(time.RFC3339Nano, result.Job.DeadlineAt)
	if deadline.Sub(created) != 10*time.Second {
		t.Fatal("routing deadline must be ten seconds")
	}
	waitJob(t, p, result.Job.ID)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := (hostBridge{bus: h}).egress(ctx, "invocation", provideraccess.OperationModels, "", "expired"); err == nil {
		t.Fatal("expired egress accepted")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls[provideraccess.CommandEgressStart] != 1 {
		t.Fatal("expired attempt dispatched another request")
	}
}

func TestLargeNormalizedResultUsesScopedPayloadHandle(t *testing.T) {
	p, h := setupPlugin(t)
	var response map[string]any
	_ = json.Unmarshal([]byte(validMixedResponse), &response)
	answers := response["answers"].(map[string]any)
	prototype := answers["choice"]
	response["answers"] = map[string]any{}
	req := mixedBatch()
	req.Questions = nil
	for i := range 100 {
		q := mixedBatch().Questions[0]
		q.ID = fmt.Sprintf("question-%03d-%s", i, strings.Repeat("q", 190))
		req.Questions = append(req.Questions, q)
		response["answers"].(map[string]any)[q.ID] = prototype
	}
	h.response, _ = json.Marshal(response)
	started, err := p.startDecision(context.Background(), aidecision.ProviderStartRequest{InvocationID: "invocation", Batch: req}, bus.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, p, started.Job.ID)
	if job.State != "completed" || job.Result != nil || job.ResultHandleID == "" {
		t.Fatalf("large result not externalized: %+v", job)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	handle := h.handles[job.ResultHandleID]
	if handle.Purpose != payloads.PurposeProviderResponse || handle.State != payloads.StateCommitted {
		t.Fatalf("invalid result handle: %+v", handle)
	}
}

package jev

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/aidecision"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/bus"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/payloads"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/provideraccess"
)

const maxProviderJobs = 256

type decisionJob struct {
	view         aidecision.Job
	invocationID string
	key          string
	fingerprint  string
	cancel       context.CancelFunc
	finishedAt   time.Time
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
func digest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func copyJob(job aidecision.Job) aidecision.Job {
	raw, _ := json.Marshal(job)
	var copied aidecision.Job
	_ = json.Unmarshal(raw, &copied)
	return copied
}

// Provider endpoints are host-only at the broker: the required ai.decision.v1
// feature guarantees publish isolation and rejects foreign subscriptions.
// bd-caller headers are not treated as authorization. InvocationID is correlated
// locally and revalidated by the host on every nested payload/egress operation.
func (p *Plugin) startDecision(_ context.Context, req aidecision.ProviderStartRequest, _ bus.Caller) (aidecision.StartResult, error) {
	var out aidecision.StartResult
	if err := req.Validate(); err != nil {
		return out, bus.Fault{Code: bus.FaultSchema, Message: "invalid Jev provider request"}
	}
	if req.Batch.ProviderID != "jev" {
		return out, bus.Fault{Code: bus.FaultDenied, Message: "provider request is not for Jev"}
	}
	key := digest([]string{req.InvocationID, req.Batch.IdempotencyKey})
	fingerprint := digest(req.Batch)
	p.mu.Lock()
	if !p.active.Load() || p.life == nil || p.life.Err() != nil {
		p.mu.Unlock()
		return out, bus.Fault{Code: bus.FaultUnavailable, Message: "Jev is unavailable"}
	}
	p.pruneJobsLocked()
	if id := p.keys[key]; id != "" {
		job := p.jobs[id]
		if job.fingerprint != fingerprint {
			p.mu.Unlock()
			return out, bus.Fault{Code: bus.FaultConflict, Message: "idempotency key was reused with different input"}
		}
		out.Job = copyJob(job.view)
		p.mu.Unlock()
		return out, nil
	}
	if len(p.jobs) >= maxProviderJobs {
		p.mu.Unlock()
		return out, bus.Fault{Code: bus.FaultBudget, Message: "Jev provider job capacity reached"}
	}
	id, err := randomID()
	if err != nil {
		p.mu.Unlock()
		return out, err
	}
	timeout := time.Duration(aidecision.EvaluationTimeoutSeconds) * time.Second
	if req.Batch.Purpose == aidecision.PurposeRouting {
		timeout = time.Duration(aidecision.RoutingTimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(p.life, timeout)
	now := time.Now().UTC()
	job := &decisionJob{invocationID: req.InvocationID, key: key, fingerprint: fingerprint, cancel: cancel, view: aidecision.Job{ID: id, ProviderID: "jev", State: aidecision.StateQueued, CreatedAt: now.Format(time.RFC3339Nano), DeadlineAt: now.Add(timeout).Format(time.RFC3339Nano)}}
	p.jobs[id] = job
	p.keys[key] = id
	out.Job = copyJob(job.view)
	p.workers.Add(1)
	p.mu.Unlock()
	go p.runDecision(ctx, job, req.Batch)
	return out, nil
}

func (p *Plugin) pruneJobsLocked() {
	for id, job := range p.jobs {
		if !job.finishedAt.IsZero() && time.Since(job.finishedAt) > 5*time.Minute {
			delete(p.jobs, id)
			delete(p.keys, job.key)
		}
	}
}

func (p *Plugin) readDecision(_ context.Context, req aidecision.ProviderReadRequest, _ bus.Caller) (aidecision.ReadResult, error) {
	var out aidecision.ReadResult
	if err := req.Validate(); err != nil {
		return out, bus.Fault{Code: bus.FaultSchema, Message: "invalid Jev job request"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	job := p.jobs[req.JobID]
	if job == nil || job.invocationID != req.InvocationID {
		return out, bus.Fault{Code: bus.FaultNotFound, Message: "Jev job not found in this invocation"}
	}
	out.Job = copyJob(job.view)
	return out, nil
}

func (p *Plugin) cancelDecision(_ context.Context, req aidecision.ProviderCancelRequest, _ bus.Caller) (aidecision.CancelResult, error) {
	var out aidecision.CancelResult
	if err := req.Validate(); err != nil {
		return out, bus.Fault{Code: bus.FaultSchema, Message: "invalid Jev cancellation"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	job := p.jobs[req.JobID]
	if job == nil || job.invocationID != req.InvocationID {
		return out, bus.Fault{Code: bus.FaultNotFound, Message: "Jev job not found in this invocation"}
	}
	if job.view.State == aidecision.StateQueued || job.view.State == aidecision.StateRunning {
		job.cancel()
		job.view.State = aidecision.StateCancelled
		job.finishedAt = time.Now()
	}
	out.Job = copyJob(job.view)
	return out, nil
}

func (p *Plugin) runDecision(ctx context.Context, job *decisionJob, request aidecision.BatchRequest) {
	defer p.workers.Done()
	defer job.cancel()
	host := hostBridge{bus: p.Bus()}
	p.mu.Lock()
	if job.view.State == aidecision.StateQueued {
		job.view.State = aidecision.StateRunning
	}
	p.mu.Unlock()
	var result *aidecision.BatchResult
	var resultHandle string
	run := func() error {
		settings, configured, err := host.settings(ctx)
		if err != nil {
			return err
		}
		if !configured {
			return providerFailure{code: "credentials_unconfigured", message: "Configure the host-owned Typesafe API key in Jev settings"}
		}
		if request.Model != settings.Model {
			return providerFailure{code: "model_not_configured", message: "The requested model is not the administrator-configured Jev model"}
		}
		body, err := EncodeRequest(ctx, request, func(ctx context.Context, id string) ([]byte, error) { return host.read(ctx, job.invocationID, id) })
		if err != nil {
			return providerFailure{code: "invalid_request", message: "The Jev request is invalid or exceeds its conservative model input limit"}
		}
		payloadID, err := host.upload(ctx, job.invocationID, payloads.PurposeProviderRequest, body)
		if err != nil {
			return err
		}
		defer host.revokePayload(job.invocationID, payloadID)
		// The host owns transient retries (at most two) and Retry-After handling
		// within the total deadline. A second retry loop here would multiply cost.
		response, err := host.egress(ctx, job.invocationID, provideraccess.OperationEvaluate, payloadID, digest([]string{job.invocationID, request.IdempotencyKey}))
		if err != nil {
			return err
		}
		decoded, err := DecodeResponse(request, response)
		if err != nil {
			return providerFailure{code: "invalid_response", message: "Typesafe returned an incomplete, invalid or mismatched decision result"}
		}
		raw, err := json.Marshal(decoded)
		if err != nil {
			return err
		}
		if len(raw) > payloads.MaxInlineTextBytes {
			resultHandle, err = host.upload(ctx, job.invocationID, payloads.PurposeProviderResponse, raw)
			return err
		}
		result = &decoded
		return nil
	}
	err := run()
	p.mu.Lock()
	if job.view.State == aidecision.StateCancelled {
		p.mu.Unlock()
		if resultHandle != "" {
			host.revokePayload(job.invocationID, resultHandle)
		}
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		job.view.State = aidecision.StateExpired
	} else if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		job.view.State = aidecision.StateCancelled
	} else if err != nil {
		job.view.State = aidecision.StateFailed
		failure := providerFailure{code: "host_unavailable", message: "A required host operation is unavailable or was denied"}
		var known providerFailure
		if errors.As(err, &known) {
			failure = known
		}
		job.view.Failure = &aidecision.Failure{Code: failure.code, Message: failure.message, Retryable: failure.retryable}
	} else {
		job.view.State = aidecision.StateCompleted
		job.view.Result = result
		job.view.ResultHandleID = resultHandle
	}
	job.finishedAt = time.Now()
	completed := job.view.State == aidecision.StateCompleted
	p.mu.Unlock()
	if !completed && resultHandle != "" {
		host.revokePayload(job.invocationID, resultHandle)
	}
}

func (p *Plugin) decisionModels(_ context.Context, req aidecision.ProviderModelsRequest, _ bus.Caller) (aidecision.ModelsResult, error) {
	if err := req.Validate(); err != nil || req.Request.ProviderID != "jev" {
		return aidecision.ModelsResult{}, bus.Fault{Code: bus.FaultSchema, Message: "invalid Jev model request"}
	}
	p.mu.Lock()
	if !p.active.Load() || p.life == nil || p.life.Err() != nil {
		p.mu.Unlock()
		return aidecision.ModelsResult{}, bus.Fault{Code: bus.FaultUnavailable, Message: "Jev is unavailable"}
	}
	if !p.modelsLoading && (p.models.ObservedAt == "" || modelCacheExpired(p.models.ObservedAt)) {
		p.modelsLoading = true
		p.models.Refreshing = true
		p.models.ProviderID = "jev"
		p.models.Failure = nil
		ctx, cancel := context.WithTimeout(p.life, 30*time.Second)
		p.workers.Add(1)
		go p.refreshModels(ctx, cancel, req.InvocationID)
	}
	raw, _ := json.Marshal(p.models)
	var out aidecision.ModelsResult
	_ = json.Unmarshal(raw, &out)
	p.mu.Unlock()
	return out, nil
}

func modelCacheExpired(value string) bool {
	at, err := time.Parse(time.RFC3339Nano, value)
	return err != nil || time.Since(at) > 5*time.Minute
}

func (p *Plugin) refreshModels(ctx context.Context, cancel context.CancelFunc, invocationID string) {
	defer p.workers.Done()
	defer cancel()
	host := hostBridge{bus: p.Bus()}
	models := aidecision.ModelsResult{ProviderID: "jev", Models: []aidecision.DecisionModel{}}
	run := func() error {
		settings, configured, err := host.settings(ctx)
		if err != nil {
			return err
		}
		if !configured {
			return errors.New("API key is not configured")
		}
		body, err := host.egress(ctx, invocationID, provideraccess.OperationModels, "", digest([]string{invocationID, "models"}))
		if err != nil {
			return err
		}
		if uniqueJSON(body) != nil {
			return errors.New("invalid models response")
		}
		var response struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if json.Unmarshal(body, &response) != nil || response.Models == nil || len(response.Models) > 256 {
			return errors.New("invalid models response")
		}
		// Typesafe documents that pinned version IDs remain valid even though
		// GET /models currently lists aliases only. An alias must be advertised.
		found := settings.Model == DefaultModel
		for _, model := range response.Models {
			found = found || model.Name == settings.Model
		}
		if !found {
			return errors.New("configured model is not available")
		}
		models.Models = []aidecision.DecisionModel{{ID: settings.Model, Label: settings.Model, Kinds: []string{aidecision.KindChoice, aidecision.KindScore, aidecision.KindNoul}}}
		return nil
	}
	if err := run(); err != nil {
		models.Failure = &aidecision.Failure{Code: "models_unavailable", Message: "Jev model discovery is unavailable; check host readiness and the configured API key", Retryable: true}
	}
	models.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	p.mu.Lock()
	p.models = models
	p.modelsLoading = false
	p.mu.Unlock()
}

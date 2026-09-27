package jev

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/bus"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/hostsettings"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/payloads"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/provideraccess"
)

type hostBridge struct{ bus bus.Bus }

func (h hostBridge) settings(ctx context.Context) (Settings, bool, error) {
	reply, err := hostsettings.Call(ctx, h.bus, hostsettings.OwnerRead, hostsettings.OwnerReadRequest{SectionID: "jev"})
	if err != nil {
		return Settings{}, false, err
	}
	if reply.SectionID != "jev" {
		return Settings{}, false, errors.New("host returned another settings owner")
	}
	settings, err := ParseSettings(reply.ValuesJSON)
	configured := false
	for _, slot := range reply.ConfiguredSecrets {
		configured = configured || slot == "apiKey"
	}
	return settings, configured, err
}

func (h hostBridge) read(ctx context.Context, invocationID, id string) ([]byte, error) {
	var out []byte
	for {
		reply, err := payloads.Call(ctx, h.bus, payloads.Read, payloads.ReadRequest{InvocationID: invocationID, HandleID: id, Offset: uint64(len(out)), Limit: payloads.MaxChunkBytes})
		if err != nil {
			return nil, err
		}
		chunk, err := base64.StdEncoding.Strict().DecodeString(reply.Data)
		if err != nil {
			return nil, errors.New("host returned invalid payload encoding")
		}
		if len(chunk) > payloads.MaxChunkBytes || reply.NextOffset != uint64(len(out)+len(chunk)) || len(chunk) > payloads.MaxAssembledBytes-len(out) {
			return nil, errors.New("host returned invalid payload progress")
		}
		out = append(out, chunk...)
		if reply.EOF {
			return out, nil
		}
		if len(chunk) == 0 {
			return nil, errors.New("host payload read made no progress")
		}
	}
}

func (h hostBridge) revokePayload(invocationID, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = payloads.Call(ctx, h.bus, payloads.Revoke, payloads.RevokeRequest{InvocationID: invocationID, HandleID: id})
}

func (h hostBridge) upload(ctx context.Context, invocationID, purpose string, body []byte) (string, error) {
	if len(body) == 0 || len(body) > payloads.MaxAssembledBytes {
		return "", errors.New("payload exceeds host bounds")
	}
	digest := sha256.Sum256(body)
	created, err := payloads.Call(ctx, h.bus, payloads.Create, payloads.CreateRequest{InvocationID: invocationID, ProviderID: "jev", Purpose: purpose, Size: uint64(len(body)), SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		return "", err
	}
	id := created.Handle.ID
	committed := false
	defer func() {
		if !committed {
			h.revokePayload(invocationID, id)
		}
	}()
	if created.Handle.ProviderID != "jev" || created.Handle.Purpose != purpose || created.Handle.Size != uint64(len(body)) || created.Handle.SHA256 != hex.EncodeToString(digest[:]) {
		return "", errors.New("host created a different payload")
	}
	for offset := 0; offset < len(body); {
		end := min(offset+payloads.MaxChunkBytes, len(body))
		reply, err := payloads.Call(ctx, h.bus, payloads.Append, payloads.AppendRequest{InvocationID: invocationID, HandleID: id, Offset: uint64(offset), Data: base64.StdEncoding.EncodeToString(body[offset:end])})
		if err != nil {
			return "", err
		}
		if reply.NextOffset != uint64(end) {
			return "", errors.New("host acknowledged an unexpected payload offset")
		}
		offset = end
	}
	reply, err := payloads.Call(ctx, h.bus, payloads.Commit, payloads.CommitRequest{InvocationID: invocationID, HandleID: id})
	if err != nil {
		return "", err
	}
	if reply.Handle.ID != id || reply.Handle.ProviderID != "jev" || reply.Handle.ProviderGeneration != created.Handle.ProviderGeneration || reply.Handle.Purpose != purpose || reply.Handle.SHA256 != hex.EncodeToString(digest[:]) || reply.Handle.Size != uint64(len(body)) {
		return "", errors.New("host committed a different payload")
	}
	committed = true
	return id, nil
}

type providerFailure struct {
	code, message string
	retryable     bool
}

func (e providerFailure) Error() string { return e.message }

func (h hostBridge) egress(ctx context.Context, invocationID, operation, payloadID, key string) ([]byte, error) {
	credential, err := provideraccess.Call(ctx, h.bus, provideraccess.Credential, provideraccess.CredentialRequest{Slot: "apiKey"})
	if err != nil {
		return nil, providerFailure{code: "credentials_unavailable", message: "Jev credentials are unavailable; configure the host-owned API key"}
	}
	if credential.Credential.ProviderID != "jev" || credential.Credential.Slot != "apiKey" {
		return nil, errors.New("host credential handle belongs to another provider")
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = provideraccess.Call(clean, h.bus, provideraccess.Revoke, provideraccess.RevokeRequest{CredentialID: credential.Credential.ID})
	}()
	seconds := uint32(provideraccess.MaxTimeoutSeconds)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		seconds = uint32(min(provideraccess.MaxTimeoutSeconds, max(1, int(remaining/time.Second))))
	}
	started, err := provideraccess.Call(ctx, h.bus, provideraccess.EgressStart, provideraccess.EgressRequest{InvocationID: invocationID, CredentialID: credential.Credential.ID, Operation: operation, PayloadID: payloadID, IdempotencyKey: key, TimeoutSeconds: seconds})
	if err != nil {
		return nil, err
	}
	job := started.Job
	finished := false
	defer func() {
		if !finished {
			clean, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = provideraccess.Call(clean, h.bus, provideraccess.EgressCancel, provideraccess.EgressCancelRequest{InvocationID: invocationID, JobID: job.ID})
		}
	}()
	for {
		switch job.State {
		case "completed":
			finished = true
			if job.Result == nil {
				return nil, errors.New("host egress omitted its completed result")
			}
			defer h.revokePayload(invocationID, job.Result.PayloadID)
			if job.Result.StatusCode < 200 || job.Result.StatusCode >= 300 {
				return nil, statusFailure(job.Result.StatusCode)
			}
			return h.read(ctx, invocationID, job.Result.PayloadID)
		case "failed":
			finished = true
			return nil, providerFailure{code: "upstream_unavailable", message: "The host could not complete the Jev API operation", retryable: job.Failure != nil && job.Failure.Retryable}
		case "cancelled":
			finished = true
			return nil, context.Canceled
		case "expired":
			finished = true
			return nil, context.DeadlineExceeded
		case "queued", "running":
		default:
			return nil, errors.New("host returned an unknown egress state")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		reply, err := provideraccess.Call(ctx, h.bus, provideraccess.EgressRead, provideraccess.EgressReadRequest{InvocationID: invocationID, JobID: job.ID})
		if err != nil {
			return nil, err
		}
		if reply.Job.ID != job.ID {
			return nil, errors.New("host returned another egress job")
		}
		job = reply.Job
	}
}

func statusFailure(status uint32) error {
	switch status {
	case 401, 403:
		return providerFailure{code: "credentials_invalid", message: "Typesafe rejected the configured API key"}
	case 422:
		return providerFailure{code: "upstream_validation", message: "Typesafe rejected the request; check the rubric and model input limits"}
	case 429, 529:
		return providerFailure{code: "upstream_busy", message: "Typesafe is rate limited or overloaded", retryable: true}
	default:
		return providerFailure{code: "upstream_error", message: fmt.Sprintf("Typesafe returned HTTP %d", status), retryable: status >= 500}
	}
}

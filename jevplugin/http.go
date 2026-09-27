package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ByteDeskAI/bytedesk-remote-gateway-plugin-sdk/v2/transport/natsconn"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/aidecision"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/bus"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/codingsessions"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/hostsettings"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/payloads"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/provideraccess"
)

func hostRequests() []bus.Pattern {
	return []bus.Pattern{
		provideraccess.CommandCredential, provideraccess.CommandRevoke, provideraccess.CommandEgressStart, provideraccess.CommandEgressRead, provideraccess.CommandEgressCancel,
		payloads.CommandCreate, payloads.CommandAppend, payloads.CommandCommit, payloads.CommandRead, payloads.CommandRevoke, hostsettings.CommandOwnerRead,
		aidecision.CommandStart, aidecision.CommandRead, aidecision.CommandCancel, aidecision.CommandModels,
		codingsessions.CommandCatalog, codingsessions.CommandCreate, codingsessions.CommandRead, codingsessions.CommandPrompt, codingsessions.CommandEvents,
		codingsessions.CommandStop, codingsessions.CommandEnd, codingsessions.CommandComplete, codingsessions.CommandApprove, codingsessions.CommandOpenSurface,
		codingsessions.CommandPreview, codingsessions.CommandPreviewRead, codingsessions.CommandPreviewCancel,
	}
}

type validRequest interface{ Validate() error }

func jsonEndpoint[Req validRequest, Resp any](call func(context.Context, Req) (Resp, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 60<<10))
		if err != nil {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if uniqueJSON(body) != nil {
			http.Error(w, "invalid JSON request", http.StatusBadRequest)
			return
		}
		var req Req
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&req) != nil || req.Validate() != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		ctx := natsconn.ContextForRequest(r)
		if _, err := natsconn.SubjectLeaseFromContext(ctx); err != nil {
			http.Error(w, "invalid host authorization", http.StatusForbidden)
			return
		}
		result, err := call(ctx, req)
		if err != nil {
			writeHostFailure(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func writeHostFailure(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	var fault bus.Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case bus.FaultDenied:
			status = http.StatusForbidden
		case bus.FaultSchema:
			status = http.StatusBadRequest
		case bus.FaultNotFound:
			status = http.StatusNotFound
		case bus.FaultConflict:
			status = http.StatusConflict
		case bus.FaultBudget:
			status = http.StatusTooManyRequests
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "host_unavailable", "message": "The host operation is unavailable or was denied. No local fallback was attempted."})
}

func (p *Plugin) Handler() http.Handler {
	b := p.Bus()
	routes := map[string]http.Handler{
		"/jev/api/decisions/start": jsonEndpoint(func(ctx context.Context, req aidecision.BatchRequest) (aidecision.StartResult, error) {
			return aidecision.Call(ctx, b, aidecision.Start, req)
		}),
		"/jev/api/decisions/read": jsonEndpoint(func(ctx context.Context, req aidecision.ReadRequest) (aidecision.ReadResult, error) {
			return aidecision.Call(ctx, b, aidecision.Read, req)
		}),
		"/jev/api/decisions/cancel": jsonEndpoint(func(ctx context.Context, req aidecision.CancelRequest) (aidecision.CancelResult, error) {
			return aidecision.Call(ctx, b, aidecision.Cancel, req)
		}),
		"/jev/api/decisions/models": jsonEndpoint(func(ctx context.Context, req aidecision.ModelsRequest) (aidecision.ModelsResult, error) {
			return aidecision.Call(ctx, b, aidecision.Models, req)
		}),
		"/jev/api/coding/catalog": jsonEndpoint(func(ctx context.Context, req codingsessions.CatalogRequest) (codingsessions.CatalogResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Catalog, req)
		}),
		"/jev/api/coding/preview": jsonEndpoint(func(ctx context.Context, req codingsessions.PreviewRequest) (codingsessions.PreviewJobResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Preview, req)
		}),
		"/jev/api/coding/preview-read": jsonEndpoint(func(ctx context.Context, req codingsessions.PreviewJobRequest) (codingsessions.PreviewJobResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.PreviewRead, req)
		}),
		"/jev/api/coding/preview-cancel": jsonEndpoint(func(ctx context.Context, req codingsessions.PreviewJobRequest) (codingsessions.PreviewJobResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.PreviewCancel, req)
		}),
		"/jev/api/coding/create": jsonEndpoint(func(ctx context.Context, req codingsessions.CreateRequest) (codingsessions.SessionResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Create, req)
		}),
		"/jev/api/coding/read": jsonEndpoint(func(ctx context.Context, req codingsessions.SessionRequest) (codingsessions.SessionResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Read, req)
		}),
		"/jev/api/coding/prompt": jsonEndpoint(func(ctx context.Context, req codingsessions.PromptRequest) (codingsessions.PromptResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Prompt, req)
		}),
		"/jev/api/coding/events": jsonEndpoint(func(ctx context.Context, req codingsessions.EventsRequest) (codingsessions.EventsResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Events, req)
		}),
		"/jev/api/coding/stop": jsonEndpoint(func(ctx context.Context, req codingsessions.StopRequest) (codingsessions.SessionResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Stop, req)
		}),
		"/jev/api/coding/end": jsonEndpoint(func(ctx context.Context, req codingsessions.SessionRequest) (codingsessions.SessionResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.End, req)
		}),
		"/jev/api/coding/complete": jsonEndpoint(func(ctx context.Context, req codingsessions.SessionRequest) (codingsessions.SessionResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Complete, req)
		}),
		"/jev/api/coding/approve": jsonEndpoint(func(ctx context.Context, req codingsessions.ApproveRequest) (codingsessions.SessionResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.Approve, req)
		}),
		"/jev/api/coding/open-surface": jsonEndpoint(func(ctx context.Context, req codingsessions.OpenSurfaceRequest) (codingsessions.OpenSurfaceResult, error) {
			return codingsessions.Call(ctx, b, codingsessions.OpenSurface, req)
		}),
		"/jev/api/payload/read": jsonEndpoint(func(ctx context.Context, req payloads.ReadRequest) (payloads.ReadResult, error) {
			return payloads.Call(ctx, b, payloads.Read, req)
		}),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !p.active.Load() {
			http.Error(w, "Jev plugin unavailable", http.StatusServiceUnavailable)
			return
		}
		if handler := routes[r.URL.Path]; handler != nil {
			handler.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/jev/panel.mjs":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			_, _ = w.Write(panelModule)
		case "/jev/panel.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			_, _ = w.Write(panelStyles)
		case "/healthz":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok"))
		case "/jev/api/status":
			ctx := natsconn.ContextForRequest(r)
			if _, err := natsconn.SubjectLeaseFromContext(ctx); err != nil {
				http.Error(w, "invalid host authorization", http.StatusForbidden)
				return
			}
			settings, configured, err := (hostBridge{bus: b}).settings(ctx)
			if err != nil {
				writeHostFailure(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(struct {
				Configured bool     `json:"configured"`
				Settings   Settings `json:"settings"`
			}{configured, settings})
		case "/jev/", "/jev/ui":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>Jev playground</title></head><body><h1>Jev playground</h1><p>Open this plugin through the authenticated Gateway panel. The host supplies the module lifecycle and authorization.</p></body></html>`))
		default:
			http.NotFound(w, r)
		}
	})
}

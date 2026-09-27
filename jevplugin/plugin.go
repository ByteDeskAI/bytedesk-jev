// Package jev implements the Store-installable Jev decision provider. The host
// owns all credentials, network traffic, payload storage and coding execution.
package jev

import (
	"context"
	_ "embed"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/ByteDeskAI/bytedesk-jev/contracts/decision"
	settingscontract "github.com/ByteDeskAI/bytedesk-jev/contracts/settings"
	pluginsdk "github.com/ByteDeskAI/bytedesk-remote-gateway-plugin-sdk/v2"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/aidecision"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/bus"
	"github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/hostsettings"
	commonplugin "github.com/ByteDeskAI/bytedesk-sdk-dependencies/v2/plugin"
)

const Version = "0.1.0"

//go:embed panel.mjs
var panelModule []byte

//go:embed panel.css
var panelStyles []byte

type Plugin struct {
	pluginsdk.Base
	active        atomic.Bool
	mu            sync.Mutex
	services      []bus.Service
	life          context.Context
	cancel        context.CancelFunc
	workers       sync.WaitGroup
	jobs          map[string]*decisionJob
	keys          map[string]string
	models        aidecision.ModelsResult
	modelsLoading bool
}

func New() *Plugin         { return &Plugin{jobs: map[string]*decisionJob{}, keys: map[string]string{}} }
func (*Plugin) ID() string { return "jev" }

func serviceDeclaration(d commonplugin.Descriptor, point commonplugin.Point) commonplugin.ServiceDecl {
	return commonplugin.ServiceDecl{Name: d.Name(), Version: "1.0.0", Endpoints: []commonplugin.EndpointDecl{{Name: d.Name(), Subject: d.Subject(), Point: string(point)}}}
}

func (p *Plugin) Manifest() pluginsdk.Manifest {
	return pluginsdk.Manifest{
		Contract: pluginsdk.ProtocolMajor, Kind: pluginsdk.KindProcess, ID: p.ID(), Version: Version,
		Identity:  &commonplugin.ManifestIdentity{DisplayName: "Jev", Description: "Reusable Choice, Score and Noul AI decisions with a host-held Typesafe key and an interactive playground."},
		Publisher: &commonplugin.Publisher{ID: "bytedesk", Name: "ByteDesk"}, Targets: []string{pluginsdk.TargetGateway}, Role: pluginsdk.RoleExtension,
		Binary: "jev", Socket: "plugin.sock", When: commonplugin.When{OS: []string{"linux"}}, Routes: []string{"/jev/"}, Scopes: []string{"plugin:jev"},
		Nav:      []commonplugin.NavItem{{ID: "jev", Label: "Jev playground", Href: "/plugins/jev", Order: 90}},
		Panels:   []commonplugin.PanelSpec{{ID: "jev", Kind: "jev", URL: "/jev/ui", Module: "/jev/panel.mjs", DocumentPaths: []string{"/jev"}}},
		Protocol: &commonplugin.ProtocolRequirements{Major: pluginsdk.ProtocolMajor, Required: []string{pluginsdk.FeatureHTTPRoutes, pluginsdk.FeatureUIModuleMount, commonplugin.FeatureDocumentPaths, pluginsdk.FeatureHostWorkloadAuth, pluginsdk.FeatureAIDecisionV1}, Hooks: []string{"activation.check"}},
		Needs:    []string{"services"}, Capabilities: []string{commonplugin.CapabilityCredentialSecret, commonplugin.CapabilityEgressProvider, pluginsdk.CapabilityProcessSupervised},
		Implements:  []commonplugin.Provider{{Point: string(commonplugin.PointAIDecision), ID: "jev", Priority: 100}, {Point: string(commonplugin.PointSettingsSection), ID: "jev", Priority: 100}},
		Config:      &commonplugin.Config{Sections: []commonplugin.ConfigSection{{ID: "jev", Title: "Jev", Description: "One host-held API key. Jev configuration and usage limits apply to this Gateway.", Fields: settingsFields()}}},
		Permissions: commonplugin.Permissions{Request: hostRequests()},
		Serves: []commonplugin.ServiceDecl{
			serviceDeclaration(decision.Start.Descriptor(), commonplugin.PointAIDecision), serviceDeclaration(decision.Read.Descriptor(), commonplugin.PointAIDecision),
			serviceDeclaration(decision.Cancel.Descriptor(), commonplugin.PointAIDecision), serviceDeclaration(decision.Models.Descriptor(), commonplugin.PointAIDecision),
			serviceDeclaration(settingscontract.Validate.Descriptor(), commonplugin.PointSettingsSection),
		},
	}
}

func (p *Plugin) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.life != nil {
		p.mu.Unlock()
		return errors.New("Jev plugin already started")
	}
	p.life, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()
	mounts := []func() (bus.Service, error){
		func() (bus.Service, error) {
			return commonplugin.ServeAtPoint(ctx, p.Bus(), decision.Start, commonplugin.PointAIDecision, p.startDecision)
		},
		func() (bus.Service, error) {
			return commonplugin.ServeAtPoint(ctx, p.Bus(), decision.Read, commonplugin.PointAIDecision, p.readDecision)
		},
		func() (bus.Service, error) {
			return commonplugin.ServeAtPoint(ctx, p.Bus(), decision.Cancel, commonplugin.PointAIDecision, p.cancelDecision)
		},
		func() (bus.Service, error) {
			return commonplugin.ServeAtPoint(ctx, p.Bus(), decision.Models, commonplugin.PointAIDecision, p.decisionModels)
		},
		func() (bus.Service, error) {
			return commonplugin.ServeAtPoint(ctx, p.Bus(), settingscontract.Validate, commonplugin.PointSettingsSection, func(_ context.Context, req hostsettings.ValidateRequest, _ bus.Caller) (hostsettings.ValidateResult, error) {
				return ValidateSettings(req), nil
			})
		},
	}
	for _, mount := range mounts {
		service, err := mount()
		if err != nil {
			_ = p.Stop(context.Background())
			return err
		}
		p.mu.Lock()
		p.services = append(p.services, service)
		p.mu.Unlock()
	}
	p.active.Store(true)
	return nil
}
func (p *Plugin) CheckActivation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.active.Load() {
		return errors.New("Jev plugin is not started")
	}
	// Missing credentials must not hide settings/onboarding. Individual provider
	// jobs remain unavailable until the host can resolve the configured slot.
	return nil
}
func (p *Plugin) Stop(ctx context.Context) error {
	p.active.Store(false)
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	services := p.services
	p.services = nil
	p.mu.Unlock()
	var failures []error
	for _, service := range services {
		if err := service.Stop(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	done := make(chan struct{})
	go func() { p.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		failures = append(failures, ctx.Err())
	}
	return errors.Join(failures...)
}

var _ pluginsdk.Plugin = (*Plugin)(nil)
var _ pluginsdk.HTTPPlugin = (*Plugin)(nil)
var _ pluginsdk.ActivationChecker = (*Plugin)(nil)

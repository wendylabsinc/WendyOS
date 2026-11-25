package services

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

type AppWorldSocketManager struct {
	ctx           context.Context
	module        *agentmodule.Manager
	trace         *rpctrace.Sink
	replicaMu     sync.Mutex
	replicaCancel context.CancelFunc
	replicaDone   chan struct{}
	replicaClosed bool
}

func NewAppWorldSocketManager(ctx context.Context, path string, provisioning *ProvisioningService) (*AppWorldSocketManager, error) {
	trace := worldRPCTiming(ctx)
	module, err := agentmodule.LoadWithOptions(ctx, path, worldIdentity{provisioning}, verifyWorldPeer, agentmodule.HostOptions{Trace: trace})
	if err != nil || module == nil {
		if trace != nil {
			trace.Close()
		}
		return nil, err
	}
	return &AppWorldSocketManager{ctx: ctx, module: module, trace: trace}, nil
}
func worldScopes(publish []appconfig.WorldPublication) []serviceorigin.PublishScope {
	out := make([]serviceorigin.PublishScope, len(publish))
	for i, p := range publish {
		out[i] = serviceorigin.PublishScope{ComponentType: p.ComponentType, Stream: p.Stream}
	}
	return out
}
func worldSubscriptions(subscribe []appconfig.WorldSubscription) []serviceaccess.Source {
	out := make([]serviceaccess.Source, len(subscribe))
	for i, s := range subscribe {
		out[i] = serviceaccess.Source{App: s.App, Service: s.Service, ComponentType: s.ComponentType, Stream: s.Stream, Device: s.Device}
	}
	return out
}
func (m *AppWorldSocketManager) Ensure(app, service string, generation uint64, publish []appconfig.WorldPublication) (string, error) {
	return m.EnsureAccess(app, service, generation, publish, nil)
}
func (m *AppWorldSocketManager) EnsureAccess(app, service string, generation uint64, publish []appconfig.WorldPublication, subscribe []appconfig.WorldSubscription) (string, error) {
	mount, err := m.module.EnsureAccess(m.ctx, app, service, generation, worldScopes(publish), worldSubscriptions(subscribe))
	return mount.Directory, err
}
func (m *AppWorldSocketManager) ValidateMount(app, service string, generation uint64, directory string, publish []appconfig.WorldPublication) error {
	return m.ValidateAccessMount(app, service, generation, directory, publish, nil)
}
func (m *AppWorldSocketManager) ValidateAccessMount(app, service string, generation uint64, directory string, publish []appconfig.WorldPublication, subscribe []appconfig.WorldSubscription) error {
	return m.module.ValidateAccessMount(m.ctx, app, service, generation, directory, worldScopes(publish), worldSubscriptions(subscribe))
}
func (m *AppWorldSocketManager) Release(app, service string, generation uint64) {
	m.module.Release(app, service, generation)
}
func (m *AppWorldSocketManager) ReleaseApp(app string) { m.module.ReleaseApp(app) }
func (m *AppWorldSocketManager) Close() error {
	m.stopReplication()
	err := m.module.Close()
	if m.trace != nil {
		m.trace.Close()
	}
	return err
}

func verifyWorldPeer(ctx context.Context, peer appservice.Peer, scope serviceorigin.Scope) error {
	if ctx.Err() != nil || peer.PID <= 0 {
		return agentmodule.ErrDenied
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", peer.PID))
	if err != nil {
		return agentmodule.ErrDenied
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		return agentmodule.ErrDenied
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return matchWorldCgroup(string(body), sharedenv.SystemdServiceName(), scope)
}

// SO_PEERPIDFD in appservice pins the original process while this function
// reads its cgroup. A UID, app-only prefix or caller-supplied ID is insufficient.
func matchWorldCgroup(body, servicePrefix string, scope serviceorigin.Scope) error {
	suffix, err := appconfig.WorldCgroupSuffix(scope.App, scope.Service, scope.DeploymentGeneration)
	if err != nil {
		return agentmodule.ErrDenied
	}
	want := servicePrefix + "-" + suffix + ".scope"
	literalPrefix := "system.slice:" + servicePrefix + ":"
	wantLiteral := literalPrefix + suffix
	found := false
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "/") {
			return agentmodule.ErrDenied
		}
		for _, part := range strings.Split(parts[2], "/") {
			if strings.HasPrefix(part, literalPrefix) {
				if part != wantLiteral {
					return agentmodule.ErrDenied
				}
				found = true
				continue
			}
			if strings.HasPrefix(part, servicePrefix+"-") && strings.HasSuffix(part, ".scope") {
				if part != want {
					return agentmodule.ErrDenied
				}
				found = true
			}
		}
	}
	if !found {
		return agentmodule.ErrDenied
	}
	return nil
}

package containerd

import (
	"context"
	"errors"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
)

type ingressRecoveryContainer struct{ staleContainerForTest }

func (ingressRecoveryContainer) ID() string { return "sh.wendy.recovery" }

func TestMeshIngressRecoveryScopesBackgroundRetryToNamespace(t *testing.T) {
	called := false
	ctr := ingressRecoveryContainer{staleContainerForTest{taskFn: func(ctx context.Context) (containerd.Task, error) {
		called = true
		ns, ok := namespaces.Namespace(ctx)
		if !ok || ns != "wendy-test" {
			t.Errorf("recovery task lookup lost its containerd namespace: %q, %v", ns, ok)
		}
		return nil, errors.New("end test before task/kernel inspection")
	}}}
	client := &Client{logger: zap.NewNop(), namespace: "wendy-test", meshIngress: meshingress.NewRegistry()}
	labels := wendyLabels(ctr.ID(), "", "test", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh"}}, "isolated", nil)
	client.rehydrateRunningMeshIngressAttempt(context.Background(), ctr, labels, 0)
	if !called {
		t.Fatal("recovery never reached its containerd lookup")
	}
}

func TestMeshIngressRecoveryHonorsStopDuringRetry(t *testing.T) {
	ctr := ingressRecoveryContainer{staleContainerForTest{taskFn: func(context.Context) (containerd.Task, error) {
		t.Error("a delayed recovery inspected a task after its owner stopped the app")
		return nil, errors.New("stopped tasks must remain denied")
	}}}
	registry := meshingress.NewRegistry()
	client := &Client{logger: zap.NewNop(), namespace: "wendy-test", meshIngress: registry}
	labels := wendyLabels(ctr.ID(), "", "test", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh"}}, "isolated", nil)
	labels[labelKeyStoppedByUser] = "true"
	client.rehydrateRunningMeshIngressAttempt(context.Background(), ctr, labels, 0)
	if registry.Allowed(8080) {
		t.Fatal("explicitly stopped task regained ingress")
	}
}

type blockedIngressRetryContainer struct {
	ingressRecoveryContainer
	labels func(context.Context) (map[string]string, error)
}

func (c blockedIngressRetryContainer) Labels(ctx context.Context) (map[string]string, error) {
	return c.labels(ctx)
}

func TestMeshIngressRecoveryCancelsBlockedRetryOnShutdown(t *testing.T) {
	entered, cleanup := make(chan struct{}), make(chan struct{})
	defer close(cleanup)
	client := &Client{logger: zap.NewNop(), namespace: "wendy-test", meshIngressStop: make(chan struct{})}
	ctr := blockedIngressRetryContainer{labels: func(ctx context.Context) (map[string]string, error) {
		if _, bounded := ctx.Deadline(); !bounded {
			t.Error("background recovery RPC has no deadline")
		}
		if ns, _ := namespaces.Namespace(ctx); ns != "wendy-test" {
			t.Error("background recovery RPC lost its namespace")
		}
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-cleanup:
			return nil, errors.New("test cleanup")
		}
	}}
	done := make(chan struct{})
	go func() { client.retryRunningMeshIngress(ctr, 0); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retry did not start")
	}
	close(client.meshIngressStop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent shutdown left its recovery RPC blocked")
	}
}

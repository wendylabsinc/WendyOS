package containerd

import (
	"context"
	"testing"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"go.uber.org/zap"
)

type shutdownExitStore struct {
	containers.Store
	record  containers.Container
	updates int
}

func (s *shutdownExitStore) Get(context.Context, string) (containers.Container, error) {
	return s.record, nil
}
func (s *shutdownExitStore) Update(_ context.Context, record containers.Container, _ ...string) (containers.Container, error) {
	s.record = record
	s.updates++
	return record, nil
}

func TestHostShutdownDoesNotOverwriteAppExitDiagnostics(t *testing.T) {
	for _, shuttingDown := range []bool{false, true} {
		name := "ordinary exit"
		if shuttingDown {
			name = "host shutdown"
		}
		t.Run(name, func(t *testing.T) {
			store := &shutdownExitStore{record: containers.Container{ID: "app", Labels: map[string]string{labelKeyExitCode: "7", labelKeyExitReason: exitReasonCrashed}}}
			remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(store)))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			client := &Client{client: remote, namespace: "wendy", logger: zap.NewNop()}
			client.hostShutdown.Store(shuttingDown)
			owner := client.trackTaskOutput("app", 1234)
			client.withTaskOutputOwner("app", owner, func() {
				client.recordContainerExit(context.Background(), "app", 137, exitReasonCrashed, time.Now())
			})
			code, _, ok := parseExitLabels(store.record.Labels)
			if !ok {
				t.Fatal("previous exit diagnostics disappeared")
			}
			if shuttingDown {
				if store.updates != 0 || code != 7 {
					t.Fatalf("intentional host stop overwrote app diagnostics: updates=%d code=%d", store.updates, code)
				}
			} else if store.updates != 1 || code != 137 {
				t.Fatalf("ordinary task exit was not recorded: updates=%d code=%d", store.updates, code)
			}
		})
	}
}

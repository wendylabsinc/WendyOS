package containerd

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/api/types"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type streamAwareDataProvider struct {
	recordingDataProvider
	app, service string
	streams      map[string]appconfig.RecordingStream
}

func (p *streamAwareDataProvider) EnsureStreams(app, service string, streams map[string]appconfig.RecordingStream) (string, error) {
	p.app = app
	p.service = service
	p.streams = streams
	return "/streams", nil
}
func TestEnsureDataSocketsCarriesServiceDeclarations(t *testing.T) {
	e := []appconfig.Entitlement{{Type: appconfig.EntitlementEpisodeWrite, Streams: map[string]appconfig.RecordingStream{"samples": {Mode: "durable", MediaType: "text/csv"}}}}
	p := new(streamAwareDataProvider)
	if _, err := ensureDataSockets(p, "test.app", "worker", e); err != nil {
		t.Fatal(err)
	}
	if p.app != "test.app" || p.service != "worker" || p.streams["samples"].Mode != "durable" {
		t.Fatal("stream configuration lost")
	}
	if _, err := ensureDataSockets(new(recordingDataProvider), "test.app", "worker", e); err == nil {
		t.Fatal("unsupported stream grant silently downgraded")
	}
}

// listingDataProvider records how the client lists recording streams in the
// live catalog.
type listingDataProvider struct {
	recordingDataProvider
	mu    sync.Mutex
	calls []string
}

func (p *listingDataProvider) AdvertiseStreams(app, service string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "list "+app+"/"+service)
}
func (p *listingDataProvider) UnadvertiseStreams(app, service string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "unlist "+app+"/"+service)
}

// waitFor waits until the calls so far are want.
func (p *listingDataProvider) waitFor(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		got := append([]string(nil), p.calls...)
		p.mu.Unlock()
		if reflect.DeepEqual(got, want) || (len(got) == 0 && len(want) == 0) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("listing calls %q, want %q", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// exitLabelStore serves containers and takes the exit labels written when a
// task exits.
type exitLabelStore struct{ ros2TestContainerStore }

func (exitLabelStore) Update(_ context.Context, record containers.Container, _ ...string) (containers.Container, error) {
	return record, nil
}

// metricslessTasks reports no metrics, so an exit is not taken for an OOM kill.
type metricslessTasks struct{ *interruptedRecoveryTasks }

func (metricslessTasks) Metrics(context.Context, *tasksapi.MetricsRequest, ...grpc.CallOption) (*tasksapi.MetricsResponse, error) {
	return nil, errors.New("no metrics")
}

// A restarted agent lists the recording streams of a task it reattaches to,
// and unlists them when the task exits; a stopped container's are not listed.
func TestRecoveredTaskListsDataStreamsWhileItRuns(t *testing.T) {
	for _, status := range []tasktypes.Status{tasktypes.Status_RUNNING, tasktypes.Status_STOPPED} {
		t.Run(status.String(), func(t *testing.T) {
			store := exitLabelStore{ros2TestContainerStore{records: []containers.Container{{ID: "my_app_worker", Labels: wendyLabels("my_app", "worker", "v1", nil, nil, "host", nil)}}}}
			tasks := &interruptedRecoveryTasks{status: status, exited: make(chan struct{})}
			remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(store), containerdclient.WithTaskClient(metricslessTasks{tasks})))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			provider := &listingDataProvider{}
			c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop(), dataSocketProvider: provider}
			defer c.closeRecoveredTaskOutput()
			if err := c.RecoverRunningTaskOutput(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if status != tasktypes.Status_RUNNING {
				provider.waitFor(t)
				return
			}
			provider.waitFor(t, "list my_app/worker")
			if _, err := tasks.Kill(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			provider.waitFor(t, "list my_app/worker", "unlist my_app/worker")
		})
	}
}

// exitedTaskForTest is a task that exited, with no output left and no metrics.
type exitedTaskForTest struct{ containerdclient.Task }

func (exitedTaskForTest) IO() cio.IO { return nil }
func (exitedTaskForTest) Metrics(context.Context) (*types.Metric, error) {
	return nil, errors.New("no metrics")
}

// A started task's exit unlists its recording streams, read from the
// container's labels.
func TestTaskExitUnlistsDataStreams(t *testing.T) {
	store := exitLabelStore{ros2TestContainerStore{records: []containers.Container{{ID: "my_app_worker", Labels: wendyLabels("my_app", "worker", "v1", nil, nil, "host", nil)}}}}
	remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(store)))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	provider := &listingDataProvider{}
	c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop(), dataSocketProvider: provider}
	owner := c.trackTaskOutput("my_app_worker", 100)
	exit := make(chan containerdclient.ExitStatus, 1)
	exit <- *containerdclient.NewExitStatus(1, time.Now(), nil)
	output := make(chan services.ContainerOutput, 64)
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go c.streamOutput(context.Background(), exitedTaskForTest{}, exit, output, "my_app_worker", "", 0, owner, stdoutR, stderrR, stdoutW, stderrW)
	for o := range output {
		if o.Done {
			break
		}
	}
	provider.waitFor(t, "unlist my_app/worker")
}

// A start attempt supersedes the previous run, whose exit can no longer
// unlist its streams, so the attempt does; one that fails before its task
// starts does not list them again.
func TestFailedStartUnlistsDataStreams(t *testing.T) {
	labels := wendyLabels("my_app", "worker", "v1", nil, nil, "shared-network", nil)
	labels[labelKeySerialIdentities] = "invalid-json" // Fail before creating a task.
	spec, err := typeurl.MarshalAny(&specs.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	store := aliasOutputContainerStore{ros2TestContainerStore{records: []containers.Container{{ID: "my_app_worker", Labels: labels, Spec: spec}}}}
	remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(store)))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	provider := &listingDataProvider{}
	c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop(), dataSocketProvider: provider}
	if _, err = c.StartContainer(context.Background(), "my_app_worker", "", nil); err == nil || !strings.Contains(err.Error(), "serial device identities") {
		t.Fatalf("start error=%v", err)
	}
	provider.waitFor(t, "unlist my_app/worker")
}

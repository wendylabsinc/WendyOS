//go:build darwin || linux

package containerd

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

// This fixture uses the production containerd client and protobuf/gRPC stack.
// Only the daemon's metadata and task stores are deterministic substitutes.
type listContainersDaemon struct {
	containersapi.UnimplementedContainersServer
	mu          sync.Mutex
	records     []*containersapi.Container
	listError   error
	lists, gets atomic.Int64
}

func (s *listContainersDaemon) ListStream(_ *containersapi.ListContainersRequest, stream containersapi.Containers_ListStreamServer) error {
	s.lists.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listError != nil {
		return s.listError
	}
	for _, record := range s.records {
		if err := stream.Send(&containersapi.ListContainerMessage{Container: record}); err != nil {
			return err
		}
	}
	return nil
}

func (s *listContainersDaemon) Get(_ context.Context, req *containersapi.GetContainerRequest) (*containersapi.GetContainerResponse, error) {
	s.gets.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.records {
		if record.ID == req.ID {
			return &containersapi.GetContainerResponse{Container: proto.Clone(record).(*containersapi.Container)}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "container removed")
}

type listContainersTasks struct {
	tasksapi.UnimplementedTasksServer
	mu    sync.Mutex
	state tasktypes.Status
	fail  bool
	gets  atomic.Int64
}

func (s *listContainersTasks) Get(_ context.Context, req *tasksapi.GetRequest) (*tasksapi.GetResponse, error) {
	s.gets.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, status.Error(codes.NotFound, "task removed")
	}
	return &tasksapi.GetResponse{Process: &tasktypes.Process{ID: req.ContainerID, ContainerID: req.ContainerID, Pid: 1, Status: s.state}}, nil
}

type listContainersTraffic struct{ payloadBytes atomic.Int64 }

func (*listContainersTraffic) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (*listContainersTraffic) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (*listContainersTraffic) HandleConn(context.Context, stats.ConnStats) {}
func (s *listContainersTraffic) HandleRPC(_ context.Context, event stats.RPCStats) {
	switch event := event.(type) {
	case *stats.InPayload:
		s.payloadBytes.Add(int64(event.Length))
	case *stats.OutPayload:
		s.payloadBytes.Add(int64(event.Length))
	}
}

func newListContainersFixture(t testing.TB, count int) (*Client, *listContainersDaemon, *listContainersTasks, *listContainersTraffic) {
	t.Helper()
	daemon := &listContainersDaemon{}
	for i := range count {
		name := fmt.Sprintf("app-%d", i)
		daemon.records = append(daemon.records, &containersapi.Container{ID: name, Image: "docker.io/library/worker:latest", Labels: map[string]string{
			labelKeyAppID: name, labelKeyAppVersion: "1", labelKeyMCPPort: "8081", labelKeyHTTPPort: "8080", labelKeyRestartPolicy: "unless-stopped",
		}})
	}
	tasks := &listContainersTasks{state: tasktypes.Status_RUNNING}
	traffic := &listContainersTraffic{}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.StatsHandler(traffic))
	containersapi.RegisterContainersServer(server, daemon)
	tasksapi.RegisterTasksServer(server, tasks)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient("passthrough:///list-containers",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := containerd.NewWithConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	return &Client{client: remote, namespace: "inventory-test", logger: zap.NewNop()}, daemon, tasks, traffic
}

func TestListContainersUsesFreshEnumerationAndLiveTaskState(t *testing.T) {
	c, daemon, tasks, _ := newListContainersFixture(t, 1)
	first, err := c.ListContainers(context.Background())
	if err != nil || len(first) != 1 || first[0].AppVersion != "1" || first[0].RunningState != agentpb.AppRunningState_RUNNING {
		t.Fatalf("first list: %v %v", first, err)
	}
	daemon.mu.Lock()
	daemon.records[0].Labels[labelKeyAppVersion] = "2"
	daemon.mu.Unlock()
	tasks.mu.Lock()
	tasks.state = tasktypes.Status_STOPPED
	tasks.mu.Unlock()
	second, err := c.ListContainers(context.Background())
	if err != nil || len(second) != 1 || second[0].AppVersion != "2" || second[0].RunningState != agentpb.AppRunningState_STOPPED {
		t.Fatalf("second list: %v %v", second, err)
	}
	if first[0].AppVersion != "1" {
		t.Fatal("previous result mutated")
	}
	if daemon.gets.Load() != 0 || daemon.lists.Load() != 2 {
		t.Fatalf("metadata calls: list=%d get=%d", daemon.lists.Load(), daemon.gets.Load())
	}
	if tasks.gets.Load() != 4 {
		t.Fatalf("live task calls: %d", tasks.gets.Load())
	}
	tasks.mu.Lock()
	tasks.fail = true
	tasks.mu.Unlock()
	third, err := c.ListContainers(context.Background())
	if err != nil || len(third) != 1 || third[0].RunningState != agentpb.AppRunningState_STOPPED {
		t.Fatalf("missing task: %v %v", third, err)
	}
	daemon.mu.Lock()
	daemon.records = nil
	daemon.mu.Unlock()
	if rows, err := c.ListContainers(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("deleted containers: %v %v", rows, err)
	}
	daemon.mu.Lock()
	daemon.listError = status.Error(codes.Unavailable, "daemon unavailable")
	daemon.mu.Unlock()
	if _, err := c.ListContainers(context.Background()); err == nil {
		t.Fatal("list error was hidden")
	}
}

func BenchmarkListContainersGRPC(b *testing.B) {
	for _, count := range []int{1, 8, 32} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			c, daemon, tasks, traffic := newListContainersFixture(b, count)
			ctx := context.Background()
			if _, err := c.ListContainers(ctx); err != nil {
				b.Fatal(err)
			}
			daemon.lists.Store(0)
			daemon.gets.Store(0)
			tasks.gets.Store(0)
			traffic.payloadBytes.Store(0)
			var before, after syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := c.ListContainers(ctx)
				if err != nil || len(rows) != count {
					b.Fatalf("list: %d %v", len(rows), err)
				}
			}
			b.StopTimer()
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
				b.Fatal(err)
			}
			cpu := after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano()
			b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			b.ReportMetric(float64(daemon.gets.Load())/float64(b.N), "metadata-gets/op")
			b.ReportMetric(float64(daemon.lists.Load()+daemon.gets.Load()+tasks.gets.Load())/float64(b.N), "rpc/op")
			b.ReportMetric(float64(traffic.payloadBytes.Load())/float64(b.N), "protobuf-B/op")
		})
	}
}

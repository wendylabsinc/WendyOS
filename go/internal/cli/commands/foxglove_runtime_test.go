//go:build regression_foxglove

package commands

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	containerdapi "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// No graph or container RPC is mocked. The target-selection seam chooses the
// isolated Agent UDS; deployment still runs foxgloveServe -> runWithAgent ->
// Docker build/OCI upload -> ContainerService -> containerd/runc. The bridge
// comes from the published ROS package installed by the generated Dockerfile.
func TestFoxgloveGeneratedAppDeploysAndCarriesROS(t *testing.T) {
	if os.Getenv("WENDY_FOXGLOVE_RUNTIME") != "1" {
		t.Skip("requires the disposable hosted Foxglove runtime fixture")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("Foxglove runtime requires the disposable hosted runner")
	}
	for _, kind := range []string{"mnt", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + kind)
		parent := os.Getenv("WENDY_FOXGLOVE_PARENT_" + kind)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("Foxglove %s namespace is not private", kind)
		}
	}
	work, output := os.Getenv("WENDY_FOXGLOVE_WORK"), os.Getenv("WENDY_FOXGLOVE_OUTPUT")
	if work == "" || output == "" {
		t.Fatal("missing owned work/evidence directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	store, err := containerd.NewClient(zap.NewNop(), filepath.Join(work, "containerd.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	listener, err := net.Listen("unix", filepath.Join(work, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(64<<20), grpc.MaxSendMsgSize(64<<20), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		err := handler(srv, stream)
		if err != nil && info.FullMethod == agentpb.WendyContainerService_ExecContainer_FullMethodName {
			t.Logf("FOXGLOVE_EXEC_ERROR %v", err)
		}
		return err
	}))
	agentpb.RegisterWendyAgentServiceServer(server, services.NewAgentService(zap.NewNop(), nil, nil, nil, nil))
	agentpb.RegisterWendyContainerServiceServer(server, services.NewContainerService(zap.NewNop(), store))
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("Foxglove Agent RPC server did not drain")
		}
	})
	oldResolve, oldDeploy := resolveFoxgloveTarget, deployFoxgloveApp
	t.Cleanup(func() { resolveFoxgloveTarget, deployFoxgloveApp = oldResolve, oldDeploy })
	resolveFoxgloveTarget = func(ctx context.Context, selector string) (*SelectedDevice, error) {
		if selector != "fixture-owned-agent" {
			return nil, fmt.Errorf("unexpected target %q", selector)
		}
		conn, err := grpcclient.ConnectUnix(ctx, listener.Addr().String())
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn}, nil
	}
	deployFoxgloveApp = func(ctx context.Context, conn *grpcclient.AgentConnection, dir string, cfg *appconfig.AppConfig, builder string) error {
		// Record the actual production inputs before their temporary directory is
		// removed. Do not replace the generated recipe or the deployment function.
		for _, name := range []string{"Dockerfile", "wendy.json"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(output, name), data, 0644); err != nil {
				return err
			}
			t.Logf("FOXGLOVE_BUILD_INPUT %s sha256=%x", name, sha256.Sum256(data))
		}
		return oldDeploy(ctx, conn, dir, cfg, builder)
	}
	// The isolated daemon starts empty, so these fixed production app names
	// cannot refer to another user's container. The namespace wrapper also
	// drains all remaining tasks before it removes owned backing files.
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		for _, name := range []string{foxgloveAppID, multiServiceContainerName(foxgloveAppID, "foxglove")} {
			_ = store.StopContainer(cleanup, name)
			_ = store.DeleteContainer(cleanup, name, true)
		}
	})
	bridgeCtx, stopBridge := context.WithCancel(ctx)
	bridgeDone := make(chan error, 1)
	port := foxgloveTestPort(t)
	go func() {
		bridgeDone <- foxgloveServe(bridgeCtx, foxgloveServeOpts{device: "fixture-owned-agent", localPort: port, domain: 71, distro: "humble", rmw: "rmw_cyclonedds_cpp", builder: "docker"})
	}()
	bridgeJoined := false
	t.Cleanup(func() {
		stopBridge()
		if !bridgeJoined {
			select {
			case <-bridgeDone:
			case <-time.After(10 * time.Second):
				t.Error("Foxglove deployment/forwarding did not drain")
			}
		}
	})
	// Poll actual container state while image building/uploading proceeds. No
	// source, port or handshake success is inferred from a detached acknowledgement.
	for {
		apps, err := store.ListContainers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		running := false
		for _, app := range apps {
			if app.AppName == foxgloveAppID && app.RunningState == agentpb.AppRunningState_RUNNING {
				running = true
			}
		}
		if running {
			break
		}
		select {
		case err := <-bridgeDone:
			bridgeJoined = true
			t.Fatalf("Foxglove real deployment/readiness failed: %v", err)
		case <-ctx.Done():
			t.Fatal("Foxglove real deployment did not produce a running app")
		case <-time.After(100 * time.Millisecond):
		}
	}
	inspect, err := containerdapi.New(filepath.Join(work, "containerd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspect.Close() })
	ids, err := store.ContainerIDsForApp(ctx, foxgloveAppID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("deployed bridge container identity: %v %v", ids, err)
	}
	imageCtx := namespaces.WithNamespace(ctx, "default")
	deployed, err := inspect.LoadContainer(imageCtx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	builtImage, err := deployed.Image(imageCtx)
	if err != nil {
		t.Fatal(err)
	}
	imageDigest := builtImage.Target().Digest.String()
	if !strings.HasPrefix(imageDigest, "sha256:") {
		t.Fatalf("missing actual image digest: %s", imageDigest)
	}
	if err := os.WriteFile(filepath.Join(output, "image-digest.txt"), []byte(imageDigest+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("FOXGLOVE_DEPLOYED_IMAGE container=%s digest=%s", ids[0], imageDigest)
	// Observe the actually installed package version, not the package requested
	// in a Dockerfile or a mutable registry tag.
	var packageInfo bytes.Buffer
	code, err := store.ExecInContainer(ctx, foxgloveAppID, []string{"dpkg-query", "-W", "-f=${Package} ${Version}\\n", "ros-humble-foxglove-bridge", "ros-humble-rmw-cyclonedds-cpp", "socat"}, false, nil, &packageInfo, &packageInfo, nil)
	if err != nil || code != 0 || !strings.Contains(packageInfo.String(), "ros-humble-foxglove-bridge ") {
		t.Fatalf("Foxglove package provenance failed: code=%d err=%v output=%s", code, err, packageInfo.String())
	}
	if err := os.WriteFile(filepath.Join(output, "installed-packages.txt"), packageInfo.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	topic := "/wendy_2306_" + nonce
	publisher := fmt.Sprintf(`import rclpy,time
from std_msgs.msg import String
rclpy.init()
n=rclpy.create_node('wendy_2306_%s')
p=n.create_publisher(String, %q, 10)
deadline=time.monotonic()+90
i=0
while rclpy.ok() and time.monotonic()<deadline:
 m=String();m.data=%q+':'+str(i);p.publish(m);i+=1
 rclpy.spin_once(n,timeout_sec=0.1)
n.destroy_node();rclpy.shutdown()
`, nonce, topic, nonce)
	publisherCtx, stopPublisher := context.WithCancel(ctx)
	publisherDone := make(chan error, 1)
	go func() {
		code, err := store.ExecInContainer(publisherCtx, foxgloveAppID, []string{"bash", "-c", "source /opt/ros/humble/setup.bash && exec python3 -c \"$1\"", "foxglove-fixture", publisher}, false, nil, io.Discard, io.Discard, nil)
		publisherDone <- fmt.Errorf("publisher exit=%d error=%v", code, err)
	}()
	t.Cleanup(func() {
		stopPublisher()
		select {
		case <-publisherDone:
		case <-time.After(5 * time.Second):
			t.Error("owned ROS publisher did not drain")
		}
	})
	observation, stopObservation := context.WithTimeout(ctx, 40*time.Second)
	defer stopObservation()
	var ws *websocket.Conn
	for {
		probe, stop := context.WithTimeout(observation, time.Second)
		ws, _, err = websocket.Dial(probe, fmt.Sprintf("ws://127.0.0.1:%d", port), &websocket.DialOptions{Subprotocols: []string{"foxglove.sdk.v1", "foxglove.websocket.v1"}})
		stop()
		if err == nil {
			break
		}
		select {
		case err := <-bridgeDone:
			bridgeJoined = true
			t.Fatalf("Foxglove real deployment/readiness failed: %v", err)
		case <-observation.Done():
			t.Fatalf("Foxglove forwarded WebSocket unavailable: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer ws.CloseNow()
	ws.SetReadLimit(2 << 20)
	if ws.Subprotocol() != "foxglove.sdk.v1" && ws.Subprotocol() != "foxglove.websocket.v1" {
		t.Fatalf("unaccepted real bridge subprotocol %q", ws.Subprotocol())
	}
	var subscription bool
	lastCounter := -1
	seen := 0
	for seen < 5 {
		kind, data, err := ws.Read(observation)
		if err != nil {
			t.Fatalf("Foxglove forwarded ROS delivery failed: received=%d error=%v", seen, err)
		}
		if kind == websocket.MessageText {
			var message struct {
				Op       string `json:"op"`
				Channels []struct {
					ID         uint32 `json:"id"`
					Topic      string `json:"topic"`
					Encoding   string `json:"encoding"`
					SchemaName string `json:"schemaName"`
				} `json:"channels"`
			}
			if err := json.Unmarshal(data, &message); err != nil {
				t.Fatal(err)
			}
			if message.Op == "advertise" && !subscription {
				for _, channel := range message.Channels {
					if channel.Topic != topic {
						continue
					}
					if channel.Encoding != "cdr" || channel.SchemaName != "std_msgs/msg/String" {
						t.Fatalf("real ROS topic lost its CDR schema: %+v", channel)
					}
					request := fmt.Sprintf(`{"op":"subscribe","subscriptions":[{"id":2306,"channelId":%d}]}`, channel.ID)
					if err := ws.Write(observation, websocket.MessageText, []byte(request)); err != nil {
						t.Fatal(err)
					}
					subscription = true
				}
			}
			continue
		}
		if kind == websocket.MessageBinary && len(data) == 9 && data[0] == 2 {
			continue // Optional server clock message, never ROS delivery evidence.
		}
		if !subscription {
			t.Fatal("ROS binary delivery arrived before subscription")
		}
		counter, err := decodeFoxgloveRuntimeString(data, nonce, lastCounter)
		if err != nil {
			t.Fatal(err)
		}
		lastCounter = counter
		seen++
	}
	t.Logf("FOXGLOVE_ROS_BYTES_VERIFIED topic=%s fresh_messages=%d last_counter=%d", topic, seen, lastCounter)
	_ = ws.CloseNow()
	stopBridge()
	select {
	case err := <-bridgeDone:
		bridgeJoined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Foxglove forwarding ignored cancellation")
	}
	apps, err := store.ListContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].AppName != foxgloveAppID || len(apps[0].Services) != 0 || apps[0].RunningState != agentpb.AppRunningState_RUNNING {
		t.Fatalf("forwarding cancellation changed deployed bridge or left legacy services: %v", apps)
	}
	t.Logf("FOXGLOVE_RUNTIME_VERIFIED protocol=%s topic=%s fresh_messages=%d last_counter=%d; actual generated build/deploy and ExecContainer relay; app survives forward cancellation", ws.Subprotocol(), topic, seen, lastCounter)
}

// This optional preparation is ordinary-user, generation-only. It lets an
// operator smoke-test the exact recipe with Docker's existing ordinary driver
// without running the hosted namespace fixture or its OCI-export builder.
func TestFoxgloveExportGeneratedBuild(t *testing.T) {
	out := os.Getenv("WENDY_FOXGLOVE_EXPORT")
	if out == "" {
		t.Skip("explicit fresh output directory required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeFoxgloveApp(out, foxgloveServeOpts{domain: 71, distro: "humble", rmw: "rmw_cyclonedds_cpp"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Dockerfile", "wendy.json"} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("FOXGLOVE_EXPORTED_INPUT %s sha256=%x", name, sha256.Sum256(data))
	}
}

// Independent decoding of the published v1 MessageData envelope and ROS2 CDR
// String, following github.com/foxglove/ws-protocol/docs/spec.md#message-data.
func decodeFoxgloveRuntimeString(data []byte, nonce string, previous int) (int, error) {
	if len(data) < 22 || data[0] != 1 || binary.LittleEndian.Uint32(data[1:5]) != 2306 || binary.LittleEndian.Uint64(data[5:13]) == 0 {
		return 0, fmt.Errorf("invalid actual WebSocket data envelope")
	}
	cdr := data[13:]
	if !bytes.Equal(cdr[:4], []byte{0, 1, 0, 0}) {
		return 0, fmt.Errorf("unexpected CDR encapsulation")
	}
	size := uint64(binary.LittleEndian.Uint32(cdr[4:8]))
	if size < 1 || uint64(len(cdr)) != 8+size || cdr[len(cdr)-1] != 0 {
		return 0, fmt.Errorf("invalid CDR string bounds")
	}
	value := string(cdr[8 : len(cdr)-1])
	counter, err := strconv.Atoi(strings.TrimPrefix(value, nonce+":"))
	if !strings.HasPrefix(value, nonce+":") || err != nil || counter <= previous {
		return 0, fmt.Errorf("Foxglove delivered stale or foreign ROS bytes: %q previous=%d", value, previous)
	}
	return counter, nil
}

func TestFoxgloveRuntimeMessageAdmission(t *testing.T) {
	nonce := "76b27ea6d1b647059f6e08fae91eab90"
	frame := func(value string) []byte {
		data := make([]byte, 21+len(value)+1)
		data[0] = 1
		binary.LittleEndian.PutUint32(data[1:5], 2306)
		binary.LittleEndian.PutUint64(data[5:13], 10)
		data[14] = 1
		binary.LittleEndian.PutUint32(data[17:21], uint32(len(value)+1))
		copy(data[21:], value)
		return data
	}
	good := frame(nonce + ":8")
	if got, err := decodeFoxgloveRuntimeString(good, nonce, 7); err != nil || got != 8 {
		t.Fatalf("valid serialized ROS string: %d %v", got, err)
	}
	cases := map[string][]byte{"short": good[:20], "stale": frame(nonce + ":7"), "foreign": frame("other:8"), "trailing-token": frame(nonce + ":8garbage"), "no-terminator": append([]byte(nil), good...), "wrong-subscription": append([]byte(nil), good...), "wrong-encoding": append([]byte(nil), good...), "zero-timestamp": append([]byte(nil), good...), "oversized-length": append([]byte(nil), good...)}
	cases["no-terminator"][len(good)-1] = 1
	cases["wrong-subscription"][1] = 0
	cases["wrong-encoding"][14] = 0
	for i := 5; i < 13; i++ {
		cases["zero-timestamp"][i] = 0
	}
	binary.LittleEndian.PutUint32(cases["oversized-length"][17:21], ^uint32(0))
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeFoxgloveRuntimeString(data, nonce, 7); err == nil {
				t.Fatal("accepted invalid/stale ROS wire bytes")
			}
		})
	}
}

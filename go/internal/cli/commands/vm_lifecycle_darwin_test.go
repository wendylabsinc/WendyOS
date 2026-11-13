//go:build darwin

package commands

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// No launch, transport, QMP, port-forward, or Agent API seam is replaced.
// The wrapper supplies a fresh private VM created by the actual CLI, and a
// source-built static ARM64 HTTP application. Hardware HVF acceptance is separate.
func TestVMOwnedLifecycle(t *testing.T) {
	name := os.Getenv("WENDY_VM_LIFECYCLE_NAME")
	if name == "" {
		t.Skip("requires explicitly owned VM lifecycle wrapper")
	}
	root := os.Getenv("WENDY_CONFIG_DIR")
	nonce := os.Getenv("WENDY_VM_LIFECYCLE_NONCE")
	if len(nonce) != 32 || name != "vm-"+nonce[:16] || root == "" || os.Getenv("WENDY_SECRET_STORE") != "file" {
		t.Fatal("invalid private fixture identity")
	}
	if b, e := os.ReadFile(filepath.Join(root, "fixture-owner")); e != nil || string(b) != nonce {
		t.Fatal("private VM ownership receipt mismatch")
	}
	s, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(name)
	if err != nil || !st.Exists || st.Running {
		t.Fatalf("requires owned stopped VM: %+v %v", st, err)
	}
	oldJSON := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = oldJSON }()
	ctx, cancel := context.WithTimeout(context.Background(), 13*time.Minute)
	defer cancel()
	defer func() {
		st, e := s.Status(name)
		if e != nil {
			t.Errorf("VM_DRAIN_UNKNOWN: %v", e)
			return
		}
		if st.Running {
			_ = lifecycleQMP(s.QMPPath(name), "quit")
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				st, e = s.Status(name)
				if e == nil && !st.Running {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if e != nil || st.Running {
				t.Errorf("VM_DRAIN_UNKNOWN: retaining owned VM: %v", e)
				return
			}
		}
		t.Log("VM_OWNED_DRAINED")
	}()
	addr, started, err := ensureSimulatorRunning(ctx, name)
	if err != nil || !started {
		t.Fatalf("actual detached launch: started=%v err=%v", started, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = lifecycleQMP(s.QMPPath(name), "stop")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	first, err := s.Status(name)
	if err != nil || !first.Running {
		t.Fatal("QEMU did not acquire run lock", err)
	}
	wantAccel := vm.AccelTCG
	if runtime.GOARCH == "arm64" {
		wantAccel = vm.AccelHVF
	}
	if first.State.Accel != wantAccel {
		t.Fatalf("production acceleration=%s want=%s", first.State.Accel, wantAccel)
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 700*time.Millisecond)
	probe, _, probeErr := connectSimulatorAgent(probeCtx, name, addr)
	probeCancel()
	if probe != nil {
		probe.Close()
	}
	if probeErr == nil {
		t.Fatal("paused first boot was already ready; cancellation boundary not observed")
	}
	waitCtx, waitCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- startSimulatorFn(waitCtx, name) }()
	// Observe the actual production wait stack before canceling. A scheduler
	// delay must not turn cancellation before the operation starts into proof.
	waitEntered := false
	waitDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(waitDeadline) {
		select {
		case e := <-done:
			waitCancel()
			t.Fatalf("boot waiter returned before cancellation: %v", e)
		default:
		}
		stacks := make([]byte, 1<<20)
		n := runtime.Stack(stacks, true)
		if n == len(stacks) {
			waitCancel()
			t.Fatal("actual wait-stack observation truncated")
		}
		if bytes.Contains(stacks[:n], []byte("commands.waitForSimulatorAgent(")) {
			waitEntered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waitEntered {
		waitCancel()
		t.Fatal("actual simulator boot wait was not observed")
	}
	beforeCancel := time.Now()
	waitCancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("VM_CANCEL_RECOVERY: expected cancellation, got %v", e)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("VM_CANCEL_RECOVERY: boot waiter did not drain")
	}
	after, err := s.Status(name)
	if err != nil || !after.Running || after.State.PID != first.State.PID || !after.State.StartedAt.Equal(first.State.StartedAt) {
		t.Fatal("cancel altered the owned VM lifecycle", err)
	}
	t.Logf("VM_CANCEL_RECOVERY_VERIFIED elapsed=%s pid=%d accel=%s", time.Since(beforeCancel), after.State.PID, wantAccel)
	if err = lifecycleQMP(s.QMPPath(name), "cont"); err != nil {
		t.Fatal(err)
	}
	if err = startSimulatorFn(ctx, name); err != nil {
		t.Fatalf("actual discover-start recovery: %v", err)
	}
	conn, version, err := connectSimulatorAgent(ctx, name, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if version.GetHostname() == "" || version.GetPublicKey() == "" {
		t.Fatal("actual Agent omitted persistent hostname/key identity")
	}
	t.Logf("VM_AGENT_READY: %s", version)
	port := lifecycleFreePort(t)
	cfg := &appconfig.AppConfig{AppID: "dev.wendy.vm-" + nonce[:16], Entitlements: []appconfig.Entitlement{{Type: "network", Mode: "host"}}, Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 15}}
	lifecycleDeploy(t, ctx, conn, cfg, nonce, port)
	if c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); e == nil {
		c.Close()
		t.Fatal("application host port was already exposed before QMP forwarding")
	}
	for range 2 {
		if err = prepareVMAppPorts(ctx, conn, cfg); err != nil {
			t.Fatal(err)
		}
	}
	host, ok := resolveHookHost(ctx, conn, cfg)
	if !ok || host != "127.0.0.1" {
		t.Fatalf("actual VM hook host=%q ok=%v", host, ok)
	}
	if err = waitForReadiness(ctx, effectiveReadiness(cfg), host, 0); err != nil {
		t.Fatalf("VM_APP_FORWARD: actual guest application never became reachable: %v", err)
	}
	firstReply := lifecycleHTTP(t, port, nonce)
	nextReply := lifecycleHTTP(t, port, nonce)
	if nextReply.Counter <= firstReply.Counter || nextReply.BootID != firstReply.BootID {
		t.Fatal("guest HTTP did not produce fresh same-boot bytes")
	}
	t.Logf("VM_APP_FORWARD_VERIFIED boot_id=%s counter=%d port=%d", firstReply.BootID, nextReply.Counter, port)
	conn.Close()
	if err = s.StopContext(ctx, name, false, 60*time.Second); err != nil {
		t.Fatalf("actual QMP powerdown: %v", err)
	}
	stopped, err := s.Status(name)
	if err != nil || stopped.Running {
		t.Fatal("production stop did not drain run lock", err)
	}
	if c, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond); e == nil {
		c.Close()
		t.Fatal("stopped guest still has a reachable app port")
	}
	if err = startSimulatorFn(ctx, name); err != nil {
		t.Fatalf("same disk/alias restart: %v", err)
	}
	restarted, err := s.Status(name)
	if err != nil || !restarted.Running || restarted.State.StartedAt.Equal(first.State.StartedAt) || restarted.State.AgentPort != first.State.AgentPort {
		t.Fatal("restart did not preserve identity/port and replace runstate", err)
	}
	second, secondVersion, err := connectSimulatorAgent(ctx, name, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if secondVersion.GetHostname() != version.GetHostname() || secondVersion.GetPublicKey() != version.GetPublicKey() {
		t.Fatal("same-disk restart changed persistent Agent identity")
	}
	// Start the persisted app through its production Agent API; do not upload or
	// replace the image after restart. An already-running app is handled by Agent.
	startCtx, startCancel := context.WithTimeout(ctx, 60*time.Second)
	restartStream, startErr := second.ContainerService.StartContainer(startCtx, &agentpb.StartContainerRequest{AppName: cfg.AppID, RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}})
	if startErr != nil {
		startCancel()
		t.Fatalf("persisted app start after restart: %v", startErr)
	}
	for {
		message, recvErr := restartStream.Recv()
		if recvErr != nil {
			startCancel()
			t.Fatalf("persisted app start acknowledgement: %v", recvErr)
		}
		if message.GetStarted() != nil {
			break
		}
	}
	startCancel()
	for range 2 {
		if err = prepareVMAppPorts(ctx, second, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if err = waitForReadiness(ctx, effectiveReadiness(cfg), host, 0); err != nil {
		t.Fatal(err)
	}
	recovered := lifecycleHTTP(t, port, nonce)
	if recovered.BootID == firstReply.BootID {
		t.Fatal("restart reused the first guest boot")
	}
	t.Logf("VM_RESTART_FORWARD_VERIFIED old_boot=%s new_boot=%s agent=%s", firstReply.BootID, recovered.BootID, secondVersion)
	if err = s.StopContext(ctx, name, false, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Log("VM_LIFECYCLE_VERIFIED")
}

type lifecycleReply struct {
	Nonce   string `json:"nonce"`
	BootID  string `json:"boot_id"`
	Counter uint64 `json:"counter"`
}

func lifecycleHTTP(t *testing.T, port int, nonce string) lifecycleReply {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result lifecycleReply
	if response.StatusCode != 200 {
		t.Fatalf("guest HTTP status=%d", response.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result); err != nil || result.Nonce != nonce || len(result.BootID) != 36 || result.Counter == 0 {
		t.Fatalf("guest HTTP identity=%+v error=%v", result, err)
	}
	return result
}
func lifecycleFreePort(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}
func lifecycleQMP(path, command string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	decoder := json.NewDecoder(io.LimitReader(conn, 65536))
	encoder := json.NewEncoder(conn)
	var greeting map[string]json.RawMessage
	if err = decoder.Decode(&greeting); err != nil {
		return err
	}
	if greeting["QMP"] == nil {
		return errors.New("missing actual QMP greeting")
	}
	for _, cmd := range []string{"qmp_capabilities", command} {
		if err = encoder.Encode(map[string]string{"execute": cmd}); err != nil {
			return err
		}
		for {
			var response map[string]json.RawMessage
			if err = decoder.Decode(&response); err != nil {
				return err
			}
			if response["event"] != nil {
				continue
			}
			if response["error"] != nil {
				return fmt.Errorf("QMP %s: %s", cmd, response["error"])
			}
			if response["return"] != nil {
				break
			}
			return errors.New("unexpected QMP response")
		}
	}
	return nil
}
func lifecycleDeploy(t *testing.T, ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, nonce string, port int) {
	t.Helper()
	path := os.Getenv("WENDY_VM_LIFECYCLE_APP")
	binary, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := elf.NewFile(bytes.NewReader(binary))
	if err != nil {
		t.Fatal(err)
	}
	defer executable.Close()
	if executable.Machine != elf.EM_AARCH64 {
		t.Fatal("guest application is not actual ARM64 ELF")
	}
	for _, p := range executable.Progs {
		if p.Type == elf.PT_INTERP {
			t.Fatal("guest scratch application has dynamic interpreter")
		}
	}
	var layer bytes.Buffer
	writer := tar.NewWriter(&layer)
	if err = writer.WriteHeader(&tar.Header{Name: "probe", Mode: 0755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(layer.Bytes()))
	image, err := json.Marshal(map[string]any{"architecture": "arm64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{digest}}, "config": map[string]any{"Entrypoint": []string{"/probe"}, "Cmd": []string{strconv.Itoa(port), nonce}}})
	if err != nil {
		t.Fatal(err)
	}
	headers, err := pushLayersByChunksWithPrepare(ctx, conn.ContainerService, []localLayer{{Digest: digest, DiffID: digest, MediaType: "application/vnd.oci.image.layer.v1.tar", Blob: layer.Bytes(), Size: int64(layer.Len())}}, func(c context.Context, h []*agentpb.RunContainerLayerHeader) error {
		_, e := conn.ContainerService.PrepareImage(c, &agentpb.RunContainerLayersRequest{ImageName: cfg.AppID + ":latest", Layers: h, ImageConfig: image})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stream, err := conn.ContainerService.RunContainer(runCtx, &agentpb.RunContainerLayersRequest{AppName: cfg.AppID, ImageName: cfg.AppID + ":latest", Layers: headers, ImageConfig: image, AppConfig: data, RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		message, e := stream.Recv()
		if e != nil {
			t.Fatalf("actual guest container start: %v", e)
		}
		if message.GetStarted() != nil {
			break
		}
	}
	t.Logf("VM_GUEST_APP_STARTED image_config_sha256=%x layer=%s binary_sha256=%x", sha256.Sum256(image), digest, sha256.Sum256(binary))
}

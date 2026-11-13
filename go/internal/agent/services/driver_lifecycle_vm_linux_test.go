//go:build linux

package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func requireDriverVM(t *testing.T) string {
	t.Helper()
	nonce := os.Getenv("WENDY_DRIVER_VM")
	if nonce == "" {
		t.Skip("requires the disposable driver QEMU guest")
	}
	if len(nonce) != 32 {
		t.Fatal("invalid VM ownership nonce")
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/fixture/nonce": nonce, "/sys/class/dmi/id/product_name": "WendyDriverFixture-" + nonce, "/proc/1/comm": "systemd"} {
		got, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(got)) != want {
			t.Fatalf("not the owned driver VM: %s", path)
		}
	}
	if os.Geteuid() != 0 {
		t.Fatal("driver VM must run its own guest root")
	}
	command, err := os.ReadFile("/proc/cmdline")
	if err != nil || !strings.Contains(" "+string(command)+" ", " wendy.driver="+nonce+" ") {
		t.Fatal("missing guest kernel ownership token")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			t.Fatal("driver guest unexpectedly has a network interface")
		}
	}
	return nonce
}

func vmModuleVersion(t *testing.T, module, expected string) {
	t.Helper()
	got, err := os.ReadFile("/sys/module/" + module + "/parameters/version")
	if err != nil || strings.TrimSpace(string(got)) != expected {
		t.Fatalf("live driver version mismatch: %s got %q error %v want %s", module, got, err, expected)
	}
}

func TestDriverLifecycleVM(t *testing.T) {
	nonce := requireDriverVM(t)
	const target = "wendy-fixture-target"
	const healthy = "wendy-fixture-healthy"
	const broken = "wendy-fixture-broken"
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	// Capture a stalled guest's real process/systemd state before the host
	// watchdog drains QEMU. This observer changes no driver or test deadline.
	diagnosticsDone := make(chan struct{})
	go func() {
		defer close(diagnosticsDone)
		timer := time.NewTimer(45 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		fmt.Fprintln(os.Stdout, "DRIVER_VM_STALL_DIAGNOSTICS")
		_ = pprof.Lookup("goroutine").WriteTo(os.Stdout, 2)
		for _, command := range [][]string{
			{"systemctl", "list-jobs", "--no-pager"},
			{"systemctl", "status", "systemd-sysext.service", "--no-pager", "--lines=20"},
			{"ps", "-eo", "pid,ppid,stat,wchan:30,args"},
			{"journalctl", "--no-pager", "--lines=80"},
		} {
			probeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			probe := exec.CommandContext(probeCtx, command[0], command[1:]...)
			probe.WaitDelay = time.Second
			probe.Stdout, probe.Stderr = os.Stdout, os.Stdout
			fmt.Fprintf(os.Stdout, "DRIVER_VM_PROBE %v\n", command)
			err := probe.Run()
			stop()
			fmt.Fprintf(os.Stdout, "DRIVER_VM_PROBE_RESULT %v\n", err)
		}
	}()
	defer func() { cancel(); <-diagnosticsDone }()
	svc := NewDriverService(zap.NewExample()) // No path, command, module, merge or verifier seams.
	socket := "/run/wendy-driver-fixture-" + nonce + ".sock"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	rpc := grpc.NewServer()
	pb.RegisterWendyDriverServiceServer(rpc, svc)
	done := make(chan struct{})
	go func() { defer close(done); _ = rpc.Serve(listener) }()
	defer func() {
		rpc.Stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fixture RPC did not drain")
		}
		_ = os.Remove(socket)
	}()
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewWendyDriverServiceClient(conn)
	list := func() *pb.ListDriversResponse {
		t.Helper()
		response, err := client.ListDrivers(ctx, &pb.ListDriversRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	assertStatus := func(name string, present, loaded bool) {
		t.Helper()
		response := list()
		for _, driver := range response.GetInstalled() {
			if driver.GetName() == name {
				if !present || driver.GetLoaded() != loaded || driver.GetUnreadable() {
					t.Fatalf("incorrect actual driver status: %v", driver)
				}
				return
			}
		}
		if present {
			t.Fatalf("installed driver absent from status: %s", name)
		}
	}
	// This phase runs only after a real QEMU poweroff and a new kernel boot. The
	// guest's boot service has re-run the unchanged production sysext apply script.
	if os.Getenv("WENDY_DRIVER_AFTER_REBOOT") == "1" {
		previous, err := os.ReadFile("/fixture/first-boot-id")
		if err != nil {
			t.Fatal(err)
		}
		current, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil || bytes.Equal(current, previous) {
			t.Fatal("removal persistence did not use a fresh kernel boot")
		}
		assertStatus(target, false, false)
		assertStatus(healthy, true, true)
		vmModuleVersion(t, "wendy_fixture_healthy", "1")
		if _, err := os.Stat("/sys/module/wendy_fixture_target"); !os.IsNotExist(err) {
			t.Fatal("removed driver reappeared after reboot")
		}
		for _, path := range []string{svc.rawPath(unameRelease(), target), filepath.Join(driverPrivatePayloadDir, target), filepath.Join(driverActiveStateDir, target)} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("removed driver artifact survived reboot: %s", path)
			}
		}
		t.Log("DRIVER_VM_REBOOT_VERIFIED real new kernel; removed module absent; healthy driver reloaded")
		return
	}
	if entries, err := os.ReadDir(driverEnabledDir); err == nil && len(entries) != 0 {
		t.Fatal("guest did not start with an empty driver store")
	}
	for _, module := range []string{"wendy_fixture_target", "wendy_fixture_healthy", "wendy_fixture_broken"} {
		if _, err := os.Stat("/sys/module/" + module); !os.IsNotExist(err) {
			t.Fatal("fixture module already resident before install")
		}
	}
	install := func(name, variant string, stage bool) *pb.DriverApplyResponse {
		t.Helper()
		t.Logf("DRIVER_VM_INSTALL_BEGIN name=%s variant=%s stage=%v", name, variant, stage)
		payload, err := os.ReadFile("/fixture/images/" + variant + "/" + name + ".raw")
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		stream, err := client.InstallDriver(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = stream.Send(&pb.InstallDriverRequest{RequestType: &pb.InstallDriverRequest_Spec{Spec: &pb.DriverSpec{Name: name, KernelVersion: unameRelease(), Sha256: hex.EncodeToString(digest[:]), StageOnly: stage}}}); err != nil {
			t.Fatal(err)
		}
		for offset := 0; offset < len(payload); offset += 64 * 1024 {
			end := min(offset+64*1024, len(payload))
			if err = stream.Send(&pb.InstallDriverRequest{RequestType: &pb.InstallDriverRequest_Chunk_{Chunk: &pb.InstallDriverRequest_Chunk{Data: payload[offset:end]}}}); err != nil {
				t.Fatal(err)
			}
		}
		if err = stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		var terminal *pb.DriverApplyResponse
		for {
			response, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.GetCompleted() != nil || response.GetFailed() != nil {
				if terminal != nil {
					t.Fatal("duplicate terminal install response")
				}
				terminal = response
			}
		}
		if terminal == nil {
			t.Fatal("install had no terminal response")
		}
		t.Logf("DRIVER_VM_INSTALL_END name=%s variant=%s response=%v", name, variant, terminal)
		return terminal
	}
	completed := func(response *pb.DriverApplyResponse) {
		t.Helper()
		if response.GetCompleted() == nil {
			t.Fatalf("driver install failed: %v", response)
		}
	}
	completed(install(healthy, "healthy", false))
	vmModuleVersion(t, "wendy_fixture_healthy", "1")
	assertStatus(healthy, true, true)
	first := install(target, "v1", false)
	completed(first)
	if first.GetCompleted().GetRebootRequired() {
		t.Fatal("fresh module load spuriously needs reboot")
	}
	vmModuleVersion(t, "wendy_fixture_target", "1")
	assertStatus(target, true, true)
	generation, err := os.ReadFile(filepath.Join(driverActiveStateDir, target, "generation"))
	if err != nil || len(generation) == 0 {
		t.Fatal("missing actual activation generation")
	}
	second := install(target, "v2", false)
	completed(second)
	vmModuleVersion(t, "wendy_fixture_target", "2") // The omit-unload control must fail here.
	if second.GetCompleted().GetRebootRequired() {
		t.Fatal("live replacement incorrectly needs reboot")
	}
	nextGeneration, err := os.ReadFile(filepath.Join(driverActiveStateDir, target, "generation"))
	if err != nil || bytes.Equal(generation, nextGeneration) {
		t.Fatal("replacement reused stale receipt")
	}
	assertStatus(target, true, true)
	vmModuleVersion(t, "wendy_fixture_healthy", "1")
	before, err := os.ReadFile(svc.rawPath(unameRelease(), target))
	if err != nil {
		t.Fatal(err)
	}
	failed := install(target, "failing", false)
	if failed.GetFailed() == nil {
		t.Fatal("failing real module init unexpectedly succeeded")
	}
	after, err := os.ReadFile(svc.rawPath(unameRelease(), target))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed replacement lost the previous driver image")
	}
	vmModuleVersion(t, "wendy_fixture_target", "2")
	vmModuleVersion(t, "wendy_fixture_healthy", "1")
	assertStatus(target, true, true)
	assertStatus(healthy, true, true)
	// Stage a separate failing-init add-on, then remove the healthy target. The
	// real global refresh fails for that neighbour; removal must still settle.
	completed(install(broken, "broken", true))
	remove, err := client.RemoveDriver(ctx, &pb.RemoveDriverRequest{Name: target})
	if err != nil {
		t.Fatal(err)
	}
	var removed *pb.DriverApplyResponse
	for {
		response, err := remove.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFailed() != nil {
			t.Fatalf("driver removal rolled back due to unrelated add-on: %s", response.GetFailed().GetErrorMessage())
		}
		if response.GetCompleted() != nil {
			removed = response
		}
	}
	if removed == nil || !removed.GetCompleted().GetRebootRequired() {
		t.Fatal("resident removed driver did not honestly require reboot")
	}
	assertStatus(target, false, false)
	assertStatus(healthy, true, true)
	assertStatus(broken, true, false)
	vmModuleVersion(t, "wendy_fixture_target", "2")
	vmModuleVersion(t, "wendy_fixture_healthy", "1")
	for _, path := range []string{svc.rawPath(unameRelease(), target), filepath.Join(driverPrivatePayloadDir, target), filepath.Join(driverActiveStateDir, target), filepath.Join("/usr/lib/modules", unameRelease(), "updates/wendyos", target, "wendy_fixture_target.ko")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("removed driver still exposed: %s", path)
		}
	}
	cleanup, err := client.RemoveDriver(ctx, &pb.RemoveDriverRequest{Name: broken})
	if err != nil {
		t.Fatal(err)
	}
	for {
		response, err := cleanup.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFailed() != nil {
			t.Fatalf("broken fixture cleanup: %v", response)
		}
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile("/fixture/first-boot-id", boot, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("DRIVER_VM_LIFECYCLE_VERIFIED %s real module v1/v2, failing-init rollback, unrelated failure isolation, resident removal", nonce))
}

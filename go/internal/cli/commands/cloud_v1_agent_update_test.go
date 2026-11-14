package commands

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	// An agent release from before the Cloud v1 tunnel was removed, and one after.
	cloudV1OldAgent = "2026.09.28-101500"
	cloudV1NewAgent = "2026.11.13-000000"
)

// cloudV1Agent is a fake device agent that reports a version and counts the
// agent uploads it receives. It refuses each upload, so a command that reaches
// the upload ends there with an error the test can recognise.
type cloudV1Agent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	version string
	os      string
	uploads atomic.Int32
}

const cloudV1UploadReached = "test agent received the upload"

func (a *cloudV1Agent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	osName := a.os
	if osName == "" {
		osName = "linux"
	}
	return &agentpb.GetAgentVersionResponse{Version: a.version, Os: osName, CpuArchitecture: "arm64"}, nil
}

func (a *cloudV1Agent) UpdateAgent(grpc.BidiStreamingServer[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse]) error {
	a.uploads.Add(1)
	return status.Error(codes.Internal, cloudV1UploadReached)
}

// cloudV1Enrollment is what the fake agent answers about its enrollment.
type cloudV1Enrollment int

const (
	// The agent predates the v2 enrollment RPC and answers the v1 one: enrolled.
	enrolledCloudV1 cloudV1Enrollment = iota
	// The agent answers the v2 RPC with a Cloud v1 enrollment (no principal).
	enrolledCloudV1ReportedByV2
	enrolledCloudV2
	notEnrolled
	enrollmentProbeFails
	// The agent serves no enrollment RPC at all.
	enrollmentUnsupported
)

type cloudV1ProvisioningV1 struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
	probes *atomic.Int32
}

func (s cloudV1ProvisioningV1) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	s.probes.Add(1)
	return &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_Provisioned{
		Provisioned: &agentpb.ProvisionedResponse{CloudHost: "cloud.wendy.sh:443", OrganizationId: 42, AssetId: 7},
	}}, nil
}

type cloudV1ProvisioningV2 struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	probes   *atomic.Int32
	response *agentpbv2.IsProvisionedResponse
	err      error
}

func (s cloudV1ProvisioningV2) IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	s.probes.Add(1)
	return s.response, s.err
}

// registerCloudV1Enrollment serves the enrollment answer and returns the
// number of times the device was asked for it.
func registerCloudV1Enrollment(enrollment cloudV1Enrollment) (func(*grpc.Server), *atomic.Int32) {
	probes := new(atomic.Int32)
	provisioned := func(principal string) *agentpbv2.IsProvisionedResponse {
		return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{
			Provisioned: &agentpbv2.ProvisionedResponse{CloudHost: "cloud.wendy.sh:443", OrganizationId: 42, AssetId: 7, PrincipalUri: principal},
		}}
	}
	return func(srv *grpc.Server) {
		switch enrollment {
		case enrolledCloudV1:
			agentpb.RegisterWendyProvisioningServiceServer(srv, cloudV1ProvisioningV1{probes: probes})
		case enrolledCloudV1ReportedByV2:
			agentpbv2.RegisterWendyProvisioningServiceServer(srv, cloudV1ProvisioningV2{probes: probes, response: provisioned("")})
		case enrolledCloudV2:
			agentpbv2.RegisterWendyProvisioningServiceServer(srv, cloudV1ProvisioningV2{probes: probes,
				response: provisioned("spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/fad6089a-06e8-4c7b-8c09-4bcbef67f387")})
		case notEnrolled:
			agentpbv2.RegisterWendyProvisioningServiceServer(srv, cloudV1ProvisioningV2{probes: probes,
				response: &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
					NotProvisioned: &agentpbv2.NotProvisionedResponse{},
				}}})
		case enrollmentProbeFails:
			agentpbv2.RegisterWendyProvisioningServiceServer(srv, cloudV1ProvisioningV2{probes: probes, err: status.Error(codes.Unavailable, "offline")})
		case enrollmentUnsupported:
		}
	}, probes
}

// cloudV1Conn connects straight to a fake agent, as a LAN connection would.
func cloudV1Conn(t *testing.T, agent *cloudV1Agent, enrollment cloudV1Enrollment) (*grpcclient.AgentConnection, *atomic.Int32) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, agent)
	register, probes := registerCloudV1Enrollment(enrollment)
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	client, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	conn := grpcclient.NewFromConn(client)
	conn.Host = "thor"
	return conn, probes
}

// throughCloudV1 makes conn look like a Cloud v1 tunnel connection.
func throughCloudV1(conn *grpcclient.AgentConnection) *grpcclient.AgentConnection {
	conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) { return conn, nil }
	return conn
}

// serveCloudV1Device makes every command in the test reach a fake agent.
func serveCloudV1Device(t *testing.T, agent *cloudV1Agent, enrollment cloudV1Enrollment) *atomic.Int32 {
	t.Helper()
	seedConfig(t, &config.Config{})
	previousDevice := deviceFlag
	deviceFlag = ""
	t.Cleanup(func() { deviceFlag = previousDevice })
	register, probes := registerCloudV1Enrollment(enrollment)
	startUDSAgentServing(t, agent, register)
	return probes
}

// cloudV1Terminal sets whether a person is at the terminal, and how they
// answer the default-No prompt. It returns the questions they were asked.
func cloudV1Terminal(t *testing.T, interactive, answer bool) *[]string {
	t.Helper()
	previousInteractive, previousConfirm, previousJSON := isInteractiveTerminalFn, confirmDefaultNoFn, jsonOutput
	t.Cleanup(func() {
		isInteractiveTerminalFn, confirmDefaultNoFn, jsonOutput = previousInteractive, previousConfirm, previousJSON
	})
	jsonOutput = false
	isInteractiveTerminalFn = func() bool { return interactive }
	asked := new([]string)
	confirmDefaultNoFn = func(question string) bool {
		*asked = append(*asked, question)
		return answer
	}
	return asked
}

func cloudV1AgentBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wendy-agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func versionResponse(v string) *agentpb.GetAgentVersionResponse {
	return &agentpb.GetAgentVersionResponse{Version: v, Os: "linux", CpuArchitecture: "arm64"}
}

func TestAgentVersionHasCloudV1Tunnel(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"2026.09.28-101500", true},
		{"v2026.09.28-101500", true},
		{"2026.10.22-080350", true},
		{"2026.10.22-080351", false}, // the first release without it
		{"2026.10.22-091205", false},
		{"2026.11.13-000000", false},
		{"0.9.3", true}, // releases from before date versions
		// A branch build carries its build date: one made before the tunnel
		// was removed still has it.
		{"2026.06.30-133859-dev", true},
		{"2026.11.01-133859-dev", false},
		// A local dev build is the latest version everywhere in the CLI.
		{"dev", false},
		// No version reported: cannot be shown to be new.
		{"", true},
	} {
		if got := agentVersionHasCloudV1Tunnel(tc.version); got != tc.want {
			t.Errorf("agentVersionHasCloudV1Tunnel(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestCloudV1DisconnectRisk(t *testing.T) {
	for _, tc := range []struct {
		name         string
		enrollment   cloudV1Enrollment
		current      string
		os           string
		next         string
		throughCloud bool
		wantRisk     bool
	}{
		{name: "Cloud v1 device with an agent that still has the tunnel", enrollment: enrolledCloudV1, current: cloudV1OldAgent, next: cloudV1NewAgent, wantRisk: true},
		{name: "Cloud v1 enrollment reported by the v2 RPC", enrollment: enrolledCloudV1ReportedByV2, current: cloudV1OldAgent, next: cloudV1NewAgent, wantRisk: true},
		{name: "a local binary of unknown version is treated as current", enrollment: enrolledCloudV1, current: cloudV1OldAgent, next: "", wantRisk: true},
		{name: "a dev build being installed has no tunnel", enrollment: enrolledCloudV1, current: cloudV1OldAgent, next: "dev", wantRisk: true},
		{name: "old branch build on the device", enrollment: enrolledCloudV1, current: "2026.06.30-133859-dev", next: cloudV1NewAgent, wantRisk: true},
		{name: "agent that reports no version", enrollment: enrolledCloudV1, current: "", next: cloudV1NewAgent, wantRisk: true},
		{name: "dev build reached through Cloud v1 proves it has the tunnel", enrollment: enrolledCloudV1, current: "dev", next: cloudV1NewAgent, throughCloud: true, wantRisk: true},

		{name: "Cloud v1 device already without the tunnel", enrollment: enrolledCloudV1ReportedByV2, current: "2026.10.25-120000", next: cloudV1NewAgent},
		{name: "dev build on the device over the LAN", enrollment: enrolledCloudV1, current: "dev", next: cloudV1NewAgent},
		{name: "installing a release that still has the tunnel", enrollment: enrolledCloudV1, current: cloudV1OldAgent, next: "2026.10.01-120000"},
		{name: "Cloud v2 device", enrollment: enrolledCloudV2, current: cloudV1OldAgent, next: cloudV1NewAgent},
		{name: "device that is not enrolled", enrollment: notEnrolled, current: cloudV1OldAgent, next: cloudV1NewAgent},
		{name: "enrollment probe fails", enrollment: enrollmentProbeFails, current: cloudV1OldAgent, next: cloudV1NewAgent},
		{name: "agent without an enrollment RPC", enrollment: enrollmentUnsupported, current: cloudV1OldAgent, next: cloudV1NewAgent},
		{name: "Mac agent keeps its own Cloud v1 tunnel", enrollment: enrolledCloudV1, current: cloudV1OldAgent, os: "darwin", next: cloudV1NewAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := cloudV1Conn(t, &cloudV1Agent{}, tc.enrollment)
			if tc.throughCloud {
				throughCloudV1(conn)
			}
			current := versionResponse(tc.current)
			if tc.os != "" {
				current.Os = tc.os
			}
			risk := cloudV1DisconnectRisk(context.Background(), conn, current, tc.next)
			if (risk != nil) != tc.wantRisk {
				t.Fatalf("risk = %+v, want risk %v", risk, tc.wantRisk)
			}
			if risk != nil && (risk.device != "thor" || risk.current != tc.current || risk.next != tc.next || risk.throughCloud != tc.throughCloud) {
				t.Fatalf("risk = %+v", risk)
			}
		})
	}
}

// The enrollment question costs a round trip to the device. It is asked only
// when the two versions leave something to lose.
func TestCloudV1DisconnectRiskAsksTheDeviceOnlyWhenVersionsLeaveARisk(t *testing.T) {
	for _, tc := range []struct {
		name          string
		current, next string
		wantProbes    int32
	}{
		{"agent already without the tunnel", cloudV1NewAgent, cloudV1NewAgent, 0},
		{"dev agent over the LAN", "dev", cloudV1NewAgent, 0},
		{"old agent", cloudV1OldAgent, cloudV1NewAgent, 1},
	} {
		conn, probes := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1ReportedByV2)
		cloudV1DisconnectRisk(context.Background(), conn, versionResponse(tc.current), tc.next)
		if got := probes.Load(); got != tc.wantProbes {
			t.Errorf("%s: asked the device for its enrollment %d times, want %d", tc.name, got, tc.wantProbes)
		}
	}
	// No connection, or no version answer, must not panic or block.
	if risk := cloudV1DisconnectRisk(context.Background(), nil, versionResponse(cloudV1OldAgent), ""); risk != nil {
		t.Fatalf("risk without a connection = %+v", risk)
	}
	conn, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1)
	if risk := cloudV1DisconnectRisk(context.Background(), conn, nil, ""); risk == nil {
		t.Fatal("a Cloud v1 device whose agent version is unknown must count as at risk")
	}
}

func TestConfirmCloudV1DisconnectWithoutATerminalFailsAndNamesTheFlag(t *testing.T) {
	for _, mode := range []string{"no terminal", "--json"} {
		t.Run(mode, func(t *testing.T) {
			asked := cloudV1Terminal(t, mode == "--json", true)
			jsonOutput = mode == "--json"
			conn, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1)

			err := confirmCloudV1Disconnect(context.Background(), conn, versionResponse(cloudV1OldAgent), cloudV1NewAgent, false)
			if err == nil {
				t.Fatal("update of a Cloud v1 device went ahead without anyone agreeing to it")
			}
			if errors.Is(err, ErrUserCancelled) {
				t.Fatalf("a refusal must exit non-zero, got a cancellation: %v", err)
			}
			if len(*asked) != 0 {
				t.Fatalf("prompted without a terminal: %q", *asked)
			}
			message := ansi.Strip(err.Error())
			for _, want := range []string{"thor", "Wendy Cloud", "Cloud v1", "--allow-cloud-v1-disconnect", "wendy device enroll"} {
				if !strings.Contains(message, want) {
					t.Errorf("error does not mention %q:\n%s", want, message)
				}
			}
			// JSON mode reports the way out as next_steps.
			steps := strings.Join(NextSteps(err), "\n")
			if !strings.Contains(steps, "--allow-cloud-v1-disconnect") {
				t.Errorf("next steps do not name the flag: %q", steps)
			}
		})
	}
}

func TestConfirmCloudV1DisconnectAsksAtATerminal(t *testing.T) {
	conn, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1)
	current := versionResponse(cloudV1OldAgent)

	asked := cloudV1Terminal(t, true, false)
	var err error
	shown := ansi.Strip(captureStderr(t, func() {
		err = confirmCloudV1Disconnect(context.Background(), conn, current, cloudV1NewAgent, false)
	}))
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("declining returned %v, want a cancellation", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "Update the agent anyway?" {
		t.Fatalf("asked %q", *asked)
	}
	for _, want := range []string{
		"This update will disconnect thor from Wendy Cloud.",
		cloudV1OldAgent, cloudV1NewAgent,
		"local network or USB",
		"wendy device unenroll --device <device-address>",
		"wendy cloud login",
		"wendy device enroll --device <device-address>",
		"Nothing was changed",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("warning does not say %q:\n%s", want, shown)
		}
	}
	if strings.Contains(shown, "through Wendy Cloud right now") {
		t.Errorf("a LAN connection was described as a Cloud connection:\n%s", shown)
	}

	asked = cloudV1Terminal(t, true, true)
	captureStderr(t, func() {
		err = confirmCloudV1Disconnect(context.Background(), conn, current, cloudV1NewAgent, false)
	})
	if err != nil || len(*asked) != 1 {
		t.Fatalf("agreeing returned %v after %d prompts", err, len(*asked))
	}
}

func TestConfirmCloudV1DisconnectSaysWhenTheConnectionItselfWillStop(t *testing.T) {
	conn, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1)
	throughCloudV1(conn)
	cloudV1Terminal(t, true, false)
	shown := ansi.Strip(captureStderr(t, func() {
		_ = confirmCloudV1Disconnect(context.Background(), conn, versionResponse(cloudV1OldAgent), cloudV1NewAgent, false)
	}))
	if !strings.Contains(shown, "You are connected to thor through Wendy Cloud right now") {
		t.Fatalf("warning does not say this connection is the one that stops:\n%s", shown)
	}

	cloudV1Terminal(t, false, false)
	err := confirmCloudV1Disconnect(context.Background(), conn, versionResponse(cloudV1OldAgent), cloudV1NewAgent, false)
	if err == nil || !strings.Contains(err.Error(), "through Wendy Cloud") {
		t.Fatalf("refusal does not say this connection is the one that stops: %v", err)
	}
}

func TestConfirmCloudV1DisconnectFlagSkipsThePrompt(t *testing.T) {
	conn, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1)
	for _, interactive := range []bool{false, true} {
		asked := cloudV1Terminal(t, interactive, false)
		var err error
		shown := ansi.Strip(captureStderr(t, func() {
			err = confirmCloudV1Disconnect(context.Background(), conn, versionResponse(cloudV1OldAgent), cloudV1NewAgent, true)
		}))
		if err != nil || len(*asked) != 0 {
			t.Fatalf("interactive=%v: flag returned %v after %d prompts", interactive, err, len(*asked))
		}
		if !strings.Contains(shown, "disconnect thor from Wendy Cloud") {
			t.Errorf("interactive=%v: the flag hid what the update does:\n%s", interactive, shown)
		}
	}
}

// Nothing to lose, nothing known, or nothing enrolled: exactly today's update.
func TestConfirmCloudV1DisconnectLeavesOtherDevicesAlone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enrollment cloudV1Enrollment
		current    string
	}{
		{"Cloud v1 device already without the tunnel", enrolledCloudV1ReportedByV2, cloudV1NewAgent},
		{"Cloud v2 device", enrolledCloudV2, cloudV1OldAgent},
		{"not enrolled", notEnrolled, cloudV1OldAgent},
		{"probe fails", enrollmentProbeFails, cloudV1OldAgent},
	} {
		for _, interactive := range []bool{false, true} {
			asked := cloudV1Terminal(t, interactive, false)
			conn, _ := cloudV1Conn(t, &cloudV1Agent{}, tc.enrollment)
			var err error
			shown := captureStderr(t, func() {
				err = confirmCloudV1Disconnect(context.Background(), conn, versionResponse(tc.current), cloudV1NewAgent, false)
			})
			if err != nil || len(*asked) != 0 || shown != "" {
				t.Errorf("%s (interactive=%v): err=%v prompts=%d output=%q", tc.name, interactive, err, len(*asked), shown)
			}
		}
	}
}

func TestDeviceUpdateRefusesToDisconnectCloudV1DeviceWithoutATerminal(t *testing.T) {
	agent := &cloudV1Agent{version: cloudV1OldAgent}
	serveCloudV1Device(t, agent, enrolledCloudV1)
	asked := cloudV1Terminal(t, false, true)

	cmd := newDeviceUpdateCmd()
	cmd.SetArgs([]string{"--binary", cloudV1AgentBinary(t)})
	_, err := captureCommandStdout(t, cmd.Execute)
	if err == nil || !strings.Contains(err.Error(), "--allow-cloud-v1-disconnect") {
		t.Fatalf("device update error = %v, want a refusal naming the flag", err)
	}
	if got := agent.uploads.Load(); got != 0 {
		t.Fatalf("uploaded the agent %d times before anyone agreed", got)
	}
	if len(*asked) != 0 {
		t.Fatalf("prompted without a terminal: %q", *asked)
	}
}

// --yes answers the OS update prompt. It is not consent to this.
func TestDeviceUpdateYesDoesNotAllowCloudV1Disconnect(t *testing.T) {
	agent := &cloudV1Agent{version: cloudV1OldAgent}
	serveCloudV1Device(t, agent, enrolledCloudV1)
	cloudV1Terminal(t, false, true)

	cmd := newDeviceUpdateCmd()
	cmd.SetArgs([]string{"--binary", cloudV1AgentBinary(t), "--yes"})
	_, err := captureCommandStdout(t, cmd.Execute)
	if err == nil || !strings.Contains(err.Error(), "--allow-cloud-v1-disconnect") || agent.uploads.Load() != 0 {
		t.Fatalf("--yes let the update through: err=%v uploads=%d", err, agent.uploads.Load())
	}
}

func TestDeviceUpdateAllowCloudV1DisconnectFlagUploads(t *testing.T) {
	agent := &cloudV1Agent{version: cloudV1OldAgent}
	serveCloudV1Device(t, agent, enrolledCloudV1)
	asked := cloudV1Terminal(t, false, false)

	cmd := newDeviceUpdateCmd()
	cmd.SetArgs([]string{"--binary", cloudV1AgentBinary(t), "--allow-cloud-v1-disconnect"})
	var err error
	captureStderr(t, func() { _, err = captureCommandStdout(t, cmd.Execute) })
	if err == nil || !strings.Contains(err.Error(), cloudV1UploadReached) {
		t.Fatalf("device update error = %v, want the upload to be reached", err)
	}
	if got := agent.uploads.Load(); got != 1 || len(*asked) != 0 {
		t.Fatalf("uploads=%d prompts=%d, want one upload and no prompt", got, len(*asked))
	}
}

func TestDeviceUpdateAsksBeforeDisconnectingCloudV1Device(t *testing.T) {
	agent := &cloudV1Agent{version: cloudV1OldAgent}
	serveCloudV1Device(t, agent, enrolledCloudV1)
	asked := cloudV1Terminal(t, true, false)

	cmd := newDeviceUpdateCmd()
	cmd.SetArgs([]string{"--binary", cloudV1AgentBinary(t)})
	var err error
	shown := ansi.Strip(captureStderr(t, func() { _, err = captureCommandStdout(t, cmd.Execute) }))
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("declining returned %v, want a cancellation", err)
	}
	if len(*asked) != 1 || agent.uploads.Load() != 0 {
		t.Fatalf("prompts=%d uploads=%d, want one prompt and no upload", len(*asked), agent.uploads.Load())
	}
	// The test agent is reached over a local socket, as on the device itself.
	if !strings.Contains(shown, "This update will disconnect this device from Wendy Cloud.") {
		t.Fatalf("no warning before the prompt:\n%s", shown)
	}
}

func TestDeviceUpdateLeavesOtherDevicesAlone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enrollment cloudV1Enrollment
		current    string
	}{
		{"Cloud v1 device already without the tunnel", enrolledCloudV1ReportedByV2, cloudV1NewAgent},
		{"Cloud v2 device", enrolledCloudV2, cloudV1OldAgent},
		{"not enrolled", notEnrolled, cloudV1OldAgent},
		{"probe fails", enrollmentProbeFails, cloudV1OldAgent},
		{"agent without an enrollment RPC", enrollmentUnsupported, cloudV1OldAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &cloudV1Agent{version: tc.current}
			serveCloudV1Device(t, agent, tc.enrollment)
			asked := cloudV1Terminal(t, false, false)

			cmd := newDeviceUpdateCmd()
			cmd.SetArgs([]string{"--binary", cloudV1AgentBinary(t)})
			_, err := captureCommandStdout(t, cmd.Execute)
			if err == nil || !strings.Contains(err.Error(), cloudV1UploadReached) {
				t.Fatalf("device update error = %v, want the upload to be reached as before", err)
			}
			if got := agent.uploads.Load(); got != 1 || len(*asked) != 0 {
				t.Fatalf("uploads=%d prompts=%d, want one upload and no prompt", got, len(*asked))
			}
		})
	}
}

// The release path refuses before it downloads anything.
func TestDeviceUpdateReleaseRefusesBeforeDownloading(t *testing.T) {
	agent := &cloudV1Agent{version: cloudV1OldAgent}
	serveCloudV1Device(t, agent, enrolledCloudV1)
	cloudV1Terminal(t, false, false)
	previous := resolveAgentVersionFn
	t.Cleanup(func() { resolveAgentVersionFn = previous })
	resolveAgentVersionFn = func(bool) (string, string, error) { return cloudV1NewAgent, "test", nil }

	cmd := newDeviceUpdateCmd()
	cmd.SetArgs(nil)
	_, err := captureCommandStdout(t, cmd.Execute)
	if err == nil || !strings.Contains(err.Error(), "--allow-cloud-v1-disconnect") || !strings.Contains(err.Error(), cloudV1NewAgent) {
		t.Fatalf("device update error = %v, want a refusal naming the flag and the release", err)
	}
	if got := agent.uploads.Load(); got != 0 {
		t.Fatalf("uploaded the agent %d times before anyone agreed", got)
	}
}

func TestPushAgentRefusesToDisconnectCloudV1Device(t *testing.T) {
	agent := &cloudV1Agent{version: cloudV1OldAgent}
	serveCloudV1Device(t, agent, enrolledCloudV1)
	cloudV1Terminal(t, false, false)

	cmd := newDevicePushAgentCmd()
	cmd.SetArgs([]string{cloudV1AgentBinary(t)})
	var err error
	captureStderr(t, func() { err = cmd.Execute() })
	if err == nil || !strings.Contains(err.Error(), "--allow-cloud-v1-disconnect") || agent.uploads.Load() != 0 {
		t.Fatalf("push-agent: err=%v uploads=%d, want a refusal before the upload", err, agent.uploads.Load())
	}

	cmd = newDevicePushAgentCmd()
	cmd.SetArgs([]string{cloudV1AgentBinary(t), "--allow-cloud-v1-disconnect"})
	captureStderr(t, func() { err = cmd.Execute() })
	if err == nil || !strings.Contains(err.Error(), cloudV1UploadReached) || agent.uploads.Load() != 1 {
		t.Fatalf("push-agent with the flag: err=%v uploads=%d, want the upload", err, agent.uploads.Load())
	}
}

// The offer on connect turns into an upload on a single keypress, in the middle
// of another command. It is not made for a device the update would disconnect.
func TestConnectUpdateOfferIsNotMadeForCloudV1Device(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	previousVersion, previousConfirm := version.Version, confirmFn
	t.Cleanup(func() { version.Version, confirmFn = previousVersion, previousConfirm })
	version.Version = cloudV1NewAgent
	cloudV1Terminal(t, true, true)
	var offers []string
	confirmFn = func(question string) bool {
		offers = append(offers, question)
		return false
	}

	agent := &cloudV1Agent{version: cloudV1OldAgent}
	conn, _ := cloudV1Conn(t, agent, enrolledCloudV1)
	var got *grpcclient.AgentConnection
	var err error
	shown := ansi.Strip(captureStderr(t, func() { got, err = checkAndOfferUpdate(context.Background(), conn) }))
	if err != nil || got != conn {
		t.Fatalf("checkAndOfferUpdate = %v, %v; want the same connection", got, err)
	}
	if len(offers) != 0 || agent.uploads.Load() != 0 {
		t.Fatalf("offered the update %q (uploads=%d) to a device it would disconnect", offers, agent.uploads.Load())
	}
	for _, want := range []string{"Agent is behind the CLI", "Wendy Cloud", "wendy device update"} {
		if !strings.Contains(shown, want) {
			t.Errorf("notice does not say %q:\n%s", want, shown)
		}
	}

	// Any other device still gets the offer.
	other, _ := cloudV1Conn(t, &cloudV1Agent{version: cloudV1OldAgent}, enrolledCloudV2)
	captureStderr(t, func() { got, err = checkAndOfferUpdate(context.Background(), other) })
	if err != nil || got != other || len(offers) != 1 {
		t.Fatalf("Cloud v2 device: conn=%v err=%v offers=%q, want one offer", got, err, offers)
	}
}

func TestOSUpdateAgentStepGuardsCloudV1Device(t *testing.T) {
	previousResolve, previousUpdate := resolveAgentVersionFn, performAgentUpdateFn
	t.Cleanup(func() { resolveAgentVersionFn, performAgentUpdateFn = previousResolve, previousUpdate })
	resolveAgentVersionFn = func(bool) (string, string, error) { return cloudV1NewAgent, "test", nil }
	updates := 0
	performAgentUpdateFn = func(context.Context, *grpcclient.AgentConnection, string, string, bool) error {
		updates++
		return errors.New(cloudV1UploadReached)
	}
	run := func(conn *grpcclient.AgentConnection, allow bool) error {
		var err error
		captureStderr(t, func() {
			_, err = captureCommandStdout(t, func() error {
				_, updateErr := ensureAgentUpToDate(context.Background(), conn, versionResponse(cloudV1OldAgent), false, allow)
				return updateErr
			})
		})
		return err
	}

	conn, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1)
	cloudV1Terminal(t, false, false)
	if err := run(conn, false); err == nil || !strings.Contains(err.Error(), "--allow-cloud-v1-disconnect") || updates != 0 {
		t.Fatalf("os update without a terminal: err=%v updates=%d, want a refusal before the agent update", err, updates)
	}

	asked := cloudV1Terminal(t, true, false)
	if err := run(conn, false); !errors.Is(err, ErrUserCancelled) || updates != 0 || len(*asked) != 1 {
		t.Fatalf("os update declined: err=%v updates=%d prompts=%d", err, updates, len(*asked))
	}

	cloudV1Terminal(t, false, false)
	if err := run(conn, true); err == nil || !strings.Contains(err.Error(), cloudV1UploadReached) || updates != 1 {
		t.Fatalf("os update with the flag: err=%v updates=%d, want the agent update", err, updates)
	}

	other, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV2)
	if err := run(other, false); err == nil || !strings.Contains(err.Error(), cloudV1UploadReached) || updates != 2 {
		t.Fatalf("os update of a Cloud v2 device: err=%v updates=%d, want the agent update as before", err, updates)
	}
}

func TestAgentUpdateCommandsHaveTheCloudV1Flag(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{
		"device update":     newDeviceUpdateCmd(),
		"os update":         newOSUpdateCmd(),
		"device push-agent": newDevicePushAgentCmd(),
	} {
		if cmd.Flags().Lookup("allow-cloud-v1-disconnect") == nil {
			t.Errorf("%s has no --allow-cloud-v1-disconnect flag", name)
		}
	}
}

// The update key in `wendy discover` runs inside the table, where nothing can
// be asked. It leaves the device alone and names the command that asks.
func TestDiscoverUpdateKeyLeavesCloudV1DeviceAlone(t *testing.T) {
	agent := &cloudV1Agent{version: cloudV1OldAgent}
	serveCloudV1Device(t, agent, enrolledCloudV1)
	previous := resolveAgentVersionFn
	t.Cleanup(func() { resolveAgentVersionFn = previous })
	resolveAgentVersionFn = func(bool) (string, string, error) { return cloudV1NewAgent, "test", nil }

	m := newDiscoverModel(context.Background(), defaultOpts(), false)
	msg, ok := m.startDeviceUpdateCmd("203.0.113.1:50051", "Tom Thor")().(discoverUpdateDoneMsg)
	if !ok {
		t.Fatal("update worker returned an unexpected message")
	}
	if msg.err != nil || agent.uploads.Load() != 0 {
		t.Fatalf("err=%v uploads=%d, want no upload and no error", msg.err, agent.uploads.Load())
	}
	flash, isError := discoverUpdateFlash(msg)
	if isError || !strings.Contains(flash, "Tom Thor") || !strings.Contains(flash, "Wendy Cloud") || !strings.Contains(flash, "'wendy device update'") {
		t.Fatalf("flash = %q (error=%v)", flash, isError)
	}
}

func TestDiscoverCloudV1NoteNamesTheCommandForTheTable(t *testing.T) {
	conn, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV1)
	throughCloudV1(conn)
	note := discoverCloudV1Note(context.Background(), conn, versionResponse(cloudV1OldAgent), cloudV1NewAgent, "thor", "wendy cloud device update")
	if !strings.Contains(note, "thor") || !strings.Contains(note, "'wendy cloud device update'") {
		t.Fatalf("note = %q", note)
	}
	other, _ := cloudV1Conn(t, &cloudV1Agent{}, enrolledCloudV2)
	if note := discoverCloudV1Note(context.Background(), other, versionResponse(cloudV1OldAgent), cloudV1NewAgent, "thor", "wendy cloud device update"); note != "" {
		t.Fatalf("Cloud v2 device got the note %q", note)
	}
}

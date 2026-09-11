package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func runDataEnroll(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newDataEnrollCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func readStaged(t *testing.T, dir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, pkienroll.StagedFileName))
	if err != nil {
		t.Fatalf("reading staged file: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding staged file: %v", err)
	}
	return got
}

func TestDataEnrollStagesTheCredential(t *testing.T) {
	dir := t.TempDir()
	out, err := runDataEnroll(t,
		"--tenant", "022b7284-f7f3-4d86-b844-d105a7c06d9e",
		"--token", "tok-abc",
		"--device-id", "sh/wendy/2/408",
		"--environment", "dev",
		"--config-path", dir,
	)
	if err != nil {
		t.Fatalf("data enroll: %v (%s)", err, out)
	}

	got := readStaged(t, dir)
	if got["token"] != "tok-abc" {
		t.Errorf("token = %q", got["token"])
	}
	if got["tenantUUID"] != "022b7284-f7f3-4d86-b844-d105a7c06d9e" {
		t.Errorf("tenantUUID = %q", got["tenantUUID"])
	}
	if got["deviceID"] != "sh/wendy/2/408" {
		t.Errorf("deviceID = %q", got["deviceID"])
	}
	if got["environment"] != "dev" {
		t.Errorf("environment = %q", got["environment"])
	}
	// Optional fields that were not given must be absent rather than empty,
	// so the agent's own defaulting decides them.
	if _, present := got["csrEndpoint"]; present {
		t.Error("csrEndpoint was written although it was not given")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, pkienroll.StagedFileName))
		if err != nil {
			t.Fatalf("stat staged file: %v", err)
		}
		// It holds a bearer credential.
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %v, want 0600", got)
		}
	}
}

func TestDataEnrollTakesTheTenantFromTheTokenClaim(t *testing.T) {
	dir := t.TempDir()
	enc := base64.RawURLEncoding.EncodeToString
	payload, err := json.Marshal(map[string]any{"type": "asset_enrollment", "org_id": 2, "asset_id": 408,
		"tenant_uuid": "022b7284-f7f3-4d86-b844-d105a7c06d9e"})
	if err != nil {
		t.Fatalf("encoding claims: %v", err)
	}
	token := enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"

	if out, err := runDataEnroll(t, "--token", token, "--config-path", dir); err != nil {
		t.Fatalf("data enroll: %v (%s)", err, out)
	}
	if got := readStaged(t, dir)["tenantUUID"]; got != "022b7284-f7f3-4d86-b844-d105a7c06d9e" {
		t.Errorf("tenantUUID = %q, want the token's claim", got)
	}
}

func TestDataEnrollRequiresATenantForAnOpaqueToken(t *testing.T) {
	// pki-core's own enrollment tokens are opaque and carry no claims, so the
	// tenant has to be given. Refusing here is better than staging a file the
	// agent will reject at startup.
	dir := t.TempDir()
	_, err := runDataEnroll(t, "--token", "opaque-value", "--config-path", dir)
	if err == nil {
		t.Fatal("want an error naming --tenant")
	}
	if _, statErr := os.Stat(filepath.Join(dir, pkienroll.StagedFileName)); !os.IsNotExist(statErr) {
		t.Error("a file was staged despite the refusal")
	}
}

func TestDataEnrollRequiresAToken(t *testing.T) {
	if _, err := runDataEnroll(t, "--tenant", "t", "--config-path", t.TempDir()); err == nil {
		t.Fatal("want an error naming --token")
	}
}

func TestAgentConfigPathFollowsTheAgent(t *testing.T) {
	t.Setenv("WENDY_CONFIG_PATH", "")
	if got := agentConfigPath(); got != defaultAgentConfigPath {
		t.Errorf("agentConfigPath = %q, want %q", got, defaultAgentConfigPath)
	}
	t.Setenv("WENDY_CONFIG_PATH", "/tmp/wendy-scratch")
	if got := agentConfigPath(); got != "/tmp/wendy-scratch" {
		t.Errorf("agentConfigPath = %q, want the override", got)
	}
}

// --- dispatch: which path the flags select -------------------------------

// recordRemote swaps the RPC path for a recorder, so the dispatch can be
// asserted without a device. It restores the real function on cleanup.
func recordRemote(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	original := dataEnrollRemote
	dataEnrollRemote = func(_ *cobra.Command, tenant, token, deviceID, csrEndpoint, environment string) error {
		calls = append(calls, strings.Join([]string{tenant, token, deviceID, csrEndpoint, environment}, "|"))
		return nil
	}
	t.Cleanup(func() { dataEnrollRemote = original })
	return &calls
}

func TestDataEnrollCallsTheAgentByDefault(t *testing.T) {
	// The device this would reach has no shell, so the RPC - not a local file -
	// has to be what an operator gets when they type nothing extra.
	calls := recordRemote(t)
	out, err := runDataEnroll(t,
		"--tenant", "022b7284-f7f3-4d86-b844-d105a7c06d9e",
		"--token", "tok-abc",
		"--device-id", "sh/wendy/2/408",
		"--environment", "dev",
	)
	if err != nil {
		t.Fatalf("data enroll: %v (%s)", err, out)
	}
	want := "022b7284-f7f3-4d86-b844-d105a7c06d9e|tok-abc|sh/wendy/2/408||dev"
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("remote calls = %v, want exactly [%s]", *calls, want)
	}
}

func TestDataEnrollLocalWritesTheFileAndCallsNothing(t *testing.T) {
	calls := recordRemote(t)
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_PATH", dir)
	out, err := runDataEnroll(t, "--local",
		"--tenant", "022b7284-f7f3-4d86-b844-d105a7c06d9e",
		"--token", "tok-abc",
	)
	if err != nil {
		t.Fatalf("data enroll --local: %v (%s)", err, out)
	}
	if len(*calls) != 0 {
		t.Fatalf("the agent was called although --local was given: %v", *calls)
	}
	if got := readStaged(t, dir)["token"]; got != "tok-abc" {
		t.Errorf("token = %q", got)
	}
}

func TestDataEnrollConfigPathImpliesLocal(t *testing.T) {
	// A config path names a directory on THIS machine; a remote agent writes
	// only into its own, so the flag can mean nothing else.
	calls := recordRemote(t)
	dir := t.TempDir()
	if out, err := runDataEnroll(t, "--token", "tok-abc", "--tenant", "t", "--config-path", dir); err != nil {
		t.Fatalf("data enroll: %v (%s)", err, out)
	}
	if len(*calls) != 0 {
		t.Fatalf("the agent was called although --config-path was given: %v", *calls)
	}
	if got := readStaged(t, dir)["token"]; got != "tok-abc" {
		t.Errorf("token = %q", got)
	}
}

func TestDataEnrollLeavesAnOpaqueTokensTenantToTheAgent(t *testing.T) {
	// The local path must refuse (nothing downstream can resolve the tenant),
	// but the agent can: it may hold a tenant the caller did not name. Refusing
	// here would burn a usable token for nothing.
	calls := recordRemote(t)
	if out, err := runDataEnroll(t, "--token", "opaque-value"); err != nil {
		t.Fatalf("data enroll: %v (%s)", err, out)
	}
	if len(*calls) != 1 {
		t.Fatalf("remote calls = %v, want one", *calls)
	}
}

// --- the RPC call and what it prints -------------------------------------

// stubProvisioningServer answers StagePKIEnrollment with one canned response.
type stubProvisioningServer struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	got  *agentpbv2.StagePKIEnrollmentRequest
	resp *agentpbv2.StagePKIEnrollmentResponse
}

func (s *stubProvisioningServer) StagePKIEnrollment(_ context.Context, req *agentpbv2.StagePKIEnrollmentRequest) (*agentpbv2.StagePKIEnrollmentResponse, error) {
	s.got = req
	return s.resp, nil
}

func startStubProvisioning(t *testing.T, stub *stubProvisioningServer) agentpbv2.WendyProvisioningServiceClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	server := grpc.NewServer()
	agentpbv2.RegisterWendyProvisioningServiceServer(server, stub)
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		server.Stop()
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
	})
	return agentpbv2.NewWendyProvisioningServiceClient(conn)
}

// callStagePKI drives the command's RPC path against a stub and returns what
// the operator would see.
func callStagePKI(t *testing.T, resp *agentpbv2.StagePKIEnrollmentResponse) (*agentpbv2.StagePKIEnrollmentRequest, string, error) {
	t.Helper()
	stub := &stubProvisioningServer{resp: resp}
	client := startStubProvisioning(t, stub)

	cmd := newDataEnrollCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	err := stagePKIEnrollmentOverRPC(cmd, client,
		"022b7284-f7f3-4d86-b844-d105a7c06d9e", "tok-abc", "sh/wendy/2/408", "", "dev")
	return stub.got, out.String(), err
}

func TestStagePKIEnrollmentOverRPCReportsSuccess(t *testing.T) {
	uri := "spiffe://wendy.sh/tenant/022b7284-f7f3-4d86-b844-d105a7c06d9e/device/sh/wendy/2/408"
	got, out, err := callStagePKI(t, &agentpbv2.StagePKIEnrollmentResponse{
		Status:       agentpbv2.StagePKIEnrollmentResponse_STATUS_ENROLLED,
		StagedPath:   "/etc/wendy-agent/pki-enrollment.json",
		SpiffeUri:    uri,
		DeviceName:   "sh/wendy/2/408",
		NotAfterUnix: 1790000000,
	})
	if err != nil {
		t.Fatalf("stagePKIEnrollmentOverRPC: %v (%s)", err, out)
	}
	// Every flag has to survive the trip; a mistyped device_id is a hard
	// refusal on the far side, not a rename.
	if got.GetToken() != "tok-abc" || got.GetDeviceId() != "sh/wendy/2/408" ||
		got.GetTenantUuid() != "022b7284-f7f3-4d86-b844-d105a7c06d9e" || got.GetEnvironment() != "dev" {
		t.Errorf("request = %v", got)
	}
	if !strings.Contains(out, uri) {
		t.Errorf("the identity is not reported: %s", out)
	}
}

func TestStagePKIEnrollmentOverRPCReportsADeferral(t *testing.T) {
	// Deferred is not a failure: the token is kept and retried, so the command
	// must exit zero and say what to do.
	_, out, err := callStagePKI(t, &agentpbv2.StagePKIEnrollmentResponse{
		Status:     agentpbv2.StagePKIEnrollmentResponse_STATUS_DEFERRED,
		StagedPath: "/etc/wendy-agent/pki-enrollment.json",
		Reason:     "the device is not enrolled with Wendy Cloud yet",
	})
	if err != nil {
		t.Fatalf("a deferral must not be an error: %v", err)
	}
	if !strings.Contains(out, "not enrolled with Wendy Cloud") {
		t.Errorf("the reason is not reported: %s", out)
	}
}

func TestStagePKIEnrollmentOverRPCFailsOnARefusal(t *testing.T) {
	// Refused IS a failure, and the message has to distinguish "retry" from
	// "mint a fresh token", because the credential is single-use.
	_, _, err := callStagePKI(t, &agentpbv2.StagePKIEnrollmentResponse{
		Status: agentpbv2.StagePKIEnrollmentResponse_STATUS_REFUSED,
		Reason: "pki-core csr frontend answered 401",
	})
	if err == nil {
		t.Fatal("a refusal must be an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "fresh") {
		t.Errorf("err = %v, want the status and the instruction to mint a fresh token", err)
	}
}

func TestStagePKIEnrollmentOverRPCReportsAnExistingIdentity(t *testing.T) {
	uri := "spiffe://wendy.sh/tenant/022b7284-f7f3-4d86-b844-d105a7c06d9e/device/sh/wendy/2/408"
	_, out, err := callStagePKI(t, &agentpbv2.StagePKIEnrollmentResponse{
		Status:    agentpbv2.StagePKIEnrollmentResponse_STATUS_ALREADY_ENROLLED,
		SpiffeUri: uri,
	})
	if err != nil {
		t.Fatalf("an existing identity must not be an error: %v", err)
	}
	if !strings.Contains(out, "not spent") {
		t.Errorf("the operator is not told the token survived: %s", out)
	}
}

func TestStagePKIEnrollmentOverRPCFailsOnAFailure(t *testing.T) {
	_, _, err := callStagePKI(t, &agentpbv2.StagePKIEnrollmentResponse{
		Status: agentpbv2.StagePKIEnrollmentResponse_STATUS_FAILED,
		Reason: "dial tcp: connection refused",
	})
	if err == nil {
		t.Fatal("a failure must be an error")
	}
}

func TestDataEnrollRefusesAnUnsendableToken(t *testing.T) {
	// A token pasted with its surrounding text cannot ride in an Authorization
	// header. Refusing here saves a round trip whose failure reads like a
	// refusal from pki-core and wrongly advises minting a fresh token.
	calls := recordRemote(t)
	_, err := runDataEnroll(t, "--tenant", "t", "--token", "token_value: a1b2c3\n")
	if err == nil {
		t.Fatal("want an error naming the token")
	}
	if !strings.Contains(err.Error(), "--token") {
		t.Errorf("err = %v, want it to name the flag", err)
	}
	if len(*calls) != 0 {
		t.Errorf("the agent was called with an unsendable token: %v", *calls)
	}
}

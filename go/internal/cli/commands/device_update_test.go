package commands

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func makeMacAgentZIP(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, contents := range entries {
		entry, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip.Create(%q): %v", name, err)
		}
		if _, err := entry.Write(contents); err != nil {
			t.Fatalf("writing %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// fakeUpdateAgentStream satisfies agentpb.WendyAgentService_UpdateAgentClient
// (a grpc.BidiStreamingClient[UpdateAgentRequest, UpdateAgentResponse]) via
// the embedded-nil trick used elsewhere in this package (see
// fakeWriteChunksStream in chunkpush_test.go). It only records the requests
// sent so a test can inspect the final control command.
type fakeUpdateAgentStream struct {
	grpc.BidiStreamingClient[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse]
	sent []*agentpb.UpdateAgentRequest
}

func (s *fakeUpdateAgentStream) Send(req *agentpb.UpdateAgentRequest) error {
	s.sent = append(s.sent, req)
	return nil
}

func (s *fakeUpdateAgentStream) CloseSend() error {
	return nil
}

// sendAgentUpdate streams the binary in chunks and then a final control
// command carrying the sha256 and (optionally) a detached signature. This
// pins the seam a signer will populate: whatever bytes the caller passes as
// signature must land verbatim on the v1 proto's Update.Signature field,
// which is what the agent's (currently-disabled) verifier reads.
func TestSendAgentUpdateSignatureReachesProtoField(t *testing.T) {
	stream := &fakeUpdateAgentStream{}
	binaryData := []byte("fake-agent-binary")
	sha256Hash := "deadbeef"
	signature := []byte("fake-ml-dsa65-signature")

	if err := sendAgentUpdate(stream, binaryData, sha256Hash, signature); err != nil {
		t.Fatalf("sendAgentUpdate() error = %v", err)
	}

	if len(stream.sent) == 0 {
		t.Fatal("sendAgentUpdate() sent no requests")
	}
	last := stream.sent[len(stream.sent)-1]
	update := last.GetControl().GetUpdate()
	if update == nil {
		t.Fatalf("last sent request has no Control.Update: %+v", last)
	}
	if update.GetSha256() != sha256Hash {
		t.Errorf("Update.Sha256 = %q, want %q", update.GetSha256(), sha256Hash)
	}
	if !bytes.Equal(update.GetSignature(), signature) {
		t.Errorf("Update.Signature = %q, want %q", update.GetSignature(), signature)
	}
}

// TestSendAgentUpdateNilSignatureLeavesFieldEmpty locks in the no-signer-yet
// default: an absent signature must not synthesize any bytes on the wire.
func TestSendAgentUpdateNilSignatureLeavesFieldEmpty(t *testing.T) {
	stream := &fakeUpdateAgentStream{}
	if err := sendAgentUpdate(stream, []byte("data"), "abc123", nil); err != nil {
		t.Fatalf("sendAgentUpdate() error = %v", err)
	}
	last := stream.sent[len(stream.sent)-1]
	if sig := last.GetControl().GetUpdate().GetSignature(); len(sig) != 0 {
		t.Errorf("Update.Signature = %q, want empty", sig)
	}
}

func TestIsZipArchive(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"zip local-file-header magic", []byte{'P', 'K', 0x03, 0x04, 0x14, 0x00}, true},
		{"ELF is not a zip", makeELFHeader(183), false},
		{"empty data", nil, false},
		{"too short for magic", []byte{'P', 'K'}, false},
		{"zip central-directory-only magic is not a local-file-header", []byte{'P', 'K', 0x05, 0x06}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isZipArchive(tc.data); got != tc.want {
				t.Errorf("isZipArchive(%v) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestDarwinAgentExecutableSHA256(t *testing.T) {
	t.Run("hashes the executable inside the app bundle", func(t *testing.T) {
		archive := makeMacAgentZIP(t, map[string][]byte{
			"WendyAgentMac.app/Contents/Info.plist":          []byte("plist"),
			"WendyAgentMac.app/Contents/MacOS/WendyAgentMac": []byte("hello"),
		})
		got, err := darwinAgentExecutableSHA256(archive)
		if err != nil {
			t.Fatalf("darwinAgentExecutableSHA256() error = %v", err)
		}
		const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
		if got != want {
			t.Fatalf("darwinAgentExecutableSHA256() = %q, want %q", got, want)
		}
	})

	t.Run("rejects an archive without the agent executable", func(t *testing.T) {
		archive := makeMacAgentZIP(t, map[string][]byte{
			"WendyAgentMac.app/Contents/Info.plist": []byte("plist"),
		})
		if _, err := darwinAgentExecutableSHA256(archive); err == nil {
			t.Fatal("darwinAgentExecutableSHA256() error = nil, want missing executable error")
		}
	})

	t.Run("rejects ambiguous app bundles", func(t *testing.T) {
		archive := makeMacAgentZIP(t, map[string][]byte{
			"WendyAgentMac.app/Contents/MacOS/WendyAgentMac": []byte("one"),
			"Other.app/Contents/MacOS/WendyAgentMac":         []byte("two"),
		})
		if _, err := darwinAgentExecutableSHA256(archive); err == nil {
			t.Fatal("darwinAgentExecutableSHA256() error = nil, want ambiguity error")
		}
	})
}

func TestCheckLocalAgentArtifact(t *testing.T) {
	zipData := []byte{'P', 'K', 0x03, 0x04, 0x14, 0x00, 0x00, 0x00}
	arm64ELF := makeELFHeader(183)
	amd64ELF := makeELFHeader(62)
	randomBytes := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}
	shellScript := []byte("#!/bin/sh\necho hi\n")

	tests := []struct {
		name         string
		data         []byte
		deviceOS     string
		deviceArch   string
		wantErr      bool
		wantContains []string
	}{
		{
			name:     "zip on darwin is accepted",
			data:     zipData,
			deviceOS: "darwin",
			wantErr:  false,
		},
		{
			name:         "linux ELF on darwin is rejected with build guidance",
			data:         arm64ELF,
			deviceOS:     "darwin",
			deviceArch:   "arm64",
			wantErr:      true,
			wantContains: []string{"macOS", "Build.sh"},
		},
		{
			name:         "random bytes on darwin are rejected",
			data:         randomBytes,
			deviceOS:     "darwin",
			wantErr:      true,
			wantContains: []string{"macOS"},
		},
		{
			name:         "zip on a Linux device names the reported OS",
			data:         zipData,
			deviceOS:     "ubuntu",
			wantErr:      true,
			wantContains: []string{"macOS agent zip", "ubuntu"},
		},
		{
			name:         "zip on an unknown-OS device falls back to linux",
			data:         zipData,
			deviceOS:     "",
			wantErr:      true,
			wantContains: []string{"linux"},
		},
		{
			name:       "matching ELF arch on a Linux device is accepted",
			data:       arm64ELF,
			deviceOS:   "wendyos",
			deviceArch: "arm64",
			wantErr:    false,
		},
		{
			name:       "mismatched ELF arch still delegates to checkELFArchitecture",
			data:       amd64ELF,
			deviceOS:   "",
			deviceArch: "arm64",
			wantErr:    true,
		},
		{
			name:       "non-ELF script is leniently accepted on Linux",
			data:       shellScript,
			deviceOS:   "wendyos",
			deviceArch: "arm64",
			wantErr:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLocalAgentArtifact(tc.data, tc.deviceOS, tc.deviceArch)
			if tc.wantErr && err == nil {
				t.Fatal("checkLocalAgentArtifact() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkLocalAgentArtifact() = %v, want nil", err)
			}
			for _, want := range tc.wantContains {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("checkLocalAgentArtifact() error = %v, want substring %q", err, want)
				}
			}
		})
	}
}

func TestAgentRestartTimeoutFor(t *testing.T) {
	tests := []struct {
		name   string
		osName string
		want   time.Duration
	}{
		{"darwin gets the extended timeout", "darwin", 60 * time.Second},
		{"darwin case-insensitive", "Darwin", 60 * time.Second},
		{"linux gets the default timeout", "linux", defaultAgentRestartTimeout},
		{"wendyos os-release id gets the default timeout", "wendyos", defaultAgentRestartTimeout},
		{"empty os gets the default timeout", "", defaultAgentRestartTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentRestartTimeoutFor(tc.osName); got != tc.want {
				t.Errorf("agentRestartTimeoutFor(%q) = %v, want %v", tc.osName, got, tc.want)
			}
		})
	}
}

func TestShouldReapplyBinary(t *testing.T) {
	tests := []struct {
		name           string
		binaryProvided bool
		outcome        osUpdateOutcome
		want           bool
	}{
		{
			name:           "--binary + OS applied + back online → re-apply",
			binaryProvided: true,
			outcome:        osUpdateOutcome{applied: true, online: true},
			want:           true,
		},
		{
			name:           "auto-download path is never re-applied",
			binaryProvided: false,
			outcome:        osUpdateOutcome{applied: true, online: true},
			want:           false,
		},
		{
			name:           "no OS update applied → nothing to survive",
			binaryProvided: true,
			outcome:        osUpdateOutcome{applied: false},
			want:           false,
		},
		{
			name:           "applied but device not confirmed online (cloud) → skip inline re-apply",
			binaryProvided: true,
			outcome:        osUpdateOutcome{applied: true, online: false},
			want:           false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldReapplyBinary(tc.binaryProvided, tc.outcome); got != tc.want {
				t.Fatalf("shouldReapplyBinary(%v, %+v) = %v, want %v", tc.binaryProvided, tc.outcome, got, tc.want)
			}
		})
	}
}

func TestWaitForUpdatedAgentReadyRetriesUntilReachable(t *testing.T) {
	started := time.Now()
	attempts := 0

	conn, err := waitForUpdatedAgentReady(context.Background(), func(context.Context) (*grpcclient.AgentConnection, error) {
		attempts++
		if elapsed := time.Since(started); elapsed < 15*time.Millisecond {
			t.Fatalf("reconnect attempted before initial restart delay: %s", elapsed)
		}
		if attempts < 3 {
			return nil, errors.New("agent restarting")
		}
		return &grpcclient.AgentConnection{}, nil
	}, agentRestartWaitOptions{
		InitialDelay: 15 * time.Millisecond,
		Timeout:      200 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("waitForUpdatedAgentReady() error = %v", err)
	}
	if conn == nil {
		t.Fatal("waitForUpdatedAgentReady() returned nil connection")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWaitForUpdatedAgentReadyReturnsLastReconnectError(t *testing.T) {
	wantErr := errors.New("connection refused")
	attempts := 0

	_, err := waitForUpdatedAgentReady(context.Background(), func(context.Context) (*grpcclient.AgentConnection, error) {
		attempts++
		return nil, wantErr
	}, agentRestartWaitOptions{
		InitialDelay: time.Millisecond,
		Timeout:      20 * time.Millisecond,
		PollInterval: 2 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("waitForUpdatedAgentReady() succeeded, want error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitForUpdatedAgentReady() error = %v, want wrapping %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "agent did not become reachable after update") {
		t.Fatalf("waitForUpdatedAgentReady() error = %q, want restart readiness context", err.Error())
	}
	if attempts == 0 {
		t.Fatal("reconnect was never attempted")
	}
}

func TestWaitForUpdatedAgentReadyHonorsCanceledContextDuringInitialDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	_, err := waitForUpdatedAgentReady(ctx, func(context.Context) (*grpcclient.AgentConnection, error) {
		attempts++
		return &grpcclient.AgentConnection{}, nil
	}, agentRestartWaitOptions{
		InitialDelay: 50 * time.Millisecond,
		Timeout:      200 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForUpdatedAgentReady() error = %v, want context.Canceled", err)
	}
	if attempts != 0 {
		t.Fatalf("reconnect attempts = %d, want 0", attempts)
	}
}

// agentUpdateTerminalError turns the update stream's terminal Recv error into
// what the user is told. A bare io.EOF / dropped transport is NOT a verdict:
// the agent restarts itself the moment the binary lands, which tears down the
// stream before the ack arrives, so those map to errAgentUpdateUnconfirmed for
// the caller to verify. Real gRPC statuses are surfaced with their message.
func TestAgentUpdateTerminalError(t *testing.T) {
	tests := []struct {
		name            string
		recvErr         error
		wantUnconfirmed bool
		wantSubstr      string
	}{
		{
			name:            "bare EOF is unconfirmed, not a failure",
			recvErr:         io.EOF,
			wantUnconfirmed: true,
		},
		{
			name:            "transport closing is unconfirmed",
			recvErr:         status.Error(codes.Unavailable, "transport is closing"),
			wantUnconfirmed: true,
		},
		{
			name:            "client cancel is unconfirmed",
			recvErr:         status.Error(codes.Canceled, "context canceled"),
			wantUnconfirmed: true,
		},
		{
			name:       "update already in progress explains the stale-lock reboot",
			recvErr:    status.Error(codes.FailedPrecondition, "an update is already in progress"),
			wantSubstr: "reboot",
		},
		{
			name:       "sha mismatch is reported verbatim",
			recvErr:    status.Error(codes.DataLoss, "SHA256 mismatch: expected aa, got bb"),
			wantSubstr: "SHA256 mismatch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := agentUpdateTerminalError(tc.recvErr)
			if err == nil {
				t.Fatal("agentUpdateTerminalError = nil, want error")
			}
			if got := errors.Is(err, errAgentUpdateUnconfirmed); got != tc.wantUnconfirmed {
				t.Fatalf("errors.Is(err, errAgentUpdateUnconfirmed) = %v, want %v (err: %v)", got, tc.wantUnconfirmed, err)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q should contain %q", err, tc.wantSubstr)
			}
		})
	}
}

// After an unconfirmed upload the CLI reconnects and checks what the device
// actually runs: the expected release version (or newer) proves the update
// landed; anything else means the old agent is still in place.
func TestAgentUpdateVerified(t *testing.T) {
	tests := []struct {
		name     string
		reported string
		expected string
		want     bool
	}{
		{"exact match", "2026.07.01-223311", "2026.07.01-223311", true},
		{"newer than expected", "2026.07.02-000001", "2026.07.01-223311", true},
		{"older agent still running", "2026.06.30-120000", "2026.07.01-223311", false},
		{"no expectation (--binary) passes", "dev-abc123", "", true},
		{"unknown reported version fails", "", "2026.07.01-223311", false},
		// CompareVersions ranks dev builds newest, but a device still
		// reporting a dev build after a RELEASE upload means the swap did
		// not land — the vacuous pass would hide exactly the silent no-op
		// this verification exists to catch.
		{"dev reported against a release expectation fails", "dev", "2026.07.01-223311", false},
		{"-dev suffix against a release expectation fails", "2026.01.01-000000-dev", "2026.07.01-223311", false},
		// ...but when the EXPECTED version is itself a dev build (a
		// workflow_dispatch publish can stamp a -dev version into the
		// manifest), a dev report is exactly what success looks like.
		{"dev reported against a dev expectation passes", "2026.01.01-000000-dev", "2026.01.01-000000-dev", true},
		{"plain dev against a dev expectation passes", "dev", "2026.01.01-000000-dev", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentUpdateVerified(tc.reported, tc.expected); got != tc.want {
				t.Fatalf("agentUpdateVerified(%q, %q) = %v, want %v", tc.reported, tc.expected, got, tc.want)
			}
		})
	}
}

// evaluateAgentUpdateOutcome precedence: a reported binary hash compared to
// the uploaded one is definitive in both directions (it is what makes dev
// pushes provable, since dev builds share identical version strings); version
// comparison is only the fallback for agents that cannot report a hash unless
// the caller requires hash proof (the macOS restart-verification path).
func TestEvaluateAgentUpdateOutcome(t *testing.T) {
	resp := func(ver, hash string) *agentpb.GetAgentVersionResponse {
		return &agentpb.GetAgentVersionResponse{Version: ver, BinarySha256: hash}
	}
	tests := []struct {
		name        string
		resp        *agentpb.GetAgentVersionResponse
		uploaded    string
		expected    string
		requireHash bool
		wantErr     bool
		wantSubstr  string
	}{
		{"hash match verifies", resp("2026.07.01-223311", "aabb01"), "aabb01", "2026.07.01-223311", false, false, ""},
		{"hash match proves a dev-over-dev push", resp("dev", "cafe02"), "cafe02", "", true, false, ""},
		{"hash match is definitive even against an older-looking version", resp("2026.06.30-120000", "cafe03"), "cafe03", "2026.07.01-223311", true, false, ""},
		{"hash comparison ignores case", resp("dev", "CAFE04"), "cafe04", "", true, false, ""},
		{"hash mismatch fails even when versions agree", resp("2026.07.01-223311", "aaaa05"), "bbbb06", "2026.07.01-223311", true, true, "not running the binary"},
		{"no reported hash falls back to version pass", resp("2026.07.01-223311", ""), "aabb07", "2026.07.01-223311", false, false, ""},
		{"required hash rejects the still-running old mac agent", resp("dev", ""), "aabb07", "", true, true, "restart was not verified"},
		{"no reported hash falls back to version fail", resp("2026.06.30-120000", ""), "aabb08", "2026.07.01-223311", false, true, "2026.06.30-120000"},
		{"no uploaded hash falls back to version", resp("2026.07.01-223311", "aabb09"), "", "2026.07.01-223311", false, false, ""},
		{"dev reported against a release expectation fails", resp("dev", ""), "", "2026.07.01-223311", false, true, "dev"},
		{"nothing to compare accepts a reachable agent", resp("dev", ""), "", "", false, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := evaluateAgentUpdateOutcome(tc.resp, tc.uploaded, tc.expected, tc.requireHash)
			if (err != nil) != tc.wantErr {
				t.Fatalf("evaluateAgentUpdateOutcome() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q should contain %q", err, tc.wantSubstr)
			}
		})
	}
}

// fakeVersionAgentClient scripts GetAgentVersion responses per call via the
// embedded-nil trick (see lifecycleFakeAgentClient in service_lifecycle_test.go).
type fakeVersionAgentClient struct {
	agentpb.WendyAgentServiceClient
	calls   int
	respond func(call int) (*agentpb.GetAgentVersionResponse, error)
}

func (c *fakeVersionAgentClient) GetAgentVersion(_ context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	c.calls++
	return c.respond(c.calls)
}

func TestVerifyAgentAfterUpdate(t *testing.T) {
	okResp := &agentpb.GetAgentVersionResponse{Version: "2026.07.01-223311", BinarySha256: "aabb"}
	fastOpts := agentVerifyWaitOptions{Timeout: 2 * time.Second, PollInterval: 10 * time.Millisecond}

	t.Run("returns the reported version and hash proof on first-try success", func(t *testing.T) {
		client := &fakeVersionAgentClient{respond: func(int) (*agentpb.GetAgentVersionResponse, error) {
			return okResp, nil
		}}
		res, err := verifyAgentAfterUpdate(context.Background(), client, "aabb", "2026.07.01-223311", fastOpts)
		if err != nil {
			t.Fatalf("verifyAgentAfterUpdate() error = %v", err)
		}
		if res.Version != "2026.07.01-223311" {
			t.Fatalf("version = %q, want %q", res.Version, "2026.07.01-223311")
		}
		if !res.HashVerified {
			t.Fatal("HashVerified = false, want true (hashes were compared and matched)")
		}
		if client.calls != 1 {
			t.Fatalf("calls = %d, want 1", client.calls)
		}
	})

	t.Run("a hash-less agent passes the version fallback without claiming hash proof", func(t *testing.T) {
		client := &fakeVersionAgentClient{respond: func(int) (*agentpb.GetAgentVersionResponse, error) {
			return &agentpb.GetAgentVersionResponse{Version: "2026.07.01-223311"}, nil
		}}
		res, err := verifyAgentAfterUpdate(context.Background(), client, "aabb", "2026.07.01-223311", fastOpts)
		if err != nil {
			t.Fatalf("verifyAgentAfterUpdate() error = %v", err)
		}
		if res.HashVerified {
			t.Fatal("HashVerified = true, want false (the agent reported no hash — nothing was proven)")
		}
	})

	t.Run("retries a transient RPC error", func(t *testing.T) {
		client := &fakeVersionAgentClient{respond: func(call int) (*agentpb.GetAgentVersionResponse, error) {
			if call == 1 {
				return nil, status.Error(codes.Unavailable, "still starting")
			}
			return okResp, nil
		}}
		res, err := verifyAgentAfterUpdate(context.Background(), client, "aabb", "", fastOpts)
		if err != nil {
			t.Fatalf("verifyAgentAfterUpdate() error = %v", err)
		}
		if res.Version != "2026.07.01-223311" {
			t.Fatalf("version = %q, want %q", res.Version, "2026.07.01-223311")
		}
		if client.calls != 2 {
			t.Fatalf("calls = %d, want 2", client.calls)
		}
	})

	t.Run("gives up after the window on persistent RPC errors", func(t *testing.T) {
		client := &fakeVersionAgentClient{respond: func(int) (*agentpb.GetAgentVersionResponse, error) {
			return nil, status.Error(codes.Unavailable, "never came up")
		}}
		_, err := verifyAgentAfterUpdate(context.Background(), client, "aabb", "",
			agentVerifyWaitOptions{Timeout: 100 * time.Millisecond, PollInterval: 20 * time.Millisecond})
		if err == nil {
			t.Fatal("verifyAgentAfterUpdate() succeeded, want error")
		}
		if !strings.Contains(err.Error(), "could not verify") {
			t.Fatalf("error %q should explain verification failed", err)
		}
	})

	t.Run("a hash mismatch is re-polled until the new agent answers", func(t *testing.T) {
		// The first reconnect can land on the still-alive OLD agent (it only
		// exits ~500ms after committing, longer on darwin), whose cached hash
		// is the old binary's — an early mismatch must not be terminal.
		client := &fakeVersionAgentClient{respond: func(call int) (*agentpb.GetAgentVersionResponse, error) {
			if call < 3 {
				return &agentpb.GetAgentVersionResponse{Version: "2026.06.30-120000", BinarySha256: "oldhash"}, nil
			}
			return okResp, nil
		}}
		res, err := verifyAgentAfterUpdate(context.Background(), client, "aabb", "", fastOpts)
		if err != nil {
			t.Fatalf("verifyAgentAfterUpdate() error = %v", err)
		}
		if !res.HashVerified {
			t.Fatal("HashVerified = false, want true once the new agent answered")
		}
		if client.calls != 3 {
			t.Fatalf("calls = %d, want 3", client.calls)
		}
	})

	t.Run("a persistent hash mismatch fails with the verdict after the window", func(t *testing.T) {
		client := &fakeVersionAgentClient{respond: func(int) (*agentpb.GetAgentVersionResponse, error) {
			return &agentpb.GetAgentVersionResponse{Version: "2026.07.01-223311", BinarySha256: "not-what-we-sent"}, nil
		}}
		_, err := verifyAgentAfterUpdate(context.Background(), client, "aabb", "",
			agentVerifyWaitOptions{Timeout: 100 * time.Millisecond, PollInterval: 20 * time.Millisecond})
		if err == nil {
			t.Fatal("verifyAgentAfterUpdate() succeeded, want hash-mismatch error")
		}
		if !strings.Contains(err.Error(), "not running the binary") {
			t.Fatalf("error %q should carry the hash-mismatch verdict, not an RPC failure", err)
		}
		if client.calls < 2 {
			t.Fatalf("calls = %d, want >= 2 (mismatch must be re-polled within the window)", client.calls)
		}
	})
}

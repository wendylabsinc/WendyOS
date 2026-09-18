package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeDeviceCachePruneClient struct {
	response *agentpbv2.PruneCacheResponse
	gotReq   *agentpbv2.PruneCacheRequest
	err      error
}

func (f *fakeDeviceCachePruneClient) PruneCache(_ context.Context, req *agentpbv2.PruneCacheRequest, _ ...grpc.CallOption) (*agentpbv2.PruneCacheResponse, error) {
	f.gotReq = req
	return f.response, f.err
}

func TestRunDeviceCachePruneRPCExplainsOldAgent(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{err: status.Error(codes.Unimplemented, "unknown method")}
	err := runDeviceCachePruneRPC(context.Background(), fake, &bytes.Buffer{}, devicePruneOptions{})
	if err == nil || !strings.Contains(err.Error(), "wendy device update") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunDeviceCachePruneRPCDryRun(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{
		ContentBlobs: 2, ContentBytes: 1_000, Snapshots: 3, SnapshotBytes: 2_000, MinimumAgeSeconds: 3600,
	}}
	var out bytes.Buffer
	if err := runDeviceCachePruneRPC(context.Background(), fake, &out, devicePruneOptions{dryRun: true}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	if !fake.gotReq.GetDryRun() || !strings.Contains(out.String(), "Eligible: 3.0 kB") {
		t.Fatalf("output = %q, dryRun=%v", out.String(), fake.gotReq.GetDryRun())
	}
}

func TestRunDeviceCachePruneRPCJSON(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{ContentBlobs: 1, MinimumAgeSeconds: 3600}}
	var out bytes.Buffer
	if err := runDeviceCachePruneRPC(context.Background(), fake, &out, devicePruneOptions{jsonOut: true}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if got["contentBlobs"] != float64(1) || got["minimumAgeSeconds"] != float64(3600) {
		t.Fatalf("JSON = %v", got)
	}
}

func TestRunDeviceCachePruneRPCAllSendsZeroMinAge(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{MinimumAgeSeconds: 0}}
	zero := time.Duration(0)
	if err := runDeviceCachePruneRPC(context.Background(), fake, &bytes.Buffer{}, devicePruneOptions{minAge: &zero}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	if fake.gotReq.MinAgeSeconds == nil || *fake.gotReq.MinAgeSeconds != 0 {
		t.Fatalf("MinAgeSeconds = %v, want 0", fake.gotReq.MinAgeSeconds)
	}
}

func TestRunDeviceCachePruneRPCMinAgeSendsSeconds(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{MinimumAgeSeconds: 3600}}
	oneHour := time.Hour
	if err := runDeviceCachePruneRPC(context.Background(), fake, &bytes.Buffer{}, devicePruneOptions{minAge: &oneHour}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	if fake.gotReq.MinAgeSeconds == nil || *fake.gotReq.MinAgeSeconds != 3600 {
		t.Fatalf("MinAgeSeconds = %v, want 3600", fake.gotReq.MinAgeSeconds)
	}
}

func TestRunDeviceCachePruneRPCDefaultSendsNoMinAge(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{MinimumAgeSeconds: 86400}}
	if err := runDeviceCachePruneRPC(context.Background(), fake, &bytes.Buffer{}, devicePruneOptions{}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	if fake.gotReq.MinAgeSeconds != nil {
		t.Fatalf("MinAgeSeconds = %v, want nil", fake.gotReq.MinAgeSeconds)
	}
}

func TestRunDeviceCachePruneRPCWarnsWhenAgentIgnoresMinAge(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{MinimumAgeSeconds: 86400}}
	oneHour := time.Hour
	var out bytes.Buffer
	if err := runDeviceCachePruneRPC(context.Background(), fake, &out, devicePruneOptions{minAge: &oneHour}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	const want = "Warning: this device agent ignored --min-age/--all and pruned with its default of 24h; update it with 'wendy device update'."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want warning %q", out.String(), want)
	}
}

func TestRunDeviceCachePruneRPCReportsReclaimedBytes(t *testing.T) {
	reclaimed := uint64(5_000)
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{
		ContentBlobs: 1, ContentBytes: 1_000, MinimumAgeSeconds: 86400, ReclaimedBytes: &reclaimed,
	}}
	var out bytes.Buffer
	if err := runDeviceCachePruneRPC(context.Background(), fake, &out, devicePruneOptions{}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	const want = "Containerd reclaimed 5.0 kB on the container storage filesystem."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	if strings.Contains(out.String(), "Containerd will reclaim unreachable data in the background") {
		t.Fatalf("output = %q, should not contain the background-reclaim line", out.String())
	}
}

func TestRunDeviceCachePruneRPCZeroAgeWording(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{MinimumAgeSeconds: 0}}
	zero := time.Duration(0)
	var out bytes.Buffer
	if err := runDeviceCachePruneRPC(context.Background(), fake, &out, devicePruneOptions{minAge: &zero}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	const want = "No cache entries are eligible for pruning.\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestParsePruneMinAgeRejectsNegative(t *testing.T) {
	if _, err := parsePruneMinAge(false, "-1h"); err == nil {
		t.Fatal("expected error for negative --min-age")
	}
}

func TestRunDeviceCachePruneRPCJSONIncludesReclaimed(t *testing.T) {
	reclaimed := uint64(2_000)
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{
		ContentBlobs: 1, MinimumAgeSeconds: 3600, ReclaimedBytes: &reclaimed,
	}}
	oneHour := time.Hour
	var out bytes.Buffer
	if err := runDeviceCachePruneRPC(context.Background(), fake, &out, devicePruneOptions{minAge: &oneHour, jsonOut: true}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if got["reclaimedBytes"] != float64(2_000) {
		t.Fatalf("reclaimedBytes = %v, want 2000", got["reclaimedBytes"])
	}
	if got["minAgeSeconds"] != float64(3600) {
		t.Fatalf("minAgeSeconds = %v, want 3600", got["minAgeSeconds"])
	}
}

func TestRunDeviceCachePruneRPCJSONNullsWhenAbsent(t *testing.T) {
	fake := &fakeDeviceCachePruneClient{response: &agentpbv2.PruneCacheResponse{ContentBlobs: 1, MinimumAgeSeconds: 86400}}
	var out bytes.Buffer
	if err := runDeviceCachePruneRPC(context.Background(), fake, &out, devicePruneOptions{jsonOut: true}); err != nil {
		t.Fatalf("runDeviceCachePruneRPC: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if got["minAgeSeconds"] != nil {
		t.Fatalf("minAgeSeconds = %v, want null", got["minAgeSeconds"])
	}
	if got["reclaimedBytes"] != nil {
		t.Fatalf("reclaimedBytes = %v, want null", got["reclaimedBytes"])
	}
}

func TestDeviceCachePruneAllAndMinAgeMutuallyExclusive(t *testing.T) {
	cmd := newDeviceCachePruneCmd()
	cmd.SetArgs([]string{"--all", "--min-age", "1h"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for --all combined with --min-age")
	}
}

func TestDeviceCachePruneMinAgeFlagRejectsNegativeBeforeRPC(t *testing.T) {
	cmd := newDeviceCachePruneCmd()
	cmd.SetArgs([]string{"--min-age", "-1h"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative --min-age")
	}
	if strings.Contains(err.Error(), "connect") {
		t.Fatalf("negative --min-age should be rejected before attempting to connect: %v", err)
	}
}

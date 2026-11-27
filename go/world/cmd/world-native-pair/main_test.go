package main

import (
	"bytes"
	"context"
	"errors"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestWorldNativePairConfiguredFixtureSocket(t *testing.T) {
	target, err := adminTarget("/var/lib/wendy/agent-control/agent.sock")
	if err != nil || target != "unix:///var/lib/wendy/agent-control/agent.sock" {
		t.Fatalf("wrong configured target %q: %v", target, err)
	}
	if target, err := adminTarget("/other/configured/agent.sock"); err != nil || target != "unix:///other/configured/agent.sock" {
		t.Fatal("explicit alternate Unix socket was not preserved")
	}
	for _, socket := range []string{"", "relative/agent.sock", "127.0.0.1:50051", "unix:///var/lib/wendy/agent-control/agent.sock"} {
		if target, err := adminTarget(socket); target != "" || status.Code(err) != codes.InvalidArgument {
			t.Fatalf("unexpected endpoint accepted %q", socket)
		}
	}
}

func TestWorldNativePairReportsOnlyBoundedFailureCode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{{status.Error(codes.Unavailable, "private socket details"), "Unavailable"},
		{status.Error(codes.PermissionDenied, "private policy details"), "PermissionDenied"},
		{errors.New("private local details"), "Unknown"}} {
		var out bytes.Buffer
		writeFailure(&out, "command", tc.err)
		if out.String() != "native pairing command failed: "+tc.code+"\n" {
			t.Fatal("failure details leaked or classification lost")
		}
	}
}

type pairingClient struct {
	agentpbv2.WendySensorPairingServiceClient
	request *agentpbv2.AddSensorPairingRequest
	lists   int
}

func (p *pairingClient) AddSensorPairing(_ context.Context, r *agentpbv2.AddSensorPairingRequest, _ ...grpc.CallOption) (*agentpbv2.AddSensorPairingResponse, error) {
	p.request = r
	return &agentpbv2.AddSensorPairingResponse{Pairing: &agentpbv2.SensorPairing{SourcePrincipalUri: r.SourcePrincipalUri, Transport: r.Transport, SensorAllowlist: r.SensorAllowlist}}, nil
}
func (p *pairingClient) ListSensorPairings(context.Context, *agentpbv2.ListSensorPairingsRequest, ...grpc.CallOption) (*agentpbv2.ListSensorPairingsResponse, error) {
	p.lists++
	return &agentpbv2.ListSensorPairingsResponse{}, nil
}
func TestWorldNativePairUsesExistingExactRPC(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/device/gateway-a"
	var out bytes.Buffer
	p := &pairingClient{}
	if err := run(context.Background(), []string{"pair", "--principal", principal, "--address", "127.0.0.1:50060"}, &out, p); err != nil {
		t.Fatal(err)
	}
	r := p.request
	if r.SourcePrincipalUri != principal || r.SourceAssetId != 0 || r.Transport != "tcp" || len(r.SensorAllowlist) != 1 || r.SensorAllowlist[0] != "camera-b" {
		t.Fatalf("wrong request %+v", r)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"source_principal_uri"`)) {
		t.Fatal("not native proto JSON")
	}
	if err := run(context.Background(), []string{"status"}, &out, p); err != nil || p.lists != 1 {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"pair", "--principal", principal, "--address", "10.0.0.1:50060"}, {"pair", "--principal", principal, "--address", "127.0.0.1:22"}, {"status", "extra"}, {"unknown"}} {
		prior := p.request
		p.request = nil
		if err := run(context.Background(), args, &out, p); err == nil || p.request != nil {
			t.Fatal("invalid command reached mutation")
		}
		p.request = prior
	}
}

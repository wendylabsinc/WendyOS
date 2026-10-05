package interceptor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func tracedDeviceContext() context.Context {
	leaf := buildLeaf(leafOptions{commonName: "secret-subject"})
	leaf.Raw = []byte("test-certificate-der")
	return metadata.NewIncomingContext(ctxWithLeaf(leaf), metadata.Pairs(
		"x-correlation-id", "11111111-1111-4111-8111-111111111111",
		"traceparent", "00-1234567890abcdef1234567890abcdef-1234567890abcdef-01",
		"authorization", "secret-authorization", "baggage", "secret-baggage", "x-user-id", "forged-user"))
}

func TestDeviceWitnessUsesCertificateAndNeverLogsPayload(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	intercept := UnaryMTLSInterceptor(zap.New(core), certs.Scope{}, OrgModeOff)
	_, err := intercept(tracedDeviceContext(), "secret-request-body", &grpc.UnaryServerInfo{FullMethod: "/test.Service/Call"}, func(context.Context, any) (any, error) {
		return nil, status.Error(codes.PermissionDenied, "secret-device-error")
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	events := logs.FilterMessage("traced device RPC").All()
	if len(events) != 2 {
		t.Fatal("missing device witness", len(events))
	}
	fields := events[1].ContextMap()
	if fields["grpc_status"] != "PermissionDenied" || fields["evidence"] != "device_observed" || fields["correlation_source"] != "peer_hint" {
		t.Fatal(fields)
	}
	if fields["peer_certificate_sha256"] == "" || fields["correlation_id"] != "11111111-1111-4111-8111-111111111111" {
		t.Fatal(fields)
	}
	encoded, _ := json.Marshal(fields)
	if strings.Contains(string(encoded), "secret-") || strings.Contains(string(encoded), "forged-user") {
		t.Fatal("secret or asserted identity in audit")
	}
}

func TestDeviceWitnessRejectsForgedOrAmbiguousContext(t *testing.T) {
	for _, scenario := range []string{"no-peer", "bad-id", "duplicate-id", "bad-trace"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := tracedDeviceContext()
			md, _ := metadata.FromIncomingContext(ctx)
			switch scenario {
			case "no-peer":
				ctx = context.Background()
			case "bad-id":
				md.Set("x-correlation-id", "secret-injection")
			case "duplicate-id":
				md.Append("x-correlation-id", "22222222-2222-4222-8222-222222222222")
			case "bad-trace":
				md.Set("traceparent", "secret-injection")
			}
			core, logs := observer.New(zap.InfoLevel)
			finish := auditTracedRPC(metadata.NewIncomingContext(ctx, md), zap.New(core), "/test.Service/Call")
			finish(nil)
			if logs.Len() != 0 {
				t.Fatal("unverified hint was logged")
			}
		})
	}
}

type auditStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s auditStream) Context() context.Context { return s.ctx }

func TestStreamingDeviceCallRecordsFinalStatus(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	intercept := StreamMTLSInterceptor(zap.New(core), certs.Scope{}, OrgModeOff)
	err := intercept(nil, auditStream{ctx: tracedDeviceContext()}, &grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"}, func(any, grpc.ServerStream) error { return status.Error(codes.Canceled, "secret-cancel") })
	if status.Code(err) != codes.Canceled || logs.All()[1].ContextMap()["grpc_status"] != "Canceled" {
		t.Fatal("lost stream outcome")
	}
}

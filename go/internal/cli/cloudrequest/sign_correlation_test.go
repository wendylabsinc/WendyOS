package cloudrequest

import (
	"context"
	"strings"
	"testing"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestInvokeAdoptsExistingFlowCorrelationAcrossCallsAndRetries(t *testing.T) {
	auth, _, _ := testAuth(t)
	const flow = "upstream.flow_123-AbC"
	md := metadata.Pairs("x-correlation-id", "", "x-correlation-id", flow)
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	conn := &fakeCloud{t: t, refuse: []error{status.Error(codes.PermissionDenied, "kid refused")}}
	for range 2 {
		if err := Invoke(ctx, conn, auth, cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1"}, &cloudpbv2.EnrollDeviceResponse{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(conn.correlation) != 1 || !conn.correlation[flow] {
		t.Fatalf("supplied flow replaced: %v", conn.correlation)
	}
	if values := md.Get("x-correlation-id"); len(values) != 2 || values[1] != flow {
		t.Fatal("caller metadata mutated")
	}
}

func TestInvokeRejectsUnsafeSuppliedCorrelationBeforeAnyRPC(t *testing.T) {
	for _, value := range []string{"space value", "line\nbreak", "ünicode", strings.Repeat("a", 65)} {
		t.Run(value, func(t *testing.T) {
			auth, _, _ := testAuth(t)
			conn := &fakeCloud{t: t}
			ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-correlation-id", value))
			err := Invoke(ctx, conn, auth, cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1"}, &cloudpbv2.EnrollDeviceResponse{})
			if err == nil || !strings.Contains(err.Error(), "invalid supplied Cloud correlation id") {
				t.Fatalf("unsafe id accepted: %v", err)
			}
			if len(conn.calls) != 0 {
				t.Fatal("unsafe id caused RPC")
			}
		})
	}
}

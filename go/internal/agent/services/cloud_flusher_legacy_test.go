package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

type legacyOTLPContract struct {
	collogspb.UnimplementedLogsServiceServer
	requests chan int
}

func (s legacyOTLPContract) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.requests <- len(req.ResourceLogs)
	return &collogspb.ExportLogsServiceResponse{}, nil
}
func TestLegacyTelemetryDefaultsToEnrollmentAndSignsExport(t *testing.T) {
	t.Setenv("WENDY_TELEMETRY_URL", "")
	ca, key, chain := brokerTestCA(t)
	cert := brokerTestServer(t, ca, key, "cloud", nil, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if info.FullMethod != "/opentelemetry.proto.collector.logs.v1.LogsService/Export" || len(md.Get("x-wendy-certificate-signature")) != 1 {
			return nil, fmt.Errorf("missing Export proof")
		}
		if got := md.Get("x-wendy-certificate-uri"); len(got) != 1 || got[0] != "urn:wendy:org:64:asset:674" {
			return nil, fmt.Errorf("missing numeric identity")
		}
		return next(ctx, req)
	}))
	contract := legacyOTLPContract{requests: make(chan int, 1)}
	collogspb.RegisterLogsServiceServer(server, contract)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	leaf, private := legacyBrokerTestLeaf(t, true)
	provisioning := &ProvisioningService{enrolled: true, cloudHost: listener.Addr().String(), orgID: 64, assetID: 674, certPEM: leaf, chainPEM: chain, keyPEM: []byte(private)}
	buffer, _ := makeTestBuffer(t, 10<<20, 1<<20)
	buffer.PublishLogs(makeLogReq("one"))
	buffer.PublishLogs(makeLogReq("two"))
	f := NewCloudFlusherWithProvisioning(zap.NewNop(), buffer, provisioning)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	select {
	case frames := <-contract.requests:
		if frames != 2 {
			t.Fatalf("frames=%d", frames)
		}
	case <-ctx.Done():
		t.Fatal("no signed legacy OTLP export")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flusher did not stop")
	}
}
func TestCloudFlusherBatchesStayBoundedAndDrain(t *testing.T) {
	buffer, _ := makeTestBuffer(t, 10<<20, 1<<20)
	// Each valid frame is below the spool's 1 MiB limit, but a merged batch of
	// these frames would exceed gRPC's default message size without a byte cap.
	for range 7 {
		buffer.PublishLogs(makeLogReq(strings.Repeat("x", 700<<10)))
	}
	f := NewCloudFlusher(zap.NewNop(), buffer, 64, 674)
	frames, calls := 0, 0
	for range 10 {
		err := f.flushSignal(SignalLogs, func(msg proto.Message) error {
			if proto.Size(msg) > cloudFlusherMaxBatchBytes {
				t.Fatalf("oversize export: %d", proto.Size(msg))
			}
			frames += len(msg.(*collogspb.ExportLogsServiceRequest).ResourceLogs)
			calls++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if frames != 7 || calls < 2 {
		t.Fatalf("drain frames=%d calls=%d", frames, calls)
	}
}

package commands

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
)

func TestRobotGatewayCloudInventoryV2(t *testing.T) {
	for _, onlineOnly := range []bool{true, false} {
		t.Run(fmt.Sprint("online=", onlineOnly), func(t *testing.T) {
			auth := discoveryV2Auth(t, 205, false)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rows, err := gatewayCloudInventory(ctx, auth, onlineOnly)
			if err != nil || len(rows) != 205 {
				t.Fatalf("inventory: %d rows, %v", len(rows), err)
			}
			name := "offline-device"
			if onlineOnly {
				name = "online-device"
			}
			for i, row := range rows {
				want := fmt.Sprintf("cloud://%s/tenant/%s/asset/00000000-0000-4000-8000-%012d", auth.CloudGRPC, testOperatorTenant, i+1)
				if row.Device != want || row.Name != name || row.Type != "macOS (arm64)" {
					t.Fatalf("incorrect inventory identity: %+v", row)
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := gatewayCloudInventory(ctx, discoveryV2Auth(t, 2, true), true)
	if err == nil || !strings.Contains(err.Error(), "membership required") || len(rows) != 0 {
		t.Fatalf("stream failure returned partial inventory: %+v, %v", rows, err)
	}
}

func TestRobotGatewayCloudInventoryLegacy(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterAssetServiceServer(srv, &discoveryV1Server{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	auth := fakeAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := gatewayCloudInventory(ctx, auth, true)
	want := "cloud://" + auth.CloudGRPC + "/org/7/asset/77"
	if err != nil || len(rows) != 1 || rows[0].Device != want {
		t.Fatalf("legacy inventory: %+v, %v", rows, err)
	}
}

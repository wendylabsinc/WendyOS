package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Exercise the actual CLI list command against the updated Agent with the
// unchanged default gRPC receive limit. Returning unrequested file metadata
// would turn this previously-small response into a message larger than 4 MiB.
func TestDataEpisodesLargeLibraryKeepsCompactDefault(t *testing.T) {
	root := t.TempDir()
	manager, err := data.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	var files []data.File
	for i := 0; i < 12; i++ {
		files = append(files, data.File{Path: fmt.Sprintf("cameras/by-id_usb-Camera_Camera_EA9667892-video-index%d/recording.mp4", i), MediaType: "video/mp4", Size: 1024})
	}
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("20261117T222547Z-%016x", i)
		dir := filepath.Join(root, id)
		if err = os.Mkdir(dir, 0750); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(data.Manifest{ID: id, Name: "campaign", State: "complete", StartedUnixNanos: int64(i + 1), Files: files})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	pb.RegisterDataServiceServer(server, services.NewDataService(manager))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient("passthrough:///cli-list-compat", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out, errOut bytes.Buffer
	if err = runDataEpisodes(ctx, pb.NewDataServiceClient(conn), &out, &errOut, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	var listed pb.DataEpisodesResponse
	if err = protojson.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Episodes) != 10000 {
		t.Fatalf("lost episodes: %d", len(listed.Episodes))
	}
	for i, row := range listed.Episodes {
		if row.Media != nil {
			t.Fatal("CLI unexpectedly opted into media")
		}
		if row.StartedUnixNanos != int64(10000-i) {
			t.Fatalf("lost order at %d", i)
		}
	}
	if size := proto.Size(&listed); size >= 4<<20 {
		t.Fatalf("compact response exceeds default budget: %d", size)
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected warning: %s", errOut.String())
	}
	t.Logf("CLI successfully listed %d episodes; protobuf=%d bytes", len(listed.Episodes), proto.Size(&listed))
}

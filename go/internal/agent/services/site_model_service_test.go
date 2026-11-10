package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func siteModelTestClient(t *testing.T) pb.SiteModelServiceClient {
	t.Helper()
	return siteModelTestClientAt(t, t.TempDir())
}
func siteModelTestClientAt(t *testing.T, root string) pb.SiteModelServiceClient {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "site-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterSiteModelServiceServer(server, NewSiteModelService(root))
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewSiteModelServiceClient(conn)
}
func TestSiteModelRoundTripAndChecksum(t *testing.T) {
	client := siteModelTestClient(t)
	ctx := context.Background()
	data := append([]byte("WENDYST2"), bytes.Repeat([]byte{42}, 600000)...)
	hash := sha256.Sum256(data)
	id := hex.EncodeToString(hash[:])
	save := func(id string) error {
		stream, err := client.Upload(ctx)
		if err != nil {
			return err
		}
		if err = stream.Send(&pb.SiteModelUploadRequest{Info: &pb.SiteModelInfo{Id: id, Name: "Office", Size: int64(len(data))}}); err != nil {
			return err
		}
		for offset := 0; offset < len(data); {
			end := offset + siteModelChunkBytes
			if end > len(data) {
				end = len(data)
			}
			if err = stream.Send(&pb.SiteModelUploadRequest{Data: data[offset:end]}); err != nil {
				return err
			}
			offset = end
		}
		_, err = stream.CloseAndRecv()
		return err
	}
	if err := save(id); err != nil {
		t.Fatal(err)
	}
	if err := save(id); err != nil {
		t.Fatalf("idempotent upload: %v", err)
	}
	list, err := client.List(ctx, &pb.SiteModelListRequest{})
	if err != nil || len(list.Models) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	stream, err := client.Download(ctx, &pb.SiteModelDownloadRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, chunk.Data...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("download differs")
	}
	wrong := sha256.Sum256([]byte("wrong"))
	if err := save(hex.EncodeToString(wrong[:])); status.Code(err) != codes.DataLoss {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	list, err = client.List(ctx, &pb.SiteModelListRequest{})
	if err != nil || len(list.Models) != 1 {
		t.Fatalf("failed upload published: %v %v", list, err)
	}
}
func TestSiteModelRequiresAuthentication(t *testing.T) {
	_, err := NewSiteModelService(t.TempDir()).List(context.Background(), &pb.SiteModelListRequest{})
	if err == nil {
		t.Fatal("unauthenticated listing allowed")
	}
}
func TestSiteModelRejectsTraversalAndIncompleteUpload(t *testing.T) {
	client := siteModelTestClient(t)
	ctx := context.Background()
	stream, err := client.Download(ctx, &pb.SiteModelDownloadRequest{Id: "../../etc/passwd"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("path accepted: %v", err)
	}
	hash := sha256.Sum256([]byte("missing"))
	upload, err := client.Upload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	upload.Send(&pb.SiteModelUploadRequest{Info: &pb.SiteModelInfo{Id: hex.EncodeToString(hash[:]), Name: "Interrupted", Size: 100}})
	_, err = upload.CloseAndRecv()
	if status.Code(err) != codes.DataLoss {
		t.Fatalf("incomplete upload accepted: %v", err)
	}
	list, err := client.List(ctx, &pb.SiteModelListRequest{})
	if err != nil || len(list.Models) != 0 {
		t.Fatalf("incomplete upload visible: %v %v", list, err)
	}
}

// An upload's temporary file outlives an agent that stops mid-transfer, and
// the quota does not count it. The next upload clears what the last one left.
func TestSiteModelUploadClearsInterruptedTransfers(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".upload-123", ".index-456"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("partial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	client := siteModelTestClientAt(t, root)
	hash := sha256.Sum256([]byte("missing"))
	upload, err := client.Upload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	upload.Send(&pb.SiteModelUploadRequest{Info: &pb.SiteModelInfo{Id: hex.EncodeToString(hash[:]), Name: "Interrupted", Size: 100}})
	if _, err = upload.CloseAndRecv(); status.Code(err) != codes.DataLoss {
		t.Fatalf("incomplete upload accepted: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: %v %v", entries, err)
	}
}

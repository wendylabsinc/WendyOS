package meshappstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type applicationFixture struct {
	pb.UnimplementedWendyMeshServiceServer
	run func(pb.WendyMeshService_MeshDialServer) error
}

func (s applicationFixture) MeshDial(stream pb.WendyMeshService_MeshDialServer) error {
	return s.run(stream)
}
func applicationClient(t *testing.T, run func(pb.WendyMeshService_MeshDialServer) error) *grpc.ClientConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterWendyMeshServiceServer(server, applicationFixture{run: run})
	go server.Serve(listener)
	client, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Stop(); listener.Close() })
	return client
}
func acceptApplication(stream pb.WendyMeshService_MeshDialServer) (uint32, error) {
	open, err := stream.Recv()
	if err != nil {
		return 0, err
	}
	if err = stream.Send(&pb.MeshDialData{}); err != nil {
		return 0, err
	}
	return open.GetOpen().Port, nil
}
func receiveApplication(stream pb.WendyMeshService_MeshDialServer) ([]byte, error) {
	var out []byte
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if data := msg.GetData(); data != nil {
			out = append(out, data.Payload...)
			if data.HalfClose {
				return out, nil
			}
		}
	}
}

func TestNormalApplicationUploadHalfCloseAndSetupCancellation(t *testing.T) {
	payload := make([]byte, 128*1024)
	_, _ = rand.Read(payload)
	completed := make(chan error, 1)
	client := applicationClient(t, func(stream pb.WendyMeshService_MeshDialServer) error {
		if _, err := acceptApplication(stream); err != nil {
			return err
		}
		got, err := receiveApplication(stream)
		if err != nil {
			completed <- err
			return err
		}
		if !bytes.Equal(got, payload) {
			t.Error("upload differs")
		}
		for len(got) > 0 {
			chunk := got[:min(ChunkSize, len(got))]
			if err := stream.Send(&pb.MeshDialData{Payload: chunk}); err != nil {
				completed <- err
				return err
			}
			got = got[len(chunk):]
		}
		err = stream.Send(&pb.MeshDialData{HalfClose: true})
		completed <- err
		return err
	})
	setup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	conn, err := Dial(setup, client, 8080)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("application echo", len(got), err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}

func TestNormalApplicationPeerHalfCloseKeepsUploadAlive(t *testing.T) {
	payload := make([]byte, 64*1024)
	_, _ = rand.Read(payload)
	completed := make(chan []byte, 1)
	client := applicationClient(t, func(stream pb.WendyMeshService_MeshDialServer) error {
		if _, err := acceptApplication(stream); err != nil {
			return err
		}
		if err := stream.Send(&pb.MeshDialData{Payload: []byte("greeting"), HalfClose: true}); err != nil {
			return err
		}
		got, err := receiveApplication(stream)
		completed <- got
		return err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, client, 8081)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	greeting, err := io.ReadAll(conn)
	if err != nil || string(greeting) != "greeting" {
		t.Fatal(string(greeting), err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal("peer half-close stopped upload", err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-completed:
		if !bytes.Equal(got, payload) {
			t.Fatal("upload after read EOF differs")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestNormalApplicationPendingOpenCancellationReleasesStream(t *testing.T) {
	finished := make(chan struct{})
	client := applicationClient(t, func(stream pb.WendyMeshService_MeshDialServer) error {
		defer close(finished)
		if _, err := stream.Recv(); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if conn, err := Dial(ctx, client, 8082); err == nil {
		conn.Close()
		t.Fatal("pending open completed")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("pending application stream did not stop")
	}
}

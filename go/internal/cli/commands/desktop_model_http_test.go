package commands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func receiveModelHTTPRequest(stream pb.WendyContainerService_ExecContainerServer) (*http.Request, error) {
	first, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	start := first.GetStart()
	args := start.GetCommand()
	if start.GetAppName() != "sh.wendy.models.ollama.11437" || start.GetTty() || len(args) != 6 || args[0] != "bash" || args[2] != desktopModelHTTPCommand || args[4] != "11437" {
		return nil, fmt.Errorf("unexpected model relay command: %v", start)
	}
	count, err := strconv.Atoi(args[5])
	if err != nil {
		return nil, err
	}
	var wire strings.Builder
	for wire.Len() < count {
		frame, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		wire.Write(frame.GetStdinData())
	}
	if wire.Len() != count {
		return nil, fmt.Errorf("wrong HTTP request size")
	}
	return http.ReadRequest(bufio.NewReader(strings.NewReader(wire.String())))
}

func TestDesktopModelHTTPStreamsAndClosesExec(t *testing.T) {
	closed := make(chan struct{})
	client := newFoxgloveExecTestClient(t, func(stream pb.WendyContainerService_ExecContainerServer) error {
		defer close(closed)
		req, err := receiveModelHTTPRequest(stream)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != "{\"prompt\":\"hello 世界\"}" || !req.Close || req.URL.Path != "/api/chat" || req.Header.Get("Content-Type") != "application/json" {
			return fmt.Errorf("request changed: %+v %q %v", req, body, err)
		}
		// Header and body arrive separately; the response must be readable
		// before the server finishes, with stderr excluded from HTTP bytes.
		for _, frame := range []*pb.ExecContainerResponse{
			{ResponseType: &pb.ExecContainerResponse_StderrData{StderrData: []byte("diagnostic")}},
			{ResponseType: &pb.ExecContainerResponse_StdoutData{StdoutData: []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")}},
			{ResponseType: &pb.ExecContainerResponse_StdoutData{StdoutData: []byte("5\r\nhello\r\n")}},
		} {
			if err := stream.Send(frame); err != nil {
				return err
			}
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://wendy-device/api/chat", strings.NewReader("{\"prompt\":\"hello 世界\"}"))
	req.Header.Set("Content-Type", "application/json")
	response, err := desktopModelRoundTrip(req, &grpcclient.AgentConnection{ContainerService: client}, 11437, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, body); err != nil || string(body) != "hello" {
		t.Fatalf("stream %q: %v", body, err)
	}
	response.Body.Close()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("body close did not cancel remote exec")
	}
}

func TestDesktopModelHTTPPreservesFailures(t *testing.T) {
	client := newFoxgloveExecTestClient(t, func(stream pb.WendyContainerService_ExecContainerServer) error {
		if _, err := receiveModelHTTPRequest(stream); err != nil {
			return err
		}
		if err := stream.Send(&pb.ExecContainerResponse{ResponseType: &pb.ExecContainerResponse_StderrData{StderrData: []byte("Connection refused")}}); err != nil {
			return err
		}
		return stream.Send(&pb.ExecContainerResponse{ResponseType: &pb.ExecContainerResponse_ExitCode{ExitCode: 1}})
	})
	req, _ := http.NewRequest(http.MethodGet, "http://wendy-device/api/tags", nil)
	_, err := desktopModelRoundTrip(req, &grpcclient.AgentConnection{ContainerService: client}, 11437, time.Second)
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("lost failure: %v", err)
	}
}

func TestDesktopModelHTTPHeaderTimeout(t *testing.T) {
	closed := make(chan struct{})
	client := newFoxgloveExecTestClient(t, func(stream pb.WendyContainerService_ExecContainerServer) error {
		defer close(closed)
		if _, err := receiveModelHTTPRequest(stream); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	req, _ := http.NewRequest(http.MethodGet, "http://wendy-device/api/tags", nil)
	_, err := desktopModelRoundTrip(req, &grpcclient.AgentConnection{ContainerService: client}, 11437, 100*time.Millisecond)
	if err == nil {
		t.Fatal("missing header timeout")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("header timeout left exec running")
	}
}

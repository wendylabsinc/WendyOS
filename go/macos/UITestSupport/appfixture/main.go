// Simulator-only fixture for the Devices lens app lifecycle UI test.
// All listeners and the DNS-SD registration are restricted to the local host.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	collector "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const webApp = "people-counter"

type deviceService struct {
	pb.UnimplementedWendyAgentServiceServer
}

func (deviceService) GetAgentVersion(context.Context, *pb.GetAgentVersionRequest) (*pb.GetAgentVersionResponse, error) {
	model := "QA gateway"
	return &pb.GetAgentVersionResponse{Version: "2026.11.04-081952", Os: "linux", CpuArchitecture: "arm64", DeviceType: &model}, nil
}

type containers struct {
	pb.UnimplementedWendyContainerServiceServer
	running atomic.Bool
	port    uint32
}

func (c *containers) ListContainers(_ *pb.ListContainersRequest, stream grpc.ServerStreamingServer[pb.ListContainersResponse]) error {
	state := pb.AppRunningState_STOPPED
	if c.running.Load() {
		state = pb.AppRunningState_RUNNING
	}
	version := "1.3.0"
	for _, app := range []*pb.AppContainer{
		{AppName: webApp, AppVersion: version, HttpPort: c.port, RunningState: state},
		{AppName: "ros2-recorder", RunningState: pb.AppRunningState_STOPPED},
	} {
		if err := stream.Send(&pb.ListContainersResponse{Container: app}); err != nil {
			return err
		}
	}
	return nil
}

func (c *containers) StartContainer(req *pb.StartContainerRequest, stream grpc.ServerStreamingServer[pb.RunContainerLayersResponse]) error {
	if req.GetAppName() != webApp {
		return status.Error(codes.NotFound, "unknown test app")
	}
	c.running.Store(true)
	log.Print("started ", webApp)
	return stream.Send(&pb.RunContainerLayersResponse{ResponseType: &pb.RunContainerLayersResponse_Started_{Started: &pb.RunContainerLayersResponse_Started{}}})
}

func (c *containers) StopContainer(_ context.Context, req *pb.StopContainerRequest) (*pb.StopContainerResponse, error) {
	if req.GetAppName() != webApp {
		return nil, status.Error(codes.NotFound, "unknown test app")
	}
	c.running.Store(false)
	log.Print("stopped ", webApp)
	return &pb.StopContainerResponse{}, nil
}

func (c *containers) GetContainerPorts(_ context.Context, req *pb.GetContainerPortsRequest) (*pb.GetContainerPortsResponse, error) {
	if req.GetAppName() == webApp && c.running.Load() {
		return &pb.GetContainerPortsResponse{Ports: []*pb.PortEntry{{Port: c.port, Protocol: "tcp", Address: "127.0.0.1"}}}, nil
	}
	return &pb.GetContainerPortsResponse{}, nil
}

type telemetry struct {
	pb.UnimplementedWendyTelemetryServiceServer
	generation int
}

func (t telemetry) StreamLogs(req *pb.StreamLogsRequest, stream grpc.ServerStreamingServer[pb.StreamLogsResponse]) error {
	app := req.GetAppName()
	if app == "" {
		app = "device"
	}
	log.Printf("subscribed to %s logs", app)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		record := &logs.LogRecord{TimeUnixNano: uint64(time.Now().UnixNano()), SeverityText: "INFO",
			Body:       &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: fmt.Sprintf("%s log record (boot %d)", app, t.generation)}},
			Attributes: []*common.KeyValue{{Key: "wendy.app.name", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: app}}}}}
		if err := stream.Send(&pb.StreamLogsResponse{Logs: &collector.ExportLogsServiceRequest{
			ResourceLogs: []*logs.ResourceLogs{{ScopeLogs: []*logs.ScopeLogs{{LogRecords: []*logs.LogRecord{record}}}}}}}); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
		}
	}
}

// Keep the advertised address fixed across outages, as it would be when a board returns.
type fixtureDevice struct {
	mu         sync.Mutex
	address    string
	server     *grpc.Server
	apps       *containers
	generation int
}

func (d *fixtureDevice) setOnline(online bool) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if online && d.server == nil {
		listener, err := net.Listen("tcp4", d.address)
		if err != nil {
			return d.generation, err
		}
		d.address = listener.Addr().String()
		d.generation++
		server := grpc.NewServer()
		pb.RegisterWendyContainerServiceServer(server, d.apps)
		pb.RegisterWendyAgentServiceServer(server, deviceService{})
		pb.RegisterWendyTelemetryServiceServer(server, telemetry{generation: d.generation})
		d.server = server
		go server.Serve(listener)
	} else if !online && d.server != nil {
		d.server.Stop()
		d.server = nil
	}
	return d.generation, nil
}

func main() {
	recovery := flag.Bool("recovery", false, "enable the loopback controls for the connection recovery test")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	web, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer web.Close()
	apps := &containers{port: uint32(web.Addr().(*net.TCPAddr).Port)}
	device := fixtureDevice{apps: apps, address: "127.0.0.1:0"}
	if _, err := device.setOnline(true); err != nil {
		log.Fatal(err)
	}
	defer device.setOnline(false)
	_, port, err := net.SplitHostPort(device.address)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	if *recovery {
		log.Printf("Recovery control URL: http://%s/recovery", web.Addr())
		for _, state := range []string{"online", "offline"} {
			mux.HandleFunc("POST /recovery/"+state, func(w http.ResponseWriter, r *http.Request) {
				generation, err := device.setOnline(state == "online")
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				log.Printf("device %s (boot %d)", state, generation)
				fmt.Fprintln(w, generation)
			})
		}
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		device.mu.Lock()
		online := device.server != nil
		device.mu.Unlock()
		if !online {
			http.Error(w, "Device offline", http.StatusServiceUnavailable)
			return
		}
		if !apps.running.Load() {
			http.Error(w, "App stopped", http.StatusServiceUnavailable)
			return
		}
		log.Print("served dashboard ", r.URL.Path)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><title>People counter</title><h1>Fixture dashboard online</h1></html>")
	})
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: mux}
	go srv.Serve(web)
	defer srv.Close()
	cmd := exec.CommandContext(ctx, "dns-sd", "-lo", "-P", "Devices UI QA", "_wendyos._udp", "local", port,
		"wendy-apps-qa.local", "127.0.0.1", "displayname=Devices UI QA", "id=wendy-apps-qa", "os=linux", "tls=false")
	log.Print("advertising Devices UI QA on loopback")
	if err = cmd.Run(); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

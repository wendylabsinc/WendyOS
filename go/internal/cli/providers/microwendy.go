package providers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const (
	// microWendyUDPPort is the port ESP32 devices listen on for WENDY_RELOAD messages.
	microWendyUDPPort = 4210

	// microWendyServiceType is the mDNS service type advertised by ESP32 Wendy devices.
	microWendyServiceType = "_wendy._tcp"
)

// microWendyBuildContext is stored in BuiltApp.Context for WASM builds.
type microWendyBuildContext struct {
	WASMPath  string
	TargetIPs []string // IPs of discovered ESP32 devices for unicast
	cancel    context.CancelFunc
}

// MicroWendyProvider builds Swift packages to WASM and serves them to ESP32 devices.
type MicroWendyProvider struct{}

func (p *MicroWendyProvider) Key() string         { return "wendy-lite" }
func (p *MicroWendyProvider) DisplayName() string { return "Micro Wendy (WASM)" }

func (p *MicroWendyProvider) IsAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "swiftly", "--version")
	return cmd.Run() == nil
}

func (p *MicroWendyProvider) CheckRequirements(ctx context.Context) error {
	if !p.IsAvailable(ctx) {
		return fmt.Errorf("swiftly is not installed or not in PATH")
	}
	return nil
}

func (p *MicroWendyProvider) DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error) {
	services, err := discovery.BrowseMDNSServices(ctx, microWendyServiceType, 3*time.Second)
	if err != nil {
		return nil, err
	}

	var devices []models.ExternalDevice
	for _, svc := range services {
		displayName := svc.InstanceName
		if displayName == "" {
			displayName = svc.Hostname
		}

		devices = append(devices, models.ExternalDevice{
			ID:          fmt.Sprintf("wendy-lite:%s", svc.Hostname),
			DisplayName: displayName,
			ProviderKey: p.Key(),
			ConnectionInfo: map[string]string{
				"hostname": svc.Hostname,
				"ip":       svc.IPAddress,
				"port":     fmt.Sprintf("%d", svc.Port),
			},
			IsWendyDevice:   true,
			CPUArchitecture: "wasm32",
		})
	}

	return devices, nil
}

func (p *MicroWendyProvider) SupportedBuildTypes() []string {
	return []string{"swift"}
}

func (p *MicroWendyProvider) CanBuild(projectPath string) bool {
	_, err := os.Stat(filepath.Join(projectPath, "Package.swift"))
	return err == nil
}

func (p *MicroWendyProvider) Build(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	if err := swifttoolchain.EnsureSwiftVersion(ctx, os.Stdout, os.Stderr); err != nil {
		return nil, err
	}

	args := []string{"build", "--triple", swifttoolchain.WasmTargetTriple}
	if !debug {
		args = append(args, "-c", "release")
	}
	cmd := swifttoolchain.SwiftCommandContext(ctx, args...)
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("swift build (wasm): %w", err)
	}

	config := "debug"
	if !debug {
		config = "release"
	}
	wasmPath := filepath.Join(projectPath, ".build", swifttoolchain.WasmTargetTriple, config, product+".wasm")

	// Collect IPs of all known devices for unicast delivery.
	var targetIPs []string
	if ip := device.ConnectionInfo["ip"]; ip != "" {
		targetIPs = append(targetIPs, ip)
	}

	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context:     &microWendyBuildContext{WASMPath: wasmPath, TargetIPs: targetIPs},
	}, nil
}

func (p *MicroWendyProvider) Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error {
	defer close(output)

	bc, ok := app.Context.(*microWendyBuildContext)
	if !ok {
		return fmt.Errorf("wendy-lite provider: invalid build context")
	}

	serveCtx, cancel := context.WithCancel(ctx)
	bc.cancel = cancel

	// Serve the .wasm file as /app.wasm over HTTP on a dynamic port.
	wasmData, err := os.ReadFile(bc.WASMPath)
	if err != nil {
		cancel()
		return fmt.Errorf("reading wasm file: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/app.wasm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/wasm")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(wasmData)))
		w.Write(wasmData)
	})

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		cancel()
		return fmt.Errorf("listening: %w", err)
	}
	httpPort := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{Handler: mux}
	go func() {
		<-serveCtx.Done()
		server.Close()
	}()

	go server.Serve(listener)

	// Determine our local IP address for the WENDY_RELOAD message.
	localIP := getOutboundIP()

	output <- RunOutput{Type: RunOutputStarted}
	output <- RunOutput{
		Type: RunOutputStdout,
		Data: []byte(fmt.Sprintf("Serving WASM at http://%s:%d/app.wasm\n", localIP, httpPort)),
	}

	// Send WENDY_RELOAD messages: unicast to each known device, then broadcast.
	reloadMsg := fmt.Sprintf("WENDY_RELOAD %s:%d", localIP, httpPort)
	go func() {
		// Brief delay to ensure the HTTP server is fully ready.
		time.Sleep(500 * time.Millisecond)

		// Unicast to discovered device IPs.
		for _, ip := range bc.TargetIPs {
			sendUDP(ip, microWendyUDPPort, reloadMsg)
		}

		// Subnet broadcast as fallback.
		sendUDPBroadcast(microWendyUDPPort, reloadMsg)

		output <- RunOutput{
			Type: RunOutputStdout,
			Data: []byte(fmt.Sprintf("Sent WENDY_RELOAD to %d device(s) + broadcast\n", len(bc.TargetIPs))),
		}
	}()

	if detach {
		return nil
	}

	// Block until context is cancelled (Ctrl+C).
	<-serveCtx.Done()
	return nil
}

func (p *MicroWendyProvider) Stop(_ context.Context, app *BuiltApp) error {
	bc, ok := app.Context.(*microWendyBuildContext)
	if !ok {
		return fmt.Errorf("wendy-lite provider: invalid build context")
	}
	if bc.cancel != nil {
		bc.cancel()
	}
	return nil
}

// getOutboundIP returns the preferred outbound IP of this machine by
// dialing a UDP connection (no actual traffic is sent).
func getOutboundIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// sendUDP sends a UDP packet to a specific host:port.
func sendUDP(host string, port int, msg string) {
	addr := &net.UDPAddr{IP: net.ParseIP(host), Port: port}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.Write([]byte(msg))
}

// sendUDPBroadcast sends a UDP broadcast packet on the given port.
func sendUDPBroadcast(port int, msg string) {
	addr := &net.UDPAddr{IP: net.IPv4bcast, Port: port}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.Write([]byte(msg))
}

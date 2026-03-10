package services

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// AgentService implements agentpb.WendyAgentServiceServer.
type AgentService struct {
	agentpb.UnimplementedWendyAgentServiceServer
	logger             *zap.Logger
	networkManager     NetworkManager
	hardwareDiscoverer HardwareDiscoverer
	bluetoothManager   BluetoothManager
	updateMu           sync.Mutex
	isUpdating         bool
}

// NewAgentService creates a new AgentService.
func NewAgentService(
	logger *zap.Logger,
	nm NetworkManager,
	hd HardwareDiscoverer,
	bm BluetoothManager,
) *AgentService {
	return &AgentService{
		logger:             logger,
		networkManager:     nm,
		hardwareDiscoverer: hd,
		bluetoothManager:   bm,
	}
}

// GetAgentVersion returns the agent version, OS, architecture, and detected feature set.
func (s *AgentService) GetAgentVersion(_ context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	resp := &agentpb.GetAgentVersionResponse{
		Version:         version.Version,
		Os:              runtime.GOOS,
		CpuArchitecture: runtime.GOARCH,
		Featureset:      DetectFeatureset(),
	}

	// Read WendyOS version if available.
	if data, err := os.ReadFile("/etc/wendy/version.txt"); err == nil {
		v := strings.TrimSpace(string(data))
		resp.OsVersion = &v
	}

	return resp, nil
}

// DetectFeatureset probes the system for available hardware capabilities.
func DetectFeatureset() []string {
	var features []string

	// GPU: check for NVIDIA devices.
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		features = append(features, "gpu")
	} else if matches, _ := os.ReadDir("/dev/dri"); len(matches) > 0 {
		features = append(features, "gpu")
	}

	// Audio: check for ALSA, PipeWire, or PulseAudio.
	if _, err := os.Stat("/proc/asound/cards"); err == nil {
		features = append(features, "audio")
	} else if _, err := exec.LookPath("pactl"); err == nil {
		features = append(features, "audio")
	}

	// Bluetooth: check for hci devices.
	if _, err := os.Stat("/sys/class/bluetooth"); err == nil {
		if entries, _ := os.ReadDir("/sys/class/bluetooth"); len(entries) > 0 {
			features = append(features, "bluetooth")
		}
	}

	// Video: check for video devices.
	if entries, _ := os.ReadDir("/dev"); len(entries) > 0 {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "video") {
				features = append(features, "video")
				break
			}
		}
	}

	// Camera: same as video for now but could be refined.
	if _, err := os.Stat("/dev/video0"); err == nil {
		features = append(features, "camera")
	}

	return features
}

// RunContainer is deprecated. Clients should use WendyContainerService.RunContainer
// or WendyContainerService.CreateContainer + StartContainer instead.
func (s *AgentService) RunContainer(stream grpc.BidiStreamingServer[agentpb.RunContainerRequest, agentpb.RunContainerResponse]) error {
	s.logger.Warn("RunContainer called on deprecated WendyAgentService.RunContainer")
	return status.Error(codes.Unimplemented,
		"RunContainer is deprecated. Use WendyContainerService.RunContainer or CreateContainer + StartContainer instead. Please update your CLI.")
}

// UpdateAgent handles streaming binary updates with SHA256 verification and atomic replacement.
func (s *AgentService) UpdateAgent(stream grpc.BidiStreamingServer[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse]) error {
	s.updateMu.Lock()
	if s.isUpdating {
		s.updateMu.Unlock()
		return status.Error(codes.FailedPrecondition, "an update is already in progress")
	}
	s.isUpdating = true
	s.updateMu.Unlock()

	defer func() {
		s.updateMu.Lock()
		s.isUpdating = false
		s.updateMu.Unlock()
	}()

	s.logger.Info("UpdateAgent stream started")

	// Receive binary chunks.
	hasher := sha256.New()
	var binaryData []byte

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "error receiving update data: %v", err)
		}

		if chunk := msg.GetChunk(); chunk != nil {
			binaryData = append(binaryData, chunk.GetData()...)
			hasher.Write(chunk.GetData())
			continue
		}

		if ctrl := msg.GetControl(); ctrl != nil {
			if ctrl.GetUpdate() != nil {
				// Verify SHA256.
				computedHash := hex.EncodeToString(hasher.Sum(nil))
				expectedHash := ctrl.GetUpdate().GetSha256()
				if expectedHash != "" && computedHash != expectedHash {
					return status.Errorf(codes.DataLoss,
						"SHA256 mismatch: expected %s, got %s", expectedHash, computedHash)
				}

				// Resolve the current binary path (follow symlinks).
				execPath, err := os.Executable()
				if err != nil {
					return status.Errorf(codes.Internal, "failed to get executable path: %v", err)
				}
				execPath, err = filepath.EvalSymlinks(execPath)
				if err != nil {
					return status.Errorf(codes.Internal, "failed to resolve executable symlinks: %v", err)
				}

				// Capture original file permissions.
				info, err := os.Stat(execPath)
				if err != nil {
					return status.Errorf(codes.Internal, "failed to stat executable: %v", err)
				}
				originalPerm := info.Mode()

				// Write the new binary to a temp file.
				tmpPath := execPath + ".update"
				if err := os.WriteFile(tmpPath, binaryData, originalPerm); err != nil {
					return status.Errorf(codes.Internal, "failed to write update file: %v", err)
				}

				// Create a backup of the current binary.
				backupPath := execPath + ".backup"
				if err := os.Rename(execPath, backupPath); err != nil {
					os.Remove(tmpPath)
					return status.Errorf(codes.Internal, "failed to create backup: %v", err)
				}

				// Atomic rename of new binary to current path.
				if err := os.Rename(tmpPath, execPath); err != nil {
					// Rollback: restore from backup.
					if rbErr := os.Rename(backupPath, execPath); rbErr != nil {
						s.logger.Error("Failed to rollback from backup",
							zap.Error(rbErr),
							zap.String("backup_path", backupPath),
						)
					}
					os.Remove(tmpPath)
					return status.Errorf(codes.Internal, "failed to replace binary: %v", err)
				}

				s.logger.Info("Agent binary updated successfully",
					zap.String("sha256", computedHash),
					zap.Int("size", len(binaryData)),
				)

				if err := stream.Send(&agentpb.UpdateAgentResponse{
					ResponseType: &agentpb.UpdateAgentResponse_Updated_{
						Updated: &agentpb.UpdateAgentResponse_Updated{},
					},
				}); err != nil {
					return err
				}

				// Trigger process exit for systemd to restart the agent.
				go func() {
					time.Sleep(500 * time.Millisecond)
					os.Exit(0)
				}()

				return nil
			}
		}
	}

	return status.Error(codes.InvalidArgument, "update stream ended without update control command")
}

// ListWiFiNetworks delegates to the NetworkManager.
func (s *AgentService) ListWiFiNetworks(ctx context.Context, _ *agentpb.ListWiFiNetworksRequest) (*agentpb.ListWiFiNetworksResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	networks, err := s.networkManager.ListWiFiNetworks(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list WiFi networks: %v", err)
	}
	return &agentpb.ListWiFiNetworksResponse{Networks: networks}, nil
}

// ConnectToWiFi delegates to the NetworkManager.
func (s *AgentService) ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) (*agentpb.ConnectToWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.ConnectToWiFi(ctx, req.GetSsid(), req.GetPassword()); err != nil {
		errMsg := err.Error()
		return &agentpb.ConnectToWiFiResponse{Success: false, ErrorMessage: &errMsg}, nil
	}
	return &agentpb.ConnectToWiFiResponse{Success: true}, nil
}

// GetWiFiStatus delegates to the NetworkManager.
func (s *AgentService) GetWiFiStatus(ctx context.Context, _ *agentpb.GetWiFiStatusRequest) (*agentpb.GetWiFiStatusResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	connected, ssid, err := s.networkManager.GetWiFiStatus(ctx)
	if err != nil {
		errMsg := err.Error()
		return &agentpb.GetWiFiStatusResponse{ErrorMessage: &errMsg}, nil
	}
	return &agentpb.GetWiFiStatusResponse{Connected: connected, Ssid: &ssid}, nil
}

// DisconnectWiFi delegates to the NetworkManager.
func (s *AgentService) DisconnectWiFi(ctx context.Context, _ *agentpb.DisconnectWiFiRequest) (*agentpb.DisconnectWiFiResponse, error) {
	if s.networkManager == nil {
		return nil, status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	if err := s.networkManager.DisconnectWiFi(ctx); err != nil {
		errMsg := err.Error()
		return &agentpb.DisconnectWiFiResponse{Success: false, ErrorMessage: &errMsg}, nil
	}
	return &agentpb.DisconnectWiFiResponse{Success: true}, nil
}

// ListHardwareCapabilities discovers hardware on the device.
func (s *AgentService) ListHardwareCapabilities(ctx context.Context, req *agentpb.ListHardwareCapabilitiesRequest) (*agentpb.ListHardwareCapabilitiesResponse, error) {
	caps, err := s.hardwareDiscoverer.Discover(ctx, req.GetCategoryFilter())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hardware discovery failed: %v", err)
	}
	return &agentpb.ListHardwareCapabilitiesResponse{Capabilities: caps}, nil
}

// ScanBluetoothPeripherals streams discovered Bluetooth peripherals.
func (s *AgentService) ScanBluetoothPeripherals(stream grpc.BidiStreamingServer[agentpb.ScanBluetoothPeripheralsRequest, agentpb.ScanBluetoothPeripheralsResponse]) error {
	ctx := stream.Context()

	// Start scanning.
	ch, err := s.bluetoothManager.Scan(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start bluetooth scan: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case peripherals, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpb.ScanBluetoothPeripheralsResponse{
				DiscoveredDevices: peripherals,
			}); err != nil {
				return err
			}
		}
	}
}

// ConnectBluetoothPeripheral connects to a Bluetooth peripheral.
func (s *AgentService) ConnectBluetoothPeripheral(ctx context.Context, req *agentpb.ConnectBluetoothPeripheralRequest) (*agentpb.ConnectBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Connect(ctx, req.GetAddress(), req.GetPair(), req.GetTrust()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to connect bluetooth peripheral: %v", err)
	}
	return &agentpb.ConnectBluetoothPeripheralResponse{}, nil
}

// DisconnectBluetoothPeripheral disconnects a Bluetooth peripheral.
func (s *AgentService) DisconnectBluetoothPeripheral(ctx context.Context, req *agentpb.DisconnectBluetoothPeripheralRequest) (*agentpb.DisconnectBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Disconnect(ctx, req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to disconnect bluetooth peripheral: %v", err)
	}
	return &agentpb.DisconnectBluetoothPeripheralResponse{}, nil
}

// ForgetBluetoothPeripheral removes a paired Bluetooth peripheral.
func (s *AgentService) ForgetBluetoothPeripheral(ctx context.Context, req *agentpb.ForgetBluetoothPeripheralRequest) (*agentpb.ForgetBluetoothPeripheralResponse, error) {
	if err := s.bluetoothManager.Forget(ctx, req.GetAddress()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to forget bluetooth peripheral: %v", err)
	}
	return &agentpb.ForgetBluetoothPeripheralResponse{}, nil
}

// menderProgressRe matches percentage patterns in mender output, e.g.
// "  10%" or "50% 5120 kB" or "Installing:  75%".
var menderProgressRe = regexp.MustCompile(`(\d{1,3})%`)

// UpdateOS streams OS update progress using mender.
func (s *AgentService) UpdateOS(req *agentpb.UpdateOSRequest, stream grpc.ServerStreamingServer[agentpb.UpdateOSResponse]) error {
	s.logger.Info("UpdateOS started", zap.String("artifact_url", req.GetArtifactUrl()))

	sendProgress := func(phase string, percent int32) {
		_ = stream.Send(&agentpb.UpdateOSResponse{
			ResponseType: &agentpb.UpdateOSResponse_Progress_{
				Progress: &agentpb.UpdateOSResponse_Progress{
					Phase:   phase,
					Percent: percent,
				},
			},
		})
	}

	sendProgress("downloading", 0)

	cmd := exec.CommandContext(stream.Context(), "mender", "install", req.GetArtifactUrl())

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return stream.Send(&agentpb.UpdateOSResponse{
			ResponseType: &agentpb.UpdateOSResponse_Failed_{
				Failed: &agentpb.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("failed to create stderr pipe: %v", err),
				},
			},
		})
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return stream.Send(&agentpb.UpdateOSResponse{
			ResponseType: &agentpb.UpdateOSResponse_Failed_{
				Failed: &agentpb.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("failed to create stdout pipe: %v", err),
				},
			},
		})
	}

	if err := cmd.Start(); err != nil {
		return stream.Send(&agentpb.UpdateOSResponse{
			ResponseType: &agentpb.UpdateOSResponse_Failed_{
				Failed: &agentpb.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("failed to start mender: %v", err),
				},
			},
		})
	}

	// Stream progress by scanning mender's output in real time.
	// Mender writes structured log lines to stderr; stdout may have additional info.
	// We merge both and parse for phase transitions and percentage patterns.
	//
	// Download progress occupies 0-80% of the overall bar.
	// Install progress occupies 80-95%.
	// 95-100% is reserved for finalization.
	phase := "downloading"
	lastPercent := int32(0)

	scanLines := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			lower := strings.ToLower(line)
			s.logger.Debug("mender output", zap.String("line", line))

			// Detect phase transitions.
			switch {
			case strings.Contains(lower, "installing") || strings.Contains(lower, "writing artifact"):
				if phase != "installing" {
					phase = "installing"
					sendProgress(phase, 80)
					lastPercent = 80
				}
			case strings.Contains(lower, "download complete") || strings.Contains(lower, "download finished"):
				if phase == "downloading" {
					sendProgress("downloading", 80)
					lastPercent = 80
				}
			}

			// Extract percentage from the line.
			if m := menderProgressRe.FindStringSubmatch(line); len(m) > 1 {
				if pct, err := strconv.Atoi(m[1]); err == nil && pct >= 0 && pct <= 100 {
					var overall int32
					if phase == "downloading" {
						// Map download 0-100% → overall 0-80%
						overall = int32(pct) * 80 / 100
					} else {
						// Map install 0-100% → overall 80-95%
						overall = 80 + int32(pct)*15/100
					}
					if overall > lastPercent {
						lastPercent = overall
						sendProgress(phase, overall)
					}
				}
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanLines(stderr) }()
	go func() { defer wg.Done(); scanLines(stdout) }()

	// Wait for output scanners to finish (pipes close when process exits).
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return stream.Send(&agentpb.UpdateOSResponse{
			ResponseType: &agentpb.UpdateOSResponse_Failed_{
				Failed: &agentpb.UpdateOSResponse_Failed{
					ErrorMessage: fmt.Sprintf("mender install failed: %v", err),
				},
			},
		})
	}

	sendProgress("finalizing", 100)

	return stream.Send(&agentpb.UpdateOSResponse{
		ResponseType: &agentpb.UpdateOSResponse_Completed_{
			Completed: &agentpb.UpdateOSResponse_Completed{
				RebootRequired: true,
			},
		},
	})
}

// CleanupOldBackups removes agent binary backups older than 48 hours.
// This should be called on startup to clean up leftovers from previous updates.
func CleanupOldBackups(logger *zap.Logger) {
	execPath, err := os.Executable()
	if err != nil {
		logger.Debug("CleanupOldBackups: failed to get executable path", zap.Error(err))
		return
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		logger.Debug("CleanupOldBackups: failed to resolve symlinks", zap.Error(err))
		return
	}
	backupPath := execPath + ".backup"

	info, err := os.Stat(backupPath)
	if err != nil {
		// No backup file exists; nothing to do.
		return
	}

	if time.Since(info.ModTime()) > 48*time.Hour {
		if err := os.Remove(backupPath); err != nil {
			logger.Warn("Failed to remove old backup", zap.String("path", backupPath), zap.Error(err))
			return
		}
		logger.Info("Removed old backup", zap.String("path", backupPath))
	}
}

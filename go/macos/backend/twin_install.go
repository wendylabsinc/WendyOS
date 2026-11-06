package backend

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type twinBundle struct {
	Digest      string          `json:"digest"`
	DiffID      string          `json:"diffID"`
	Size        int64           `json:"size"`
	ImageConfig json.RawMessage `json:"imageConfig"`
	AppConfig   json.RawMessage `json:"appConfig"`
}

// OpenTwin installs the service carried by this signed Apple app. Installation
// is serialized with device operations and cancelled when the session changes.
func (b *Backend) OpenTwin(session uint64, directory string) (string, error) {
	err := func() error {
		ctx, done := b.beginWithTimeout(10 * time.Minute)
		defer done()
		conn, err := b.connection(session)
		if err != nil {
			return err
		}
		return ensureTwin(ctx, conn, directory)
	}()
	if err != nil {
		return "", fmt.Errorf("preparing Digital Twin on the selected device: %w", err)
	}
	return b.OpenApp(TwinAppID, session)
}
func loadTwinBundle(directory, arch string) (twinBundle, []byte, string, error) {
	var bundle twinBundle
	if arch != "arm64" && arch != "amd64" {
		return bundle, nil, "", errors.New("Digital Twin requires Linux arm64 or x86-64")
	}
	metadata, err := os.ReadFile(filepath.Join(directory, arch+".json"))
	if err != nil {
		return bundle, nil, "", fmt.Errorf("this app build is missing its Linux twin bundle: %w", err)
	}
	if err = json.Unmarshal(metadata, &bundle); err != nil {
		return bundle, nil, "", err
	}
	layer, err := os.ReadFile(filepath.Join(directory, arch+".tar.gz"))
	if err != nil {
		return bundle, nil, "", err
	}
	var cfg struct{ AppID, Version, Platform string }
	if err = json.Unmarshal(bundle.AppConfig, &cfg); err != nil {
		return bundle, nil, "", err
	}
	if cfg.AppID != TwinAppID || cfg.Platform != "linux" || !strings.HasPrefix(cfg.Version, "0.1.0-") || len(layer) == 0 || len(layer) > 128<<20 || int64(len(layer)) != bundle.Size || fmt.Sprintf("sha256:%x", sha256.Sum256(layer)) != bundle.Digest {
		return bundle, nil, "", errors.New("invalid bundled Digital Twin service")
	}
	var image struct {
		Architecture, OS string
		RootFS           struct {
			DiffIDs []string `json:"diff_ids"`
		}
	}
	if err = json.Unmarshal(bundle.ImageConfig, &image); err != nil {
		return bundle, nil, "", err
	}
	if image.Architecture != arch || image.OS != "linux" || len(image.RootFS.DiffIDs) != 1 || image.RootFS.DiffIDs[0] != bundle.DiffID {
		return bundle, nil, "", errors.New("invalid bundled image configuration")
	}
	return bundle, layer, cfg.Version, nil
}
func ensureTwin(ctx context.Context, conn *grpcclient.AgentConnection, directory string) error {
	hardware := commands.DesktopModelHardware(ctx, conn)
	if hardware.OS != "linux" {
		return errors.New("select a Linux device for Digital Twin")
	}
	arch := hardware.Architecture
	if arch == "aarch64" {
		arch = "arm64"
	}
	if arch == "x86_64" {
		arch = "amd64"
	}
	bundle, layer, version, err := loadTwinBundle(directory, arch)
	if err != nil {
		return err
	}
	apps, err := listDesktopApps(ctx, conn)
	if err != nil {
		return err
	}
	install := true
	for _, app := range apps {
		if app.GetAppName() != TwinAppID {
			if app.GetHttpPort() == 8791 {
				return errors.New("another app declares Digital Twin's port 8791")
			}
			continue
		}
		if app.GetHttpPort() != 8791 || len(app.GetServices()) != 0 {
			return errors.New("an app with incompatible settings already uses the Digital Twin service name")
		}
		if app.GetAppVersion() == version {
			install = false
			if app.GetRunningState() != pb.AppRunningState_RUNNING {
				if err = commands.DesktopStartApp(ctx, conn, TwinAppID); err != nil {
					return err
				}
			}
		}
	}
	if install {
		upload, err := conn.ContainerService.WriteLayer(ctx)
		if err != nil {
			return err
		}
		for offset := 0; offset < len(layer); offset += 256 << 10 {
			end := min(offset+(256<<10), len(layer))
			chunk := &pb.WriteLayerRequest{Data: layer[offset:end]}
			if offset == 0 {
				chunk.Digest = bundle.Digest
			}
			if err = upload.Send(chunk); err != nil {
				return err
			}
		}
		if err = upload.CloseSend(); err != nil {
			return err
		}
		if _, err = upload.Recv(); err != nil {
			return err
		}
		runCtx, cancel := context.WithCancel(ctx)
		stream, err := conn.ContainerService.RunContainer(runCtx, &pb.RunContainerLayersRequest{
			ImageName: "dev.wendy.digital-twin:" + version, AppName: TwinAppID, AppConfig: bundle.AppConfig, ImageConfig: bundle.ImageConfig,
			Layers:        []*pb.RunContainerLayerHeader{{Digest: bundle.Digest, DiffId: bundle.DiffID, Size: bundle.Size, Gzip: true}},
			RestartPolicy: &pb.RestartPolicy{Mode: pb.RestartPolicyMode_UNLESS_STOPPED},
		})
		if err != nil {
			cancel()
			return err
		}
		started := false
		for {
			message, e := stream.Recv()
			if e != nil {
				cancel()
				if errors.Is(e, io.EOF) {
					return errors.New("Digital Twin installation ended without a start acknowledgement")
				}
				return e
			}
			if message.GetStarted() != nil {
				started = true
				break
			}
		}
		cancel()
		if !started {
			return errors.New("Digital Twin did not start")
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialTwin(ctx, conn)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	ready, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var last error
	for {
		req, _ := http.NewRequestWithContext(ready, "GET", "http://wendy-device/health", nil)
		response, e := client.Do(req)
		if e == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 64))
			response.Body.Close()
			if readErr == nil && response.StatusCode == 200 && string(body) == "ok" {
				return nil
			}
			e = errors.New("Digital Twin returned an unexpected health response")
		}
		last = e
		select {
		case <-ready.Done():
			return fmt.Errorf("Digital Twin did not become ready: %w", last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func dialTwin(ctx context.Context, conn *grpcclient.AgentConnection) (net.Conn, error) {
	return commands.DesktopTwinConnection(ctx, conn)
}

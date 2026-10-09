package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

// prepareLiteFlash modifies only a private copy. Published firmware and local
// input images remain unchanged, including any physical device credentials.
func prepareLiteFlash(name string, data []byte) ([]byte, error) {
	if len(data) < espPartTableOffset+espPartEntrySize || len(data) > 16<<20 {
		return nil, fmt.Errorf("expected a merged ESP32-C6 flash image of at most 16 MiB")
	}
	if data[0] != 0xe9 || binary.LittleEndian.Uint16(data[12:14]) != 13 {
		return nil, fmt.Errorf("wendy-lite simulation requires merged ESP32-C6 firmware, including its bootloader at offset 0")
	}
	img := NewEspFlashImage(data)
	part, err := img.findPartition("wendy_conf")
	if err != nil {
		return nil, err
	}
	if part.Size < 8 || uint64(part.Offset)+uint64(part.Size) > 16<<20 || part.Offset < espPartTableOffset+espPartTableMaxLen {
		return nil, fmt.Errorf("invalid Wendy Lite configuration partition")
	}
	conf := &litepb.WendyConf{DeviceName: proto.String(name), Wifi: &litepb.WendyConfWifi{
		Networks: []*litepb.WendyConfWifiNetwork{{Ssid: vm.LiteSSID, Password: vm.LitePassword}},
	}}
	pb, err := proto.Marshal(conf)
	if err != nil {
		return nil, err
	}
	payload := append([]byte("WYC0\x00\x00\x00\x00"), pb...)
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(pb)))
	if err := img.SetPartition("wendy_conf", payload); err != nil {
		return nil, err
	}
	return img.Bytes(), nil
}

func createLiteSimulator(ctx context.Context, store *vm.Store, opts wendymcp.SimulatorCreateOptions) (*wendymcp.SimulatorInfo, error) {
	if opts.Image != "" && opts.Version != "" {
		return nil, fmt.Errorf("image and version cannot be combined")
	}
	if err := store.CheckCreatable(opts.Name); err != nil {
		return nil, err
	}
	profile := opts.Profile
	if profile == "" {
		profile = vm.ProfileWendyLite
	}
	firmwareID := "esp32c6"
	if profile == vm.ProfileWendyLiteNative {
		firmwareID = "esp32c6_native"
	}
	path, version, source := opts.Image, "", "local"
	if path == "" {
		manifest, err := fetchMainManifestContext(ctx)
		if err != nil {
			return nil, err
		}
		firmware, ok := manifest.Firmware[firmwareID]
		if !ok {
			return nil, fmt.Errorf("no published %s firmware; supply a merged --image", firmwareID)
		}
		version = opts.Version
		if version == "" {
			version = firmware.Latest
		}
		fm, err := fetchFirmwareManifestContext(ctx, firmware.ManifestPath)
		if err != nil {
			return nil, err
		}
		if fm.FirmwareID != "" && fm.FirmwareID != firmwareID {
			return nil, fmt.Errorf("firmware manifest does not describe %s", firmwareID)
		}
		info, err := getFirmwareInfo(fm, version)
		if err != nil {
			return nil, err
		}

		dir, err := os.MkdirTemp("", "wendy-lite-simulator-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		path, err = downloadMCPSimulatorImage(ctx, dir, info.DownloadURL)
		if err != nil {
			return nil, err
		}
		if err := verifyMCPSimulatorImage(ctx, path, info.Checksum); err != nil {
			return nil, err
		}
		source = "release"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("firmware must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(simulatorContextReader{ctx: ctx, reader: f}, (16<<20)+1))
	if err != nil {
		return nil, err
	}
	data, err = prepareLiteFlash(opts.Name, data)
	if err != nil {
		return nil, err
	}
	if profile == vm.ProfileWendyLiteNative {
		img := NewEspFlashImage(data)
		for _, part := range []string{"ota_0", "ota_1", "otadata"} {
			if _, err := img.findPartition(part); err != nil {
				return nil, fmt.Errorf("native ESP-IDF simulation requires native firmware with OTA slots: %w", err)
			}
		}
	}
	size := int64(len(data))
	if err := store.CreateFrom(opts.Name, bytes.NewReader(data), size, size, vm.Meta{
		Profile: profile, ImageVersion: version, ImageSource: source,
	}); err != nil {
		return nil, err
	}
	return readMCPSimulator(store, opts.Name)
}

func isLiteSimulator(name string) bool {
	store, err := vm.NewStore()
	if err != nil || vm.ValidName(name) != nil {
		return false
	}
	meta, ok := store.ReadMeta(name)
	return ok && vm.IsLiteProfile(meta.Profile)
}

func connectLiteSimulator(ctx context.Context, name, addr string) (*SelectedDevice, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		client := liteclient.NewWendyLiteClient()
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = client.ConnectInsecureContext(attempt, addr)
		cancel()
		if err == nil {
			err = client.Ping()
		}
		client.Close()
		if err == nil {
			break
		}
		if !simulatorStillRunning(name) || time.Now().After(deadline) {
			return nil, fmt.Errorf("Wendy Lite simulator %q did not become ready: %w%s", name, err, vmConsoleTail(name))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return &SelectedDevice{External: &models.ExternalDevice{
		ID: "vm:" + name, DisplayName: name, ProviderKey: "wendy-lite", IsWendyDevice: true,
		ConnectionInfo: map[string]string{"type": "LAN", "ip": host, "port": port},
	}, Provider: &providers.MicroWendyProvider{}, DefaultSelector: "vm:" + name}, nil
}

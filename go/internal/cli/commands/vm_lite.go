package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

const maxLiteFirmwareBytes = 16 << 20

// prepareLiteFlash modifies only a private copy. Published firmware and local
// input images remain unchanged, including any physical device credentials.
func prepareLiteFlash(name string, data []byte) ([]byte, error) {
	if len(data) < espPartTableOffset+espPartEntrySize || len(data) > maxLiteFirmwareBytes {
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
	if part.Size < 8 || uint64(part.Offset)+uint64(part.Size) > maxLiteFirmwareBytes || part.Offset < espPartTableOffset+espPartTableMaxLen {
		return nil, fmt.Errorf("invalid Wendy Lite configuration partition")
	}
	conf := &litepb.WendyConf{DeviceName: proto.String(name), Wifi: &litepb.WendyConfWifi{
		Networks: []*litepb.WendyConfWifiNetwork{{Ssid: vm.LiteSSID, Password: vm.LitePassword}},
	}}
	payload, err := wendyConfPayload(conf)
	if err != nil {
		return nil, err
	}
	if err := img.SetPartition("wendy_conf", payload); err != nil {
		return nil, err
	}
	return img.Bytes(), nil
}

// createLiteSimulator creates a stopped Wendy Lite simulator. Without
// opts.Image, it builds Wendy Lite from source for the simulator profile, as
// wendy install builds it for a board (buildLiteFirmware): the latest
// release, the nightly tag with opts.Nightly, or opts.Version.
func createLiteSimulator(ctx context.Context, store *vm.Store, opts wendymcp.SimulatorCreateOptions) (*wendymcp.SimulatorInfo, error) {
	if opts.Image != "" && opts.Version != "" {
		return nil, fmt.Errorf("image and version cannot be combined")
	}
	if opts.Image != "" && opts.Nightly {
		return nil, fmt.Errorf("image and nightly cannot be combined")
	}
	if opts.Nightly && opts.Version != "" {
		return nil, fmt.Errorf("nightly and version cannot be combined")
	}
	// Before the build, which takes minutes.
	if err := store.CheckCreatable(opts.Name); err != nil {
		return nil, err
	}
	profile := opts.Profile
	if profile == "" {
		profile = vm.ProfileWendyLite
	}
	var data []byte
	version, source := "", "local"
	if opts.Image == "" {
		// The build output goes to stderr: stdout is the JSON-RPC channel of
		// the MCP server and the desktop app.
		fw, err := buildLiteFirmwareFn(ctx, liteBuildOptions{
			nightly:     opts.Nightly,
			version:     opts.Version,
			simulator:   profile,
			out:         os.Stderr,
			versionHint: liteSimulatorVersionHint(opts.Name, profile),
		})
		if err != nil {
			return nil, err
		}
		data, version, source = fw.img.Bytes(), fw.src.Version, "build"
	} else {
		var err error
		if data, err = readLiteFirmwareFile(ctx, opts.Image); err != nil {
			return nil, err
		}
	}
	data, err := prepareLiteFlash(opts.Name, data)
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

// readLiteFirmwareFile reads a local merged firmware image, up to one byte
// more than prepareLiteFlash accepts.
func readLiteFirmwareFile(ctx context.Context, path string) ([]byte, error) {
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
	return io.ReadAll(io.LimitReader(simulatorContextReader{ctx: ctx, reader: f}, maxLiteFirmwareBytes+1))
}

// liteSimulatorVersionHint is the hint of a Wendy Lite version that cannot
// be built, for the simulator name of the profile.
func liteSimulatorVersionHint(name, profile string) string {
	return fmt.Sprintf("Specify a Wendy Lite version: wendy vm create %s --profile %s --version <version>", name, profile)
}

// liteSimulatorGiven returns the configurator values of a simulator profile:
// the values of the profile in the simulator section of the master
// configuration m of Wendy Lite src, which explains them. The rules give the
// other values; a question left is an error (liteSimulatorPrompter).
func liteSimulatorGiven(m *litebuild.Model, src litebuild.Source, profile string) (map[string]any, error) {
	values, ok := m.Simulators[profile]
	if !ok {
		return nil, liteSimulatorIncompatible(src, profile, fmt.Errorf(
			"%s has no %s profile %q: update the Wendy CLI, or choose another Wendy Lite version with --version",
			litebuild.MasterConfFile, litebuild.SimulatorSection, profile))
	}
	return maps.Clone(values), nil
}

// checkLiteSimulatorChip checks the chip of a simulator build, before the
// build: esp-emu emulates only vm.LiteChip.
func checkLiteSimulatorChip(src litebuild.Source, profile, chip string) error {
	if chip != vm.LiteChip {
		return liteSimulatorIncompatible(src, profile, fmt.Errorf(
			"the profile gives chip %s, but the simulator emulates an %s", chip, vm.LiteChip))
	}
	return nil
}

// liteSimulatorIncompatible is the error of a Wendy Lite version that cannot
// be built with the values of a simulator profile.
func liteSimulatorIncompatible(src litebuild.Source, profile string, err error) error {
	return fmt.Errorf("Wendy Lite %s cannot be built for the %s simulator: %w", src, profile, err)
}

// liteSimulatorPrompter answers no question, even in a terminal: the values
// of a simulator profile must leave none. A question means that the Wendy
// Lite version has an attribute that this CLI does not know.
type liteSimulatorPrompter struct {
	litebuild.TUIPrompter // for Info
	src                   litebuild.Source
	profile               string
}

func (p *liteSimulatorPrompter) Select(q litebuild.Question) (int, error) {
	return 0, p.question(q)
}

func (p *liteSimulatorPrompter) Confirm(q litebuild.Question) (bool, error) {
	return false, p.question(q)
}

func (p *liteSimulatorPrompter) Text(q litebuild.Question, _ func(string) error) (string, error) {
	return "", p.question(q)
}

func (p *liteSimulatorPrompter) question(q litebuild.Question) error {
	return fmt.Errorf("Wendy Lite %s asks for %q (%s), which the %s simulator does not set: update the Wendy CLI, or choose another Wendy Lite version with --version",
		p.src, q.Attr.Title(), q.Attr.Name, p.profile)
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
	}, Provider: &providers.MicroWendyProvider{}, Selector: "vm:" + name}, nil
}

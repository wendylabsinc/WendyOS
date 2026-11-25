package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

func liteTestImage() []byte {
	b := bytes.Repeat([]byte{0xff}, 0x10000)
	b[0] = 0xe9
	binary.LittleEndian.PutUint16(b[12:14], 13)
	p := b[espPartTableOffset : espPartTableOffset+espPartEntrySize]
	clear(p)
	binary.LittleEndian.PutUint16(p, espPartMagic)
	p[2] = 1
	binary.LittleEndian.PutUint32(p[4:], 0xf000)
	binary.LittleEndian.PutUint32(p[8:], 0x1000)
	copy(p[12:], "wendy_conf")
	return b
}

// liteTestNativeImage is liteTestImage with the OTA partitions of native
// firmware.
func liteTestNativeImage() []byte {
	b := liteTestImage()
	for i, part := range []struct {
		name         string
		typ, subtype byte
		offset, size uint32
	}{
		{"otadata", 1, 0, 0xd000, 0x2000},
		{"ota_0", 0, 0x10, 0x10000, 0x1000},
		{"ota_1", 0, 0x11, 0x11000, 0x1000},
	} {
		p := b[espPartTableOffset+(i+1)*espPartEntrySize : espPartTableOffset+(i+2)*espPartEntrySize]
		clear(p)
		binary.LittleEndian.PutUint16(p, espPartMagic)
		p[2], p[3] = part.typ, part.subtype
		binary.LittleEndian.PutUint32(p[4:], part.offset)
		binary.LittleEndian.PutUint32(p[8:], part.size)
		copy(p[12:], part.name)
	}
	return b
}

// stubLiteSimulatorBuild replaces the Wendy Lite build: it records the
// options and returns image.
func stubLiteSimulatorBuild(t *testing.T, image []byte) *[]liteBuildOptions {
	t.Helper()
	var calls []liteBuildOptions
	previous := buildLiteFirmwareFn
	t.Cleanup(func() { buildLiteFirmwareFn = previous })
	buildLiteFirmwareFn = func(_ context.Context, o liteBuildOptions) (*liteFirmware, error) {
		calls = append(calls, o)
		src := litebuild.Source{Name: "feature/x", Version: strings.Repeat("ab", 20), Commit: strings.Repeat("ab", 20)}
		return &liteFirmware{img: NewEspFlashImage(bytes.Clone(image)), src: src}, nil
	}
	return &calls
}

func TestLiteSimulatorBuildsFirmware(t *testing.T) {
	for _, tc := range []struct {
		profile, appType string
		image            []byte
	}{
		{vm.ProfileWendyLite, "wasm", liteTestImage()},
		{vm.ProfileWendyLiteNative, "native", liteTestNativeImage()},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			calls := stubLiteSimulatorBuild(t, tc.image)
			store := &vm.Store{Root: t.TempDir()}
			info, err := createLiteSimulator(context.Background(), store, wendymcp.SimulatorCreateOptions{Name: "lite", Profile: tc.profile, Version: "feature/x"})
			if err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Fatalf("%d builds, want 1", len(*calls))
			}
			o := (*calls)[0]
			want := map[string]string{
				"board": "custom", "chip": "esp32c6", "flash_size": "8MiB",
				"app_type": tc.appType, "sensor_link_av": "false", "cloud_tunnel": "false",
				"chain_algorithm": "ecdsa_p256",
			}
			if !maps.Equal(o.attrs, want) || o.simulator != tc.profile || o.version != "feature/x" || o.nightly {
				t.Fatalf("build options: attrs %v, simulator %q, version %q, nightly %v", o.attrs, o.simulator, o.version, o.nightly)
			}
			// stdout is the JSON-RPC channel of the MCP server.
			if o.out != os.Stderr {
				t.Fatal("the build output does not go to stderr")
			}
			if !strings.Contains(o.versionHint, "wendy vm create lite --profile "+tc.profile+" --version") {
				t.Fatalf("version hint: %q", o.versionHint)
			}
			if info.Profile != tc.profile || info.Version != strings.Repeat("ab", 20) || info.Source != "build" || info.State != "stopped" {
				t.Fatalf("unexpected simulator: %+v", info)
			}
			disk, err := os.ReadFile(store.DiskPath("lite"))
			if err != nil {
				t.Fatal(err)
			}
			if string(disk[0xf000:0xf004]) != "WYC0" {
				t.Fatal("the simulator configuration is not written")
			}

			// A taken name fails before the build.
			if _, err := createLiteSimulator(context.Background(), store, wendymcp.SimulatorCreateOptions{Name: "lite", Profile: tc.profile, Nightly: true}); err == nil {
				t.Fatal("duplicate accepted")
			}
			if len(*calls) != 1 {
				t.Fatal("a taken name was built")
			}
		})
	}
}

func TestLiteSimulatorBuildChecks(t *testing.T) {
	calls := stubLiteSimulatorBuild(t, liteTestImage())
	store := &vm.Store{Root: t.TempDir()}

	// A WASM build has no OTA slots: the native profile refuses it.
	_, err := createLiteSimulator(context.Background(), store, wendymcp.SimulatorCreateOptions{Name: "native", Profile: vm.ProfileWendyLiteNative, Nightly: true})
	if err == nil || !strings.Contains(err.Error(), "OTA slots") {
		t.Fatalf("got %v", err)
	}
	if len(*calls) != 1 || !(*calls)[0].nightly {
		t.Fatalf("builds: %+v", *calls)
	}
	if _, err := os.Stat(store.Dir("native")); !os.IsNotExist(err) {
		t.Fatal("incompatible firmware created a simulator")
	}

	path := t.TempDir() + "/firmware.bin"
	if err := os.WriteFile(path, liteTestImage(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []wendymcp.SimulatorCreateOptions{
		{Name: "lite", Image: path, Nightly: true},
		{Name: "lite", Image: path, Version: "1.0.0"},
		{Name: "lite", Nightly: true, Version: "1.0.0"},
	} {
		if _, err := createLiteSimulator(context.Background(), store, opts); err == nil {
			t.Errorf("%+v accepted", opts)
		}
	}
	if len(*calls) != 1 {
		t.Fatal("refused options were built")
	}
}

// liteSimulatorConf is a master configuration with the attributes of the
// simulator profiles. Each attribute changes the sdkconfig, as in the real
// one: an attribute that changes nothing is never asked, and a value given
// for it is not possible.
const liteSimulatorConf = `
attributes:
  board: {type: string, enum: [custom, esp32c6_devkit_c_1_n8]}
  chip: {type: string, enum: [esp32c6, esp32s3]}
  idf_version: {type: string, enum: ["6.0.3"]}
  idf_preview: {type: boolean}
  flash_size: {type: size, enum: [4MiB, 8MiB]}
  app_type: {type: string, enum: [native, wasm]}
  sensor_link_av: {type: boolean}
  wendy_net: {type: boolean}
  cloud_tunnel: {type: boolean}
  chain_algorithm: {type: string, enum: [ecdsa_p256, ml_dsa]}
rules:
  - if: board == "custom"
    set: {wendy_net: false}
  - if: board == "esp32c6_devkit_c_1_n8"
    set: {chip: esp32c6, flash_size: 8MiB}
    sdkconfig: CONFIG_DEVKIT=y
  - set: {idf_version: "6.0.3", idf_preview: false}
  - if: chip == "esp32c6"
    sdkconfig: CONFIG_C6=y
  - if: flash_size == 8MiB
    sdkconfig: CONFIG_8MB=y
  - if: app_type == "native"
    sdkconfig: CONFIG_NATIVE=y
  - if: sensor_link_av
    sdkconfig: CONFIG_AV=y
  - if: wendy_net
    sdkconfig: CONFIG_NET=y
  - if: cloud_tunnel
    sdkconfig: CONFIG_TUNNEL=y
  - if: chain_algorithm == "ml_dsa"
    sdkconfig: CONFIG_MLDSA=y
  - if: chain_algorithm == "ml_dsa" && chip == "esp32c6"
    warn: {id: w1, message: The ESP32-C6 has no PSRAM for ML-DSA.}
display:
  board: {priority: 1}
  chip: {priority: 2}
  flash_size: {priority: 3}
  sensor_link_av: {priority: 4}
  app_type: {priority: 5}
  wendy_net: {priority: 6}
  cloud_tunnel: {priority: 7}
  chain_algorithm: {priority: 8}
`

func TestLiteSimulatorBuildValues(t *testing.T) {
	src := litebuild.Source{Name: "1.2.3", Version: "1.2.3", Commit: strings.Repeat("cd", 20)}
	run := func(conf string) (*litebuild.Model, *litebuild.Result, error) {
		t.Helper()
		m, err := litebuild.Load([]byte(conf), "test")
		if err != nil {
			t.Fatal(err)
		}
		c, err := litebuild.NewConfigurator(m, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.Given, err = liteSimulatorGiven(m, src, vm.ProfileWendyLiteNative, liteSimulatorAttrs(vm.ProfileWendyLiteNative))
		if err != nil {
			return m, nil, err
		}
		res, err := c.Run(&liteSimulatorPrompter{TUIPrompter: litebuild.TUIPrompter{Out: io.Discard}, src: src, profile: vm.ProfileWendyLiteNative})
		return m, res, err
	}

	// The profile answers every question.
	m, res, err := run(liteSimulatorConf)
	if err != nil {
		t.Fatal(err)
	}
	b, err := liteBuildSteps(m, res)
	if err != nil || b.chip != "esp32c6" {
		t.Fatalf("build: %+v, %v", b, err)
	}
	if !slices.Contains(b.configure, "app_type=native") || !slices.Contains(b.configure, "board=custom") {
		t.Fatalf("configure.py arguments: %v", b.configure)
	}
	if len(res.Solution.Warnings) != 0 {
		t.Errorf("the profile gives warning %s", res.Solution.Warnings[0].Warning.ID)
	}

	// A question that the profile does not answer.
	noCustomRule := strings.Replace(liteSimulatorConf, "  - if: board == \"custom\"\n    set: {wendy_net: false}\n", "", 1)
	_, _, err = run(noCustomRule)
	if err == nil || !strings.Contains(err.Error(), "(wendy_net), which the wendy-lite-native simulator does not set") {
		t.Fatalf("got %v", err)
	}

	// A profile value that the master configuration does not accept.
	_, _, err = run(strings.Replace(noCustomRule, "[custom, esp32c6_devkit_c_1_n8]", "[esp32c6_devkit_c_1_n8]", 1))
	if err == nil || !strings.HasPrefix(err.Error(), "Wendy Lite 1.2.3 cannot be built for the wendy-lite-native simulator: ") {
		t.Fatalf("got %v", err)
	}
}

func TestLiteFlashPrivateConfiguration(t *testing.T) {
	input := liteTestImage()
	original := bytes.Clone(input)
	b, err := prepareLiteFlash("test-lite", input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("source firmware changed")
	}
	if !bytes.Equal(b[:0xf000], original[:0xf000]) {
		t.Fatal("firmware outside config changed")
	}
	if string(b[0xf000:0xf004]) != "WYC0" {
		t.Fatal("missing config header")
	}
	var conf litepb.WendyConf
	n := binary.LittleEndian.Uint32(b[0xf004:0xf008])
	if err := proto.Unmarshal(b[0xf008:0xf008+n], &conf); err != nil {
		t.Fatal(err)
	}
	if conf.GetDeviceName() != "test-lite" || conf.GetWifi().GetNetworks()[0].GetSsid() != vm.LiteSSID || conf.Provisioning != nil {
		t.Fatalf("unexpected config: %v", &conf)
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { b[0] = 0 },
		func(b []byte) { b[12] = 9 },
		func(b []byte) { binary.LittleEndian.PutUint32(b[espPartTableOffset+4:], 0xfffffff0) },
	} {
		bad := bytes.Clone(input)
		mutate(bad)
		if _, err := prepareLiteFlash("test-lite", bad); err == nil {
			t.Fatal("invalid firmware accepted")
		}
	}
}

func TestLiteSimulatorCreation(t *testing.T) {
	store := &vm.Store{Root: t.TempDir()}
	path := t.TempDir() + "/firmware.bin"
	if err := os.WriteFile(path, liteTestImage(), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := createMCPSimulator(context.Background(), store, wendymcp.SimulatorCreateOptions{Name: "lite", Profile: vm.ProfileWendyLite, Image: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Profile != vm.ProfileWendyLite || info.DiskBytes != 0x10000 || info.State != "stopped" {
		t.Fatalf("unexpected simulator: %+v", info)
	}
	if _, err := os.Stat(store.VarsPath("lite")); !os.IsNotExist(err) {
		t.Fatal("Lite created UEFI variables")
	}
	if _, err := createMCPSimulator(context.Background(), store, wendymcp.SimulatorCreateOptions{Name: "lite", Profile: vm.ProfileWendyLite, Image: path}, nil); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestLiteNativeSimulatorRejectsWasmImage(t *testing.T) {
	store := &vm.Store{Root: t.TempDir()}
	path := t.TempDir() + "/wasm.bin"
	if err := os.WriteFile(path, liteTestImage(), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := createLiteSimulator(context.Background(), store, wendymcp.SimulatorCreateOptions{Name: "native", Profile: vm.ProfileWendyLiteNative, Image: path})
	if err == nil || !strings.Contains(err.Error(), "OTA slots") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(store.Dir("native")); !os.IsNotExist(err) {
		t.Fatal("incompatible image created a simulator")
	}
}

func TestLiteNativeSimulatorFirmware(t *testing.T) {
	image := os.Getenv("WENDY_TEST_LITE_NATIVE_FIRMWARE")
	if image == "" {
		t.Skip("set WENDY_TEST_LITE_NATIVE_FIRMWARE to a merged native firmware image")
	}
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	store, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	info, err := createLiteSimulator(ctx, store, wendymcp.SimulatorCreateOptions{Name: "native", Profile: vm.ProfileWendyLiteNative, Image: image})
	if err != nil {
		t.Fatal(err)
	}
	if info.Profile != vm.ProfileWendyLiteNative {
		t.Fatalf("profile: %s", info.Profile)
	}
	t.Cleanup(func() { _ = store.Stop("native", true, time.Second) })
	target, err := connectSimulatorChoice(ctx, &simulatorChoice{Name: "native"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	di, err := target.Provider.GetDeviceInfo(ctx, *target.External)
	if err != nil {
		t.Fatal(err)
	}
	if !di.NativeAppSupport {
		t.Fatal("native firmware did not advertise native app support")
	}

	// Re-upload the native firmware's app image through WendyCom OTA, then
	// verify the bootloader selected the other slot after a reboot.
	flash, err := LoadEspFlashImage(image)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := flash.findPartition("ota_0")
	if err != nil {
		t.Fatal(err)
	}
	if uint64(slot.Offset)+uint64(slot.Size) > uint64(len(flash.Bytes())) {
		t.Fatal("truncated native app partition")
	}
	appPath := t.TempDir() + "/native.bin"
	if err := os.WriteFile(appPath, flash.Bytes()[slot.Offset:slot.Offset+slot.Size], 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Status("native")
	if err != nil {
		t.Fatal(err)
	}
	client := liteclient.NewWendyLiteClient()
	if err := client.ConnectInsecureContext(ctx, fmt.Sprintf("127.0.0.1:%d", st.State.AgentPort)); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.PushApp(appPath, liteclient.AppTypeNative, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.ResetTargetDevice(true, 0); err != nil {
		t.Fatal(err)
	}
	client.Close()
	for {
		log, _ := os.ReadFile(store.LogPath("native"))
		if strings.Contains(string(log), "Loaded app from partition at offset 0x1f0000") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("native OTA did not boot: %s", log)
		case <-time.After(100 * time.Millisecond):
		}
	}
	reconnected, err := connectSimulatorChoice(ctx, &simulatorChoice{Name: "native"}, true)
	if err != nil {
		t.Fatal(err)
	}
	reconnected.Close()
	if err := store.StopContext(ctx, "native", false, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

// Opt-in hardware-free integration test using a real merged Wendy Lite build.
func TestLiteSimulatorFirmware(t *testing.T) {
	image := os.Getenv("WENDY_TEST_LITE_FIRMWARE")
	if image == "" {
		t.Skip("set WENDY_TEST_LITE_FIRMWARE and put esp-emu on PATH")
	}
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	store, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err = createLiteSimulator(ctx, store, wendymcp.SimulatorCreateOptions{Name: "lite", Image: image})
	if err != nil {
		t.Fatal(err)
	}
	picked, err := connectSimulatorChoice(ctx, &simulatorChoice{Name: "lite"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if picked.External == nil || picked.External.ProviderKey != "wendy-lite" {
		t.Fatalf("wrong target: %+v", picked)
	}
	picked.Close()
	t.Cleanup(func() { _ = store.Stop("lite", true, time.Second) })
	st, err := store.Status("lite")
	if err != nil {
		t.Fatal(err)
	}
	c := liteclient.NewWendyLiteClient()
	attempt, done := context.WithTimeout(ctx, 5*time.Second)
	err = c.ConnectInsecureContext(attempt, fmt.Sprintf("127.0.0.1:%d", st.State.AgentPort))
	done()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A valid WASM module exporting an empty _start. Execute it through the
	// firmware's WAMR runtime, then verify the same app survives a cold boot.
	wasm, _ := hex.DecodeString("0061736d0100000001040160000003020100070a01065f737461727400000a040102000b")
	appPath := t.TempDir() + "/smoke.wasm"
	if err := os.WriteFile(appPath, wasm, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.PushApp(appPath, liteclient.AppTypeWasm, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.StartApp(); err != nil {
		t.Fatal(err)
	}
	c.Close()

	if err := store.StopContext(ctx, "lite", false, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	st, err = store.Status("lite")
	if err != nil || st.Running {
		t.Fatalf("stop: %+v %v", st, err)
	}
	picked, err = connectSimulatorChoice(ctx, &simulatorChoice{Name: "lite"}, true)
	if err != nil {
		t.Fatal(err)
	}
	picked.Close()
	if err := store.StopContext(ctx, "lite", false, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(store.LogPath("lite"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "invalid WASM binary") || !strings.Contains(string(log), "WASM module finished") {
		t.Fatalf("app was not persisted: %s", log)
	}
}

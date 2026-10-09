package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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

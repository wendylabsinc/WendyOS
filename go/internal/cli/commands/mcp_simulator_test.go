package commands

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func TestMCPSimulatorLocalCreationProfilesAndLifecycle(t *testing.T) {
	store := &vm.Store{Root: t.TempDir()}
	image := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(image, []byte("disk bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	backend := newSimulatorBackend(func() (*vm.Store, error) { return store, nil }, func(context.Context, vm.Arch, string) (string, string, func(), error) {
		t.Fatal("local image triggered network resolver")
		return "", "", nil, nil
	})
	for _, profile := range []string{"generic", "go2", "g1", "so101", "rosmaster-r2"} {
		info, err := backend.Create(context.Background(), wendymcp.SimulatorCreateOptions{Name: profile, Profile: profile, Image: image, DiskGiB: 1})
		if err != nil {
			t.Fatal(err)
		}
		if info.State != "stopped" || info.Device != "vm:"+profile || info.Profile != profile || info.DiskBytes != 1<<30 || info.Source != "local" {
			t.Fatalf("created %+v", info)
		}
		if _, err := backend.Create(context.Background(), wendymcp.SimulatorCreateOptions{Name: profile, Profile: profile, DiskGiB: 1}); err == nil {
			t.Fatal("duplicate name accepted")
		}
	}
	items, err := backend.List(context.Background())
	if err != nil || len(items) != 5 {
		t.Fatalf("list: %v %v", items, err)
	}
	if _, err := backend.Stop(context.Background(), "go2", false, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(context.Background(), "go2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Dir("go2")); !os.IsNotExist(err) {
		t.Fatalf("delete left simulator: %v", err)
	}
	if _, err := backend.Stop(context.Background(), "missing", false, time.Second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stop missing: %v", err)
	}
}

func TestMCPSimulatorListsCorruptProfileAndIncompleteCreation(t *testing.T) {
	store := &vm.Store{Root: t.TempDir()}
	if err := store.CreateFrom("broken", strings.NewReader("disk"), 4, 1024, vm.Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.RobotProfilePath("broken"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.Dir("creating"), 0700); err != nil {
		t.Fatal(err)
	}
	backend := newSimulatorBackend(func() (*vm.Store, error) { return store, nil }, nil)
	items, err := backend.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("list: %+v %v", items, err)
	}
	if items[0].Profile != "" || items[0].Error == "" || items[1].State != "unknown" || items[1].Error == "" {
		t.Fatalf("unreadable records reported as healthy: %+v", items)
	}
}

func TestMCPSimulatorPublishedCreationHonorsCancellationBeforeWrite(t *testing.T) {
	store := &vm.Store{Root: t.TempDir()}
	image := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(image, []byte("disk"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cleaned := false
	backend := newSimulatorBackend(func() (*vm.Store, error) { return store, nil }, func(ctx context.Context, _ vm.Arch, version string) (string, string, func(), error) {
		if version != "v1" {
			t.Fatalf("version = %q", version)
		}
		cancel()
		return image, version, func() { cleaned = true }, nil
	})
	_, err := backend.Create(ctx, wendymcp.SimulatorCreateOptions{Name: "dev", Profile: "g1", Version: "v1", DiskGiB: 1})
	if !errors.Is(err, context.Canceled) || !cleaned {
		t.Fatalf("cancelled create: %v, cleaned %v", err, cleaned)
	}
	if _, err := os.Stat(store.Dir("dev")); !os.IsNotExist(err) {
		t.Fatalf("cancelled creation left VM: %v", err)
	}
}

func TestMCPSimulatorDownloadCancellationRemovesPartialFile(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := downloadMCPSimulatorImage(ctx, dir, server.URL)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("download cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download ignored cancellation")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("partial files left: %v %v", files, err)
	}
}

func TestMCPSimulatorImageCacheValidatesWithoutGlobalDownloader(t *testing.T) {
	const content = "verified disk"
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()
	dir := t.TempDir()
	ctx := context.Background()
	info := &imageInfo{DownloadURL: server.URL, Checksum: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
	resolve := func() (string, func(), error) {
		return resolveVMImageWith(dir, info,
			func(info *imageInfo) (string, error) { return downloadMCPSimulatorImage(ctx, dir, info.DownloadURL) },
			func(path, digest string) error { return verifyMCPSimulatorImage(ctx, path, digest) })
	}
	for range 2 {
		_, cleanup, err := resolve()
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
	}
	if downloads != 1 {
		t.Fatalf("cache downloaded %d times", downloads)
	}
	info.Checksum = strings.Repeat("0", 64)
	if _, _, err := resolve(); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("accepted bad digest: %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("failed checksum left temporary download: %v %v", files, err)
	}
}

func TestMCPSimulatorCreateRecordsArch(t *testing.T) {
	stubDefaultGuestArch(t, vm.ArchAMD64)
	store := &vm.Store{Root: t.TempDir()}
	image := filepath.Join(t.TempDir(), "image.wic")
	if err := os.WriteFile(image, []byte("disk bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var resolved []vm.Arch
	backend := newSimulatorBackend(func() (*vm.Store, error) { return store, nil }, func(_ context.Context, arch vm.Arch, _ string) (string, string, func(), error) {
		resolved = append(resolved, arch)
		return image, "0.21.0", func() {}, nil
	})
	info, err := backend.Create(context.Background(), wendymcp.SimulatorCreateOptions{Name: "x86", Profile: "g1", Image: image, DiskGiB: 1, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Arch != "amd64" {
		t.Errorf("local image with arch amd64: info.Arch = %q", info.Arch)
	}
	if m, _ := store.ReadMeta("x86"); m.Arch != vm.ArchAMD64 {
		t.Errorf("meta arch = %q, want amd64", m.Arch)
	}
	if _, err := os.Stat(store.VarsPath("x86")); !os.IsNotExist(err) {
		t.Errorf("x86-64 simulator has a variable store at create: %v", err)
	}
	info, err = backend.Create(context.Background(), wendymcp.SimulatorCreateOptions{Name: "local", Profile: "generic", Image: image, DiskGiB: 1})
	if err != nil || info.Arch != "arm64" {
		t.Fatalf("local image without arch: %+v, %v; want arm64", info, err)
	}
	info, err = backend.Create(context.Background(), wendymcp.SimulatorCreateOptions{Name: "published", Profile: "generic", DiskGiB: 1})
	if err != nil || info.Arch != "amd64" || len(resolved) != 1 || resolved[0] != vm.ArchAMD64 {
		t.Fatalf("published image: %+v, %v, resolver saw %v; want the default amd64 throughout", info, err, resolved)
	}
	if err := store.CreateFrom("lite", strings.NewReader("fw"), 2, 1024, vm.Meta{Profile: "wendy-lite"}); err != nil {
		t.Fatal(err)
	}
	items, err := backend.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"x86": "amd64", "local": "arm64", "published": "amd64", "lite": ""}
	for _, item := range items {
		if item.Arch != want[item.Name] {
			t.Errorf("simulator_list %s: arch %q, want %q", item.Name, item.Arch, want[item.Name])
		}
	}
}

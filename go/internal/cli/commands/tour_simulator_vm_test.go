//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func TestEnsureTourVMReusesCompletedVM(t *testing.T) {
	store := robotTestStore(t)
	if err := store.CreateFrom("tour-test", strings.NewReader("existing"), 8, 4096, vm.Meta{}); err != nil {
		t.Fatal(err)
	}
	previous := createTourVMFn
	t.Cleanup(func() { createTourVMFn = previous })
	createTourVMFn = func(context.Context, io.Writer, string) error {
		t.Fatal("attempted to recreate existing VM")
		return nil
	}
	var out bytes.Buffer
	for range 2 {
		if err := ensureTourVM(context.Background(), &out, "tour-test"); err != nil {
			t.Fatal(err)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("existing VM printed setup details: %s", out.String())
	}
	profile, exists, err := store.ReadRobotProfile("tour-test")
	if err != nil || !exists || profile.Kind != vm.RobotKindGo2 {
		t.Fatalf("Go2 profile not retained: %+v, %v, %v", profile, exists, err)
	}
	disk, err := os.ReadFile(store.DiskPath("tour-test"))
	if err != nil || !bytes.HasPrefix(disk, []byte("existing")) {
		t.Fatalf("existing VM disk changed: %v", err)
	}
}

func TestEnsureTourVMResumesInterruptedCreation(t *testing.T) {
	store := robotTestStore(t)
	if err := os.MkdirAll(store.Dir("tour-test"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.DiskPath("tour-test"), []byte("partial image"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := createTourVMFn
	t.Cleanup(func() { createTourVMFn = previous })
	creates := 0
	createTourVMFn = func(ctx context.Context, out io.Writer, name string) error {
		creates++
		_, _ = io.WriteString(out, "Created VM. Start it with wendy vm start.\n")
		return store.CreateFrom(name, strings.NewReader("complete image"), 14, 4096, vm.Meta{})
	}
	var out bytes.Buffer
	if err := ensureTourVM(context.Background(), &out, "tour-test"); err != nil {
		t.Fatal(err)
	}
	if creates != 1 || !strings.Contains(out.String(), "Resuming interrupted setup") || strings.Contains(out.String(), "Created VM") {
		t.Fatalf("creates=%d output=%s", creates, out.String())
	}
	backups, err := filepath.Glob(filepath.Join(store.Root, ".interrupted-tour-test-*", "tour-test", "disk.img"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("missing original disk: %v, %v", backups, err)
	}
	disk, err := os.ReadFile(backups[0])
	if err != nil || string(disk) != "partial image" {
		t.Fatalf("original partial disk changed: %q, %v", disk, err)
	}
	if strings.Contains(out.String(), filepath.Dir(backups[0])) {
		t.Fatalf("successful setup printed internal recovery path: %s", out.String())
	}
	if has, err := tourVMHasGo2Profile(store, "tour-test"); err != nil || !has {
		t.Fatalf("recovery did not attach Go2: %v, %v", has, err)
	}
	if err := ensureTourVM(context.Background(), &out, "tour-test"); err != nil || creates != 1 {
		t.Fatalf("resumed VM not reused: creates=%d, %v", creates, err)
	}
}

func TestEnsureTourVMRejectsAnotherRobotAndHonorsCancellation(t *testing.T) {
	store := robotTestStore(t)
	robotTestProfileForKind(t, store, "tour-test", vm.RobotKindG1)
	previous := createTourVMFn
	t.Cleanup(func() { createTourVMFn = previous })
	createTourVMFn = func(context.Context, io.Writer, string) error { t.Fatal("created VM after refusal"); return nil }
	if err := ensureTourVM(context.Background(), io.Discard, "tour-test"); err == nil || !strings.Contains(err.Error(), "different robot profile") {
		t.Fatalf("accepted G1 as tour Go2: %v", err)
	}
	if _, err := os.Stat(store.Dir("tour-test")); err != nil {
		t.Fatalf("moved another robot's VM: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ensureTourVM(ctx, io.Discard, "new-tour"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestEnsureTourVMDoesNotStartCreationAfterPreparationIsCancelled(t *testing.T) {
	store := robotTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	robotVMStore = func() (*vm.Store, error) {
		cancel()
		return store, nil
	}
	previous := createTourVMFn
	t.Cleanup(func() { createTourVMFn = previous })
	createTourVMFn = func(context.Context, io.Writer, string) error {
		t.Fatal("started creation after cancellation")
		return nil
	}
	if err := ensureTourVM(ctx, io.Discard, "tour-test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation during preparation: %v", err)
	}
}

func TestEnsureTourVMPropagatesCancellationIntoCreation(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	store, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	previousStore, previousTransport := robotVMStore, http.DefaultTransport
	t.Cleanup(func() {
		robotVMStore, http.DefaultTransport = previousStore, previousTransport
	})
	robotVMStore = func() (*vm.Store, error) { return store, nil }
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "tour"))
	defer cancel()
	requests := 0
	http.DefaultTransport = vmImageTestTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Context().Value(contextKey{}) != "tour" {
			t.Error("VM creation dropped the tour context before fetching its manifest")
			return nil, errors.New("missing tour context")
		}
		cancel()
		return nil, req.Context().Err()
	})
	if err := ensureTourVM(ctx, io.Discard, "tour-test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("creation lost in-flight cancellation: %v", err)
	}
	if requests != 1 {
		t.Fatalf("cancelled creation made %d requests, want 1", requests)
	}
	if _, err := os.Stat(store.Dir("tour-test")); !os.IsNotExist(err) {
		t.Fatalf("cancelled creation left a VM directory: %v", err)
	}
}

package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

func useConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	return dir
}

// addTipKey is an Update func that adds one entry to a map field, so a test can
// count how many of many concurrent updates survived.
func addTipKey(key string) func(*Config) (bool, error) {
	return func(c *Config) (bool, error) {
		if c.OptimizeTipShownAt == nil {
			c.OptimizeTipShownAt = map[string]string{}
		}
		c.OptimizeTipShownAt[key] = "2026-09-28"
		return true, nil
	}
}

func TestUpdateSavesTheChangeAndKeepsEverythingElse(t *testing.T) {
	useConfigDir(t)
	if err := Save(&Config{DefaultDevice: "kept.local"}); err != nil {
		t.Fatal(err)
	}
	if err := Update(addTipKey("a")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OptimizeTipShownAt["a"] == "" || cfg.DefaultDevice != "kept.local" {
		t.Fatalf("after Update: tips=%v default=%q; want the new key and the untouched default", cfg.OptimizeTipShownAt, cfg.DefaultDevice)
	}
}

func TestUpdateWithoutAChangeDoesNotWrite(t *testing.T) {
	dir := useConfigDir(t)
	called := false
	if err := Update(func(*Config) (bool, error) { called = true; return false, nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !called {
		t.Fatal("Update never called fn")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("Update with changed=false created config.json (stat err %v)", err)
	}
}

func TestUpdateReturnsFnErrorWithoutSaving(t *testing.T) {
	useConfigDir(t)
	if err := Save(&Config{DefaultDevice: "before.local"}); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	err := Update(func(c *Config) (bool, error) {
		c.DefaultDevice = "after.local"
		return true, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Update = %v, want fn's error", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultDevice != "before.local" {
		t.Fatalf("DefaultDevice = %q: Update saved although fn failed", cfg.DefaultDevice)
	}
}

func TestUpdateKeepsEveryConcurrentChange(t *testing.T) {
	useConfigDir(t)
	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Update(addTipKey(fmt.Sprintf("g%d", i)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.OptimizeTipShownAt); got != writers {
		t.Fatalf("%d of %d concurrent updates survived", got, writers)
	}
}

// Real processes, not goroutines: the lock has to hold across wendy processes,
// which is the case parallel agent sessions hit.
func TestUpdateKeepsEveryChangeAcrossProcesses(t *testing.T) {
	dir := useConfigDir(t)
	const procs, perProc = 6, 15
	var wg sync.WaitGroup
	failures := make(chan string, procs)
	for p := 0; p < procs; p++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"WENDY_CONFIG_DIR="+dir,
			"WENDY_CONFIG_UPDATE_HELPER=1",
			"WENDY_CONFIG_UPDATE_ID="+strconv.Itoa(p),
			"WENDY_CONFIG_UPDATE_N="+strconv.Itoa(perProc),
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := cmd.CombinedOutput(); err != nil {
				failures <- fmt.Sprintf("helper process: %v: %s", err, out)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Fatal(f)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.OptimizeTipShownAt); got != procs*perProc {
		t.Fatalf("%d of %d updates from %d processes survived; Update is not serialised across processes", got, procs*perProc, procs)
	}
}

// TestUpdateHelperProcess is the child side of
// TestUpdateKeepsEveryChangeAcrossProcesses.
func TestUpdateHelperProcess(t *testing.T) {
	if os.Getenv("WENDY_CONFIG_UPDATE_HELPER") != "1" {
		t.Skip("helper process for TestUpdateKeepsEveryChangeAcrossProcesses")
	}
	id := os.Getenv("WENDY_CONFIG_UPDATE_ID")
	n, err := strconv.Atoi(os.Getenv("WENDY_CONFIG_UPDATE_N"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := Update(addTipKey(fmt.Sprintf("p%s-%d", id, i))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateGivesUpWhileAnotherProcessHoldsTheLock(t *testing.T) {
	dir := useConfigDir(t)
	release, err := flock.Acquire(filepath.Join(dir, "config.lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	prev := updateLockTimeout
	updateLockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { updateLockTimeout = prev })

	called := false
	err = Update(func(*Config) (bool, error) { called = true; return true, nil })
	if err == nil || !strings.Contains(err.Error(), "config.lock") {
		t.Fatalf("Update = %v, want an error naming config.lock", err)
	}
	if !errors.Is(err, flock.ErrTimeout) {
		t.Fatalf("Update = %v, want errors.Is(err, flock.ErrTimeout) so callers can tell a timeout from any other lock failure", err)
	}
	if called {
		t.Fatal("Update ran fn without holding the lock")
	}
}

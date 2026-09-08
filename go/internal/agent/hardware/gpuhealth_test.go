package hardware

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A probe on a host with no accelerator must decline to answer rather than
// invent a verdict — the caller reports nothing at all in that case.
func TestProbeGPUDriver_NoAcceleratorDeclines(t *testing.T) {
	if _, err := os.Stat("/dev/kfd"); err == nil {
		t.Skip("host has an AMD compute device")
	}
	if _, err := os.Stat("/etc/nv_tegra_release"); err == nil {
		t.Skip("host is a Jetson")
	}
	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		t.Skip("host has an NVIDIA driver")
	}

	if h, ok := ProbeGPUDriver(context.Background()); ok {
		t.Errorf("ProbeGPUDriver reported %+v on a host with no accelerator; want no answer", h)
	}
}

// openable is the whole point of the probe: a node that stats fine but cannot be
// opened is exactly the state a presence check cannot see.
func TestOpenable_DistinguishesStatFromOpen(t *testing.T) {
	// A path that exists and opens.
	if err := openable(os.DevNull); err != nil {
		t.Errorf("openable(%s) = %v; want nil", os.DevNull, err)
	}

	// A path that does not exist at all.
	if err := openable(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("openable(absent path) = nil; want an error")
	}

	// A path that stats but refuses to open: a directory opened O_RDONLY is
	// permitted, so use an unreadable file, which stat sees and open rejects.
	unreadable := filepath.Join(t.TempDir(), "unreadable")
	if err := os.WriteFile(unreadable, []byte("x"), 0o000); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if _, err := os.Stat(unreadable); err != nil {
		t.Fatalf("fixture should stat: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; mode 0000 does not deny open")
	}
	if err := openable(unreadable); err == nil {
		t.Error("openable(unreadable) = nil; want the open to fail where the stat succeeded")
	}
}

func TestGPUDriverDescription_NamesTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{driverStatusResponding, "responding"},
		{driverStatusNotResponding, "NOT responding"},
		{driverStatusAbsent, "absent"},
	} {
		got := gpuDriverDescription(GPUDriverHealth{Vendor: "nvidia", Status: tc.status})
		if !strings.Contains(got, tc.want) {
			t.Errorf("description for %q = %q; want it to contain %q", tc.status, got, tc.want)
		}
		if !strings.Contains(got, "nvidia") {
			t.Errorf("description for %q = %q; want the vendor named", tc.status, got)
		}
	}
}

// A wedged driver can be verbose; the verdict must stay readable.
func TestTruncate_KeepsDetailBounded(t *testing.T) {
	got := truncate(strings.Repeat("x", 500) + "\nsecond line")
	if len(got) > 210 {
		t.Errorf("len = %d; want it bounded", len(got))
	}
	if strings.Contains(got, "\n") {
		t.Error("detail kept a newline; it goes into a single-line property")
	}
}

func TestGPUProbeLimiter_CancellationBoundsOutstandingWork(t *testing.T) {
	limiter := gpuProbeLimiter{slots: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	result := make(chan GPUDriverHealth, 1)
	go func() {
		result <- limiter.run(ctx, "nvidia", "open", func(context.Context) GPUDriverHealth {
			close(entered)
			<-release
			defer close(finished)
			return GPUDriverHealth{Status: driverStatusResponding}
		})
	}()
	<-entered
	cancel()
	select {
	case h := <-result:
		if h.Status != driverStatusUnknown {
			t.Fatalf("cancelled probe: %+v", h)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked caller")
	}
	h := limiter.run(context.Background(), "nvidia", "open", func(context.Context) GPUDriverHealth { t.Error("started a second worker"); return GPUDriverHealth{} })
	if h.Status != driverStatusUnknown || !strings.Contains(h.Detail, "in progress") {
		t.Fatalf("busy probe: %+v", h)
	}
}

func TestGPUProbeLimiter_Deadline(t *testing.T) {
	limiter := gpuProbeLimiter{slots: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	h := limiter.run(ctx, "nvidia", "query", func(ctx context.Context) GPUDriverHealth {
		<-ctx.Done()
		return GPUDriverHealth{Status: driverStatusUnknown}
	})
	if h.Status != driverStatusUnknown {
		t.Fatalf("deadline: %+v", h)
	}
}

func TestProbeNvidiaSMI_ReportsQueryErrorsWithoutDiagnosingDriver(t *testing.T) {
	for _, tc := range []struct{ name, script, status, detail string }{
		{"success", "printf '8.7\\n8.0\\n'", driverStatusResponding, "CUDA execution not tested"},
		{"mixed", "printf '8.7\\nN/A\\n'", driverStatusUnknown, "incomplete"},
		{"unsupported", "echo 'unsupported query field' >&2; exit 2", driverStatusUnknown, "unsupported query field"},
		{"empty", "exit 0", driverStatusUnknown, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nvidia-smi")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			h := probeNvidiaSMI(context.Background(), path)
			if h.Status != tc.status || !strings.Contains(h.Detail, tc.detail) {
				t.Fatalf("probe: %+v", h)
			}
		})
	}
}

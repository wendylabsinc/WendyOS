//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

const jobTestPassword = "hunter2-job-secret"

func runInstallJobsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newOSInstallJobsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func stubInstallJobPlan(t *testing.T) {
	t.Helper()
	prev := installationJobHooksFn
	t.Cleanup(func() { installationJobHooksFn = prev })
	installationJobHooksFn = func() onboarding.JobHooks {
		return onboarding.JobHooks{Plan: func(_ context.Context, o onboarding.Options) (*onboarding.Plan, error) {
			return &onboarding.Plan{DeviceType: o.DeviceType, Method: "removable-media", Version: "1.2.3", Storage: "sd", ArtifactURL: "https://example.invalid/image.zst", ArtifactSHA256: strings.Repeat("a", 64), Target: &onboarding.Drive{ID: o.Drive, Name: "Test media", Capacity: 1 << 34, Removable: true}}, nil
		}}
	}
}

func TestInstallJobStartCarriesFirstBootChoicesWithoutEchoingPasswords(t *testing.T) {
	stubInstallJobPlan(t)
	root := t.TempDir()
	out, err := runInstallJobsCmd(t, "start", "--job-root", root, "--device-type", "raspberry-pi-5", "--drive", "/dev/test",
		"--device-name", "nano-lab", "--wifi", "ssid=Home,password="+jobTestPassword+",priority=5", "--wifi", "ssid=Cafe")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, jobTestPassword) {
		t.Fatalf("start output leaks the password: %s", out)
	}
	var j onboarding.Job
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		t.Fatal(err)
	}
	if j.Provisioning == nil || j.Provisioning.DeviceName != "nano-lab" || strings.Join(j.Provisioning.WiFi, ",") != "Home,Cafe" {
		t.Fatalf("summary %+v", j.Provisioning)
	}
	// The worker gets the full input back from the store.
	p, err := onboarding.JobStore{Root: root}.ReadProvisioning(&j)
	if err != nil {
		t.Fatal(err)
	}
	fb := newInstallJobFirstBoot(p)
	want := []wendyconf.WifiCredential{{SSID: "Home", Password: jobTestPassword, Priority: 5}, {SSID: "Cafe"}}
	if fb.name != "nano-lab" || len(fb.creds) != 2 || fb.creds[0] != want[0] || fb.creds[1] != want[1] {
		t.Fatalf("worker input %+v", fb)
	}
	status, err := runInstallJobsCmd(t, "status", "--job-root", root, "--job-id", j.ID)
	if err != nil || strings.Contains(status, jobTestPassword) {
		t.Fatalf("status: %v %s", err, status)
	}

	out, err = runInstallJobsCmd(t, "start", "--job-root", root, "--device-type", "raspberry-pi-5", "--drive", "/dev/test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"provisioning":{"auto_name":true,"no_wifi":true}`) {
		t.Fatalf("absent input is not stated as no Wi-Fi and an auto name: %s", out)
	}
}

func TestInstallJobStartRejectsBadFirstBootInputBeforePlanning(t *testing.T) {
	prev := installationJobHooksFn
	t.Cleanup(func() { installationJobHooksFn = prev })
	installationJobHooksFn = func() onboarding.JobHooks {
		return onboarding.JobHooks{Plan: func(context.Context, onboarding.Options) (*onboarding.Plan, error) {
			t.Fatal("planned before validating first-boot input")
			return nil, nil
		}}
	}
	for name, args := range map[string][]string{
		"no-wifi with wifi": {"--no-wifi", "--wifi", "ssid=Home,password=" + jobTestPassword},
		"malformed entry":   {"--wifi", "ssid=Home," + jobTestPassword},
		"unknown key":       {"--wifi", "ssid=Home,pasword=" + jobTestPassword},
		"no ssid":           {"--wifi", "password=" + jobTestPassword},
		"bad name":          {"--device-name", "Nano Lab"},
		"newline password":  {"--wifi", "ssid=Home,password=" + jobTestPassword + "\n[device]"},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "jobs")
			out, err := runInstallJobsCmd(t, append([]string{"start", "--job-root", root, "--device-type", "raspberry-pi-5", "--drive", "/dev/test"}, args...)...)
			if err == nil {
				t.Fatal("accepted bad first-boot input")
			}
			if strings.Contains(err.Error(), jobTestPassword) || strings.Contains(out, jobTestPassword) {
				t.Fatalf("error leaks the password: %v %s", err, out)
			}
			if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("created a job for bad input")
			}
		})
	}
}

// installJobMediaStub records what reached the disk work after the image write.
type installJobMediaStub struct {
	provisioned, marked, ejected int
	creds                        []wendyconf.WifiCredential
	name                         string
}

// stubInstallJobMedia replaces that disk work with failures chosen by the test.
func stubInstallJobMedia(t *testing.T, provisionErr, ejectErr error, present bool) *installJobMediaStub {
	t.Helper()
	stub := &installJobMediaStub{}
	withStubbedProvisioning(t, func(_ drive, creds []wendyconf.WifiCredential, name string, _ []byte) error {
		stub.provisioned++
		stub.creds, stub.name = creds, name
		return provisionErr
	}, false, nil)
	drivePresentFn = func(drive) bool { return present }
	prevMark, prevEject := installJobMarkFATVolumes, installJobEjectDisk
	t.Cleanup(func() { installJobMarkFATVolumes, installJobEjectDisk = prevMark, prevEject })
	installJobMarkFATVolumes = func(drive) { stub.marked++ }
	installJobEjectDisk = func(drive) error { stub.ejected++; return ejectErr }
	return stub
}

func noCheckpoint(string, bool) error { return nil }

func TestInstallJobMediaProvisionsAfterTheWrite(t *testing.T) {
	target := drive{DevicePath: "/dev/test", Name: "Test media", IsRemovable: true}
	fb := installJobFirstBoot{name: "nano-lab", creds: []wendyconf.WifiCredential{{SSID: "Home", Password: jobTestPassword}}}

	t.Run("requested and written", func(t *testing.T) {
		stub := stubInstallJobMedia(t, nil, nil, true)
		phases := []string{}
		err := provisionInstallJobMedia(context.Background(), target, fb, func(phase string, beginsWrite bool) error {
			if beginsWrite {
				t.Fatal("provisioning moved the write boundary")
			}
			phases = append(phases, phase)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.provisioned != 1 || stub.name != "nano-lab" || len(stub.creds) != 1 || stub.creds[0].Password != jobTestPassword {
			t.Fatalf("provisioning got %+v", stub)
		}
		if stub.marked != 1 || stub.ejected != 1 || len(phases) != 1 {
			t.Fatalf("release %+v phases %v", stub, phases)
		}
	})
	t.Run("nothing requested writes only the image", func(t *testing.T) {
		stub := stubInstallJobMedia(t, nil, nil, true)
		err := provisionInstallJobMedia(context.Background(), target, installJobFirstBoot{}, func(phase string, _ bool) error {
			t.Fatalf("entered phase %q without first-boot input", phase)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if stub.provisioned != 0 || stub.marked != 0 || stub.ejected != 0 {
			t.Fatalf("a job without first-boot input touched the media after the image: %+v", stub)
		}
	})
	t.Run("requested and failed", func(t *testing.T) {
		stub := stubInstallJobMedia(t, errors.New("config partition not found"), nil, true)
		err := provisionInstallJobMedia(context.Background(), target, fb, noCheckpoint)
		var provErr *onboarding.ProvisioningError
		if !errors.As(err, &provErr) || provErr.Configured || !strings.Contains(err.Error(), "config partition not found") {
			t.Fatalf("err %v", err)
		}
		if stub.ejected != 1 {
			t.Fatal("left the media mounted after a failed configuration")
		}
	})
	t.Run("eject failure fails the job clearly", func(t *testing.T) {
		stubInstallJobMedia(t, nil, errors.New("disk4 is still mounted: eject it in Finder before removing it"), true)
		err := provisionInstallJobMedia(context.Background(), target, fb, noCheckpoint)
		var provErr *onboarding.ProvisioningError
		if !errors.As(err, &provErr) || !provErr.Configured || !strings.Contains(err.Error(), "eject it in Finder") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("written but still mounted is configured", func(t *testing.T) {
		stub := stubInstallJobMedia(t, &configStillMountedError{dir: "/tmp/wendyos-config-1", cause: errExit1}, nil, true)
		err := provisionInstallJobMedia(context.Background(), target, fb, noCheckpoint)
		var provErr *onboarding.ProvisioningError
		if !errors.As(err, &provErr) || !provErr.Configured || !strings.Contains(err.Error(), "sudo umount /tmp/wendyos-config-1") || stub.provisioned != 1 {
			t.Fatalf("err %v stub %+v", err, stub)
		}
	})
	t.Run("disconnected drive is never ejected", func(t *testing.T) {
		stub := stubInstallJobMedia(t, errors.New("no such device or address"), nil, false)
		err := provisionInstallJobMedia(context.Background(), target, fb, noCheckpoint)
		var provErr *onboarding.ProvisioningError
		if !errors.As(err, &provErr) || stub.marked != 0 || stub.ejected != 0 {
			t.Fatalf("err %v stub %+v", err, stub)
		}
	})
	t.Run("checkpoint failure", func(t *testing.T) {
		stub := stubInstallJobMedia(t, nil, nil, true)
		err := provisionInstallJobMedia(context.Background(), target, fb, func(string, bool) error { return errors.New("job store unwritable") })
		var provErr *onboarding.ProvisioningError
		if !errors.As(err, &provErr) || stub.provisioned != 0 {
			t.Fatalf("err %v stub %+v", err, stub)
		}
	})
}

type configInjector = func(string, []wendyconf.WifiCredential, string, []byte, io.Writer, func(string), *localInstallAgent) error

// withLocalAgent runs inject with a local agent so no release is downloaded.
func withLocalAgent(inject configInjector) configInjector {
	agent := &localInstallAgent{path: "test-agent", data: installTestELF(elf.EM_AARCH64), arch: "arm64"}
	return func(img string, creds []wendyconf.WifiCredential, name string, prov []byte, out io.Writer, detail func(string), _ *localInstallAgent) error {
		return inject(img, creds, name, prov, out, detail, agent)
	}
}

func copyTestFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFirstBootSeeded(t *testing.T, img string) {
	t.Helper()
	conf, err := readFATFile(t, img, "wendy.conf")
	if err != nil {
		t.Fatalf("wendy.conf: %v", err)
	}
	for _, want := range []string{"ssid = Home", "password = " + jobTestPassword, "name = nano-lab"} {
		if !strings.Contains(string(conf), want) {
			t.Fatalf("wendy.conf lacks %q:\n%s", want, conf)
		}
	}
	if b, err := readFATFile(t, img, "clock_floor"); err != nil || len(b) != 8 {
		t.Fatalf("clock_floor %d %v", len(b), err)
	}
	if b, err := readFATFile(t, img, "wendy-agent"); err != nil || len(b) == 0 {
		t.Fatalf("agent seed %d %v", len(b), err)
	}
}

func TestInstallJobRecoveryWorkspacesCarryFirstBootInput(t *testing.T) {
	fb := installJobFirstBoot{name: "nano-lab", creds: []wendyconf.WifiCredential{{SSID: "Home", Password: jobTestPassword}}}
	prevOrin, prevThor := installJobInjectOrinConfig, installJobInjectThorConfig
	t.Cleanup(func() { installJobInjectOrinConfig, installJobInjectThorConfig = prevOrin, prevThor })
	installJobInjectOrinConfig, installJobInjectThorConfig = withLocalAgent(injectRecoveryConfig), withLocalAgent(injectConfigPartition)

	t.Run("Orin", func(t *testing.T) {
		root := t.TempDir()
		cached := filepath.Join(root, "stage2", "flash", "config.img")
		copyTestFile(t, newFATImage(t), cached)
		if err := os.WriteFile(filepath.Join(root, "stage2", "flash", "layout.xml"), []byte("layout"), 0o644); err != nil {
			t.Fatal(err)
		}
		fp := &flashpack.Flashpack{Root: root}
		fp.Manifest.Layout.FlashImages = "stage2/flash"
		fp.Manifest.Layout.ConfigImage = "stage2/flash/config.img"
		fp.Manifest.Layout.PartitionLayout = "stage2/flash/layout.xml"
		workspace, layout, err := prepareOrinJobWorkspace(fp, fb, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(workspace)
		if layout != filepath.Join(workspace, "layout.xml") {
			t.Fatalf("layout %s", layout)
		}
		assertFirstBootSeeded(t, filepath.Join(workspace, "config.img"))
		if _, err := readFATFile(t, cached, "wendy.conf"); err == nil {
			t.Fatal("wrote first-boot input into the cached flashpack")
		}
	})
	t.Run("Thor", func(t *testing.T) {
		root := t.TempDir()
		cached := filepath.Join(root, "ws", "flash-images", "config-partition.fat32.img")
		copyTestFile(t, newFATImage(t), cached)
		fp := &flashpack.Flashpack{Root: root}
		fp.Manifest.Layout.FlashWorkspace = "ws"
		workspace, err := prepareThorJobWorkspace(fp, fb, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(workspace)
		assertFirstBootSeeded(t, filepath.Join(workspace, "config-partition.fat32.img"))
		if _, err := readFATFile(t, cached, "wendy.conf"); err == nil {
			t.Fatal("wrote first-boot input into the cached flashpack")
		}
	})
	t.Run("without input the published config image is flashed", func(t *testing.T) {
		installJobInjectOrinConfig = func(string, []wendyconf.WifiCredential, string, []byte, io.Writer, func(string), *localInstallAgent) error {
			t.Fatal("injected into the Orin config image without first-boot input")
			return nil
		}
		installJobInjectThorConfig = func(string, []wendyconf.WifiCredential, string, []byte, io.Writer, func(string), *localInstallAgent) error {
			t.Fatal("injected into the Thor config image without first-boot input")
			return nil
		}
		root := t.TempDir()
		orinCached := filepath.Join(root, "stage2", "flash", "config.img")
		copyTestFile(t, newFATImage(t), orinCached)
		if err := os.WriteFile(filepath.Join(root, "stage2", "flash", "layout.xml"), []byte("layout"), 0o644); err != nil {
			t.Fatal(err)
		}
		thorCached := filepath.Join(root, "ws", "flash-images", "config-partition.fat32.img")
		copyTestFile(t, newFATImage(t), thorCached)
		fp := &flashpack.Flashpack{Root: root}
		fp.Manifest.Layout.FlashImages = "stage2/flash"
		fp.Manifest.Layout.ConfigImage = "stage2/flash/config.img"
		fp.Manifest.Layout.PartitionLayout = "stage2/flash/layout.xml"
		fp.Manifest.Layout.FlashWorkspace = "ws"

		orin, _, err := prepareOrinJobWorkspace(fp, installJobFirstBoot{}, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(orin)
		thor, err := prepareThorJobWorkspace(fp, installJobFirstBoot{}, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(thor)
		for cached, flashed := range map[string]string{orinCached: filepath.Join(orin, "config.img"), thorCached: filepath.Join(thor, "config-partition.fat32.img")} {
			want, err := os.ReadFile(cached)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(flashed); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%s differs from the published config image (%v)", flashed, err)
			}
		}
	})
	t.Run("injection failure removes the workspace", func(t *testing.T) {
		root := t.TempDir()
		copyTestFile(t, newFATImage(t), filepath.Join(root, "ws", "flash-images", "config-partition.fat32.img"))
		fp := &flashpack.Flashpack{Root: root}
		fp.Manifest.Layout.FlashWorkspace = "ws"
		installJobInjectThorConfig = func(string, []wendyconf.WifiCredential, string, []byte, io.Writer, func(string), *localInstallAgent) error {
			return errors.New("config image is full")
		}
		if _, err := prepareThorJobWorkspace(fp, fb, func(string) {}); err == nil || !strings.Contains(err.Error(), "config image is full") {
			t.Fatalf("err %v", err)
		}
		leftovers, _ := filepath.Glob(filepath.Join(root, "ws", ".wendy-recovery-run-*"))
		if len(leftovers) != 0 {
			t.Fatalf("workspace left behind: %v", leftovers)
		}
	})
}

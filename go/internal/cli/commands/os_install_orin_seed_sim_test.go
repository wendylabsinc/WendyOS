//go:build darwin || linux

package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/diskfs/go-diskfs/partition/gpt"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// TestOrinFullInstallSeedSim rehearses the host half of a Jetson Orin full
// (USB-C recovery) install with no board attached: the real flashpack,
// workspace, config injection and partition writer, aimed at a sparse file
// that stands in for the NVMe. Run 2 onward starts from a disk whose config
// partition the previous "install" left as ext4, as first boot does.
//
//	WENDY_ORIN_SEED_SIM_DIR=<out> go test ./internal/cli/commands \
//	    -run TestOrinFullInstallSeedSim -v -timeout 3h
func TestOrinFullInstallSeedSim(t *testing.T) {
	outDir := os.Getenv("WENDY_ORIN_SEED_SIM_DIR")
	if outDir == "" {
		t.Skip("set WENDY_ORIN_SEED_SIM_DIR to run the Orin full-install seed rehearsal (downloads a multi-GB flashpack)")
	}
	device := envOr("WENDY_ORIN_SEED_SIM_DEVICE", "jetson-orin-nano")
	storage := envOr("WENDY_ORIN_SEED_SIM_STORAGE", "nvme")
	runs, _ := strconv.Atoi(envOr("WENDY_ORIN_SEED_SIM_RUNS", "3"))
	diskBytes := int64(128) << 30

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dm, ver, err := resolveDeviceArtifact(device, os.Getenv("WENDY_ORIN_SEED_SIM_VERSION"), os.Getenv("WENDY_ORIN_SEED_SIM_NIGHTLY") == "1", 0)
	if err != nil {
		t.Fatalf("resolving %s: %v", device, err)
	}
	info, err := getRecoveryFlashpackInfo(dm, device, ver, storage)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir, err := osCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	fp, cached, err := resolveT234Flashpack(cacheDir, flashpack.RecoveryRef{Device: device, Storage: storage, Version: ver}, info, func(s string) {})
	if err != nil {
		t.Fatalf("flashpack: %v", err)
	}
	t.Logf("flashpack %s %s %s cached=%v (%s)", device, storage, ver, cached, time.Since(start).Round(time.Second))

	diskPath := filepath.Join(outDir, storage+".img")
	if err := os.Remove(diskPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if f, err := os.Create(diskPath); err != nil {
		t.Fatal(err)
	} else if err := f.Truncate(diskBytes); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}

	for i := 1; i <= runs; i++ {
		name := fmt.Sprintf("seed-sim-%d", i)
		creds := []wendyconf.WifiCredential{{SSID: fmt.Sprintf("Seed Sim %d", i), Password: fmt.Sprintf("pw-%d-%d", i, time.Now().UnixNano())}}

		if i > 1 {
			if err := convertConfigToExt4(diskPath); err != nil {
				t.Fatalf("run %d: simulating the previous first boot: %v", i, err)
			}
		}

		ws, layout, err := prepareT234Workspace(fp)
		if err != nil {
			t.Fatalf("run %d: workspace: %v", i, err)
		}
		configRel, err := filepath.Rel(fp.FlashImagesDir(), fp.ConfigImage())
		if err != nil {
			t.Fatal(err)
		}
		injected := filepath.Join(ws, configRel)
		var log strings.Builder
		if err := injectRecoveryConfig(injected, creds, name, nil, &log, func(string) {}, nil); err != nil {
			t.Fatalf("run %d: inject: %v\n%s", i, err, log.String())
		}
		if s := strings.TrimSpace(log.String()); s != "" {
			t.Logf("run %d: inject said: %s", i, s)
		}
		injCopy := filepath.Join(outDir, fmt.Sprintf("run%d-injected-config.img", i))
		if err := copyRegularFileOver(injected, injCopy); err != nil {
			t.Fatal(err)
		}

		start = time.Now()
		werr := t234.RunWriter(t234.WriterOptions{Device: diskPath, WritePlan: true, LayoutPath: layout, ImagesDir: ws, RootfsDevice: fp.Manifest.RootfsDevice})
		os.RemoveAll(ws) //nolint:errcheck
		if werr != nil {
			t.Fatalf("run %d: write plan: %v", i, werr)
		}

		flashed := filepath.Join(outDir, fmt.Sprintf("run%d-flashed-config.img", i))
		part, err := extractPartition(diskPath, "config", flashed)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		imgInfo, _ := os.Stat(injCopy)
		t.Logf("run %d: wrote in %s; config image %d bytes, partition %d bytes at LBA %d",
			i, time.Since(start).Round(time.Second), imgInfo.Size(), part.Size, part.Start)

		for _, stage := range []struct{ label, img string }{{"injected", injCopy}, {"flashed", flashed}} {
			conf, err := readFATFile(t, stage.img, "wendy.conf")
			if err != nil {
				t.Errorf("run %d %s: reading wendy.conf: %v", i, stage.label, err)
				continue
			}
			if err := verifySeed(conf, name, creds); err != nil {
				t.Errorf("run %d %s: %v\n--- wendy.conf ---\n%s", i, stage.label, err, conf)
			}
		}
		if magic, err := ext4MagicAt(flashed); err != nil {
			t.Errorf("run %d: %v", i, err)
		} else if magic {
			t.Errorf("run %d: flashed config partition still carries an ext4 superblock", i)
		}
	}
}

// verifySeed reports whether wendy.conf, as the agent would read it on first
// boot, still carries the device name and every Wi-Fi network this install
// asked for. parseSeedINI below parses exactly the way the agent does.
func verifySeed(conf []byte, wantName string, wantCreds []wendyconf.WifiCredential) error {
	sections := parseSeedINI(conf)
	var problems []string
	if got := sections["device"]["name"]; got != wantName {
		problems = append(problems, fmt.Sprintf("device name is %q, want %q", got, wantName))
	}
	seeded := map[string]wendyconf.WifiCredential{}
	for _, c := range wendyconf.UnmarshalWiFi(sections) {
		if _, dup := seeded[c.SSID]; dup {
			problems = append(problems, fmt.Sprintf("network %q is seeded more than once", c.SSID))
		}
		seeded[c.SSID] = c
	}
	for _, want := range wantCreds {
		got, ok := seeded[want.SSID]
		delete(seeded, want.SSID)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("network %q is missing", want.SSID))
		case got.Password != want.Password:
			// Never print a password, even a test one.
			problems = append(problems, fmt.Sprintf("network %q has a different password", want.SSID))
		case got.Priority != want.Priority || got.Hidden != want.Hidden || !strings.EqualFold(got.Security, want.Security):
			problems = append(problems, fmt.Sprintf("network %q is priority %d, hidden %v, security %q; want %d, %v, %q",
				want.SSID, got.Priority, got.Hidden, got.Security, want.Priority, want.Hidden, want.Security))
		}
	}
	extra := make([]string, 0, len(seeded))
	for ssid := range seeded {
		extra = append(extra, fmt.Sprintf("network %q was not requested", ssid))
	}
	sort.Strings(extra)
	if problems = append(problems, extra...); len(problems) > 0 {
		return errors.New("the agent would not apply this seed: " + strings.Join(problems, "; "))
	}
	return nil
}

// parseSeedINI mirrors the agent's configpartition.parseINI: sections and
// key = value pairs, comments and keys outside a section ignored, last key wins.
func parseSeedINI(data []byte) map[string]map[string]string {
	out := map[string]map[string]string{}
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			if out[section] == nil {
				out[section] = map[string]string{}
			}
		case section != "":
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) != "" {
				out[section][strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func copyRegularFileOver(src, dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyRegularFile(src, dst, 0o644)
}

func findGPTPartition(diskPath, name string) (*gpt.Partition, error) {
	d, err := diskfs.Open(diskPath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	pt, err := d.GetPartitionTable()
	if err != nil {
		return nil, fmt.Errorf("reading GPT: %w", err)
	}
	table, ok := pt.(*gpt.Table)
	if !ok {
		return nil, fmt.Errorf("partition table is %T, not GPT", pt)
	}
	for _, p := range table.Partitions {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no %q partition in the GPT", name)
}

// extractPartition copies one partition of diskPath into its own image file.
func extractPartition(diskPath, name, dst string) (*gpt.Partition, error) {
	p, err := findGPTPartition(diskPath, name)
	if err != nil {
		return nil, err
	}
	src, err := os.Open(diskPath)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.NewSectionReader(src, int64(p.Start)*512, int64(p.Size))); err != nil {
		return nil, err
	}
	return p, out.Close()
}

// convertConfigToExt4 leaves the disk the way a first boot does: the config
// partition reformatted as ext4 (wendyos-config-convert.sh).
func convertConfigToExt4(diskPath string) error {
	p, err := findGPTPartition(diskPath, "config")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(diskPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fs, err := ext4.Create(backendfile.New(f, false), int64(p.Size), int64(p.Start)*512, 512, &ext4.Params{VolumeName: "config"})
	if err != nil {
		return err
	}
	if err := fs.Close(); err != nil {
		return err
	}
	if magic, err := ext4MagicAtOffset(f, int64(p.Start)*512); err != nil || !magic {
		return fmt.Errorf("ext4 superblock missing after the simulated conversion (err=%v)", err)
	}
	return f.Sync()
}

func ext4MagicAt(img string) (bool, error) {
	f, err := os.Open(img)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return ext4MagicAtOffset(f, 0)
}

// ext4MagicAtOffset checks s_magic (0xEF53) of the primary superblock.
func ext4MagicAtOffset(r io.ReaderAt, partOffset int64) (bool, error) {
	b := make([]byte, 2)
	if _, err := r.ReadAt(b, partOffset+1024+0x38); err != nil {
		return false, err
	}
	return b[0] == 0x53 && b[1] == 0xEF, nil
}

// memTarget collects config-partition files the way the installer writes them.
type memTarget map[string][]byte

func (m memTarget) WriteFile(name string, data []byte, _ os.FileMode) error {
	m[name] = data
	return nil
}

func TestVerifySeed(t *testing.T) {
	home := wendyconf.WifiCredential{SSID: "Home Net", Password: "home secret"}
	office := wendyconf.WifiCredential{SSID: "Office", Password: "office-secret", Priority: 5, Hidden: true, Security: "wpa3"}
	written := func(name string, creds ...wendyconf.WifiCredential) []byte {
		t.Helper()
		files := memTarget{}
		if err := writeConfigFilesTo(files, nil, creds, name, nil); err != nil {
			t.Fatal(err)
		}
		return files["wendy.conf"]
	}
	cases := []struct {
		name      string
		conf      []byte
		wantName  string
		wantCreds []wendyconf.WifiCredential
		wantErr   string
	}{
		{"what the installer writes", written("lab-nano", home, office), "lab-nano", []wendyconf.WifiCredential{home, office}, ""},
		{"name only", written("lab-nano"), "lab-nano", nil, ""},
		{"wrong name", written("lab-nano", home), "lab-orin", []wendyconf.WifiCredential{home}, `device name is "lab-nano", want "lab-orin"`},
		{"name lost", written("", home), "lab-nano", []wendyconf.WifiCredential{home}, `device name is "", want "lab-nano"`},
		{"network lost", written("lab-nano", home), "lab-nano", []wendyconf.WifiCredential{home, office}, `network "Office" is missing`},
		{"stale network", written("lab-nano", home, office), "lab-nano", []wendyconf.WifiCredential{home}, `network "Office" was not requested`},
		{"stale password", written("lab-nano", wendyconf.WifiCredential{SSID: "Home Net", Password: "old secret"}), "lab-nano",
			[]wendyconf.WifiCredential{home}, `network "Home Net" has a different password`},
		{"settings changed", written("lab-nano", wendyconf.WifiCredential{SSID: "Office", Password: "office-secret"}), "lab-nano",
			[]wendyconf.WifiCredential{office}, `network "Office" is priority 0`},
		{"seeded twice", []byte("[wifi]\nssid = Home Net\npassword = home secret\n\n[wifi.2]\nssid = Home Net\npassword = home secret\n\n[device]\nname = lab-nano\n"),
			"lab-nano", []wendyconf.WifiCredential{home}, `network "Home Net" is seeded more than once`},
		{"empty file", nil, "lab-nano", []wendyconf.WifiCredential{home}, `network "Home Net" is missing`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifySeed(c.conf, c.wantName, c.wantCreds)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("verifySeed = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("verifySeed = %v; want it to say %q", err, c.wantErr)
			}
			for _, secret := range []string{"home secret", "office-secret", "old secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("verifySeed printed a password: %v", err)
				}
			}
		})
	}
}

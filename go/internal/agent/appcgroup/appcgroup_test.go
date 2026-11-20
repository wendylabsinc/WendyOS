package appcgroup

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestDefaultReserve(t *testing.T) {
	for _, tt := range []struct {
		name         string
		total, wantR uint64
	}{
		{"400 MiB caps at half", 400 * mib, 200 * mib},
		{"512 MiB caps at half", 512 * mib, 256 * mib},
		{"900 MiB caps at half", 900 * mib, 450 * mib},
		{"2 GiB", 2048 * mib, 830 * mib},
		{"8 GiB", 8192 * mib, 1014 * mib},
		{"128 GiB", 131072 * mib, 4701 * mib},
		{"Arduino UNO Q", 3789272 * 1024, 880 * mib},
		{"Jetson Thor", 128766784 * 1024, 4541 * mib},
	} {
		got := DefaultReserve(tt.total)
		if got != tt.wantR {
			t.Errorf("%s: DefaultReserve = %d MiB, want %d MiB", tt.name, got/mib, tt.wantR/mib)
		}
		if got%mib != 0 {
			t.Errorf("%s: reserve %d is not a whole MiB", tt.name, got)
		}
		if tt.total%4096 == 0 && (tt.total-got)%4096 != 0 {
			t.Errorf("%s: memory.max %d is not page aligned", tt.name, tt.total-got)
		}
	}
	const thor uint64 = 128766784 * 1024
	if got, want := DefaultReserve(thor), (768*mib+thor*3/100+mib-1)/mib*mib; got != want {
		t.Errorf("Thor reserve = %d, want ceil formula %d", got, want)
	}
}

func TestReserveOverride(t *testing.T) {
	const total = 8 * gib
	for _, tt := range []struct {
		override string
		want     uint64
	}{
		{"768Mi", 768 * mib},
		{" 1Gi\n", gib},
		{"off", 0},
		{" off\n", 0},
		{"", 1014 * mib},
		{"7936Mi", 7936 * mib}, // leaves exactly minAppMemory
		{"256Mi", 256 * mib},   // exactly minReserve
	} {
		got, err := Reserve(total, tt.override)
		if err != nil || got != tt.want {
			t.Errorf("Reserve(%q) = %d, %v; want %d", tt.override, got, err, tt.want)
		}
	}
	for _, bad := range []string{"abc", "0", "-1", "768", "255Mi", "7937Mi", "8Gi", "9Gi", "99999999999999Ti"} {
		if got, err := Reserve(total, bad); err == nil {
			t.Errorf("Reserve(%q) = %d, want an error", bad, got)
		}
	}
	if got, err := Reserve(400*mib, ""); err == nil {
		t.Errorf("default reserve on a 400 MiB host = %d, want an error", got)
	}
}

func TestHostMemoryTotal(t *testing.T) {
	total, err := hostMemoryTotal([]byte("MemFree: 1 kB\nMemTotal:       8000000 kB\n"))
	if err != nil || total != 8000000*1024 {
		t.Fatalf("total %d error %v", total, err)
	}
	for _, invalid := range []string{"", "MemTotal: 0 kB", "MemTotal: -1 kB", "MemTotal: 5 MB", "MemTotal: 18446744073709551615 kB"} {
		if _, err := hostMemoryTotal([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid memory total %q", invalid)
		}
	}
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fakeRoot mimics a cgroup v2 root. withApps pre-creates wendy-apps with its
// control files, as the kernel does on mkdir; Configure never creates them.
func fakeRoot(t *testing.T, controllers string, withApps bool) string {
	t.Helper()
	root := t.TempDir()
	put(t, filepath.Join(root, "cgroup.controllers"), controllers)
	put(t, filepath.Join(root, "cgroup.subtree_control"), "cpu\n")
	if !withApps {
		return root
	}
	apps := filepath.Join(root, Parent)
	if err := os.Mkdir(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"cgroup.controllers":     "cpu memory pids\n",
		"cgroup.subtree_control": "\n",
		"memory.max":             "max\n",
		"memory.high":            "5798205850\n",
		"memory.swap.max":        "0\n",
	} {
		put(t, filepath.Join(apps, name), value)
	}
	return root
}

// snapshot records every file and directory under root, to prove no writes.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			files[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		files[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestConfigureWritesOnlyTheHardLimit(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids\n", true)
	apps := filepath.Join(root, Parent)
	swapBefore := get(t, filepath.Join(apps, "memory.swap.max"))

	st := Configure(root, 8*gib, gib)
	want := Status{Unified: true, Limited: true, Reserve: gib, Max: 7 * gib}
	if st != want {
		t.Fatalf("status = %+v, want %+v", st, want)
	}
	if got := get(t, filepath.Join(apps, "memory.max")); got != strconv.FormatUint(7*gib, 10) {
		t.Errorf("memory.max = %q", got)
	}
	if got := get(t, filepath.Join(apps, "memory.high")); got != "max" {
		t.Errorf("memory.high = %q, want max", got)
	}
	if got := get(t, filepath.Join(apps, "memory.swap.max")); got != swapBefore {
		t.Errorf("memory.swap.max changed from %q to %q", swapBefore, got)
	}
	if got := get(t, filepath.Join(root, "cgroup.subtree_control")); got != "+memory" {
		t.Errorf("root cgroup.subtree_control = %q, want +memory", got)
	}
	if got := get(t, filepath.Join(apps, "cgroup.subtree_control")); got != "+memory" {
		t.Errorf("wendy-apps cgroup.subtree_control = %q, want +memory", got)
	}
	if again := Configure(root, 8*gib, gib); again != want {
		t.Fatalf("second Configure = %+v, want %+v", again, want)
	}
}

func TestConfigureLeavesUnchangedValuesAlone(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids\n", true)
	apps := filepath.Join(root, Parent)
	put(t, filepath.Join(root, "cgroup.subtree_control"), "cpu memory\n")
	put(t, filepath.Join(apps, "cgroup.subtree_control"), "memory\n")
	put(t, filepath.Join(apps, "memory.max"), strconv.FormatUint(7*gib, 10)+"\n")
	put(t, filepath.Join(apps, "memory.high"), "max\n")
	for _, name := range []string{"cgroup.subtree_control", Parent + "/cgroup.subtree_control", Parent + "/memory.max", Parent + "/memory.high"} {
		if err := os.Chmod(filepath.Join(root, name), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot(t, root)

	st := Configure(root, 8*gib, gib)
	if st.Err != nil || !st.Limited {
		t.Fatalf("status = %+v", st)
	}
	if after := snapshot(t, root); !maps.Equal(before, after) {
		t.Fatalf("files changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestConfigureWithoutMemoryControllerIsNotAnError(t *testing.T) {
	root := fakeRoot(t, "cpu io pids\n", false)
	before := snapshot(t, root)

	st := Configure(root, 8*gib, gib)
	want := Status{Unified: true, Note: "memory controller unavailable"}
	if st != want {
		t.Fatalf("status = %+v, want %+v", st, want)
	}
	if after := snapshot(t, root); !maps.Equal(before, after) {
		t.Fatalf("files changed:\nbefore %v\nafter  %v", before, after)
	}

	// The agent's path: a bad override or unreadable meminfo must not matter.
	configDir := t.TempDir()
	put(t, filepath.Join(configDir, OverrideFile), "abc")
	useFakeHost(t, root, filepath.Join(t.TempDir(), "missing-meminfo"))
	if st := Ensure(configDir); st != want {
		t.Fatalf("Ensure = %+v, want %+v", st, want)
	}
	if after := snapshot(t, root); !maps.Equal(before, after) {
		t.Fatalf("Ensure changed files:\nbefore %v\nafter  %v", before, after)
	}
}

func TestConfigureOnCgroupV1(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	st := Configure(root, 8*gib, gib)
	if st.Unified || st.Limited || st.Err != nil {
		t.Fatalf("status = %+v", st)
	}
	if after := snapshot(t, root); !maps.Equal(before, after) {
		t.Fatalf("files changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestReserveOffRemovesTheLimit(t *testing.T) {
	root := fakeRoot(t, "cpu memory pids\n", true)
	maxPath := filepath.Join(root, Parent, "memory.max")
	put(t, maxPath, strconv.FormatUint(7*gib, 10)+"\n")

	st := Configure(root, 8*gib, 0)
	if st.Err != nil || st.Limited || !st.Unified || st.Max != 0 {
		t.Fatalf("status = %+v", st)
	}
	if got := get(t, maxPath); got != "max" {
		t.Fatalf("memory.max = %q, want max", got)
	}
}

func TestConfigureDoesNotCreateMissingControlFiles(t *testing.T) {
	t.Run("memory.max", func(t *testing.T) {
		root := fakeRoot(t, "cpu memory pids\n", true)
		path := filepath.Join(root, Parent, "memory.max")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if st := Configure(root, 8*gib, gib); st.Err == nil || st.Limited {
			t.Fatalf("status = %+v, want an error and no limit", st)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("memory.max was created")
		}
	})
	t.Run("memory.high is optional", func(t *testing.T) {
		root := fakeRoot(t, "cpu memory pids\n", true)
		path := filepath.Join(root, Parent, "memory.high")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if st := Configure(root, 8*gib, gib); st.Err != nil || !st.Limited {
			t.Fatalf("status = %+v", st)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("memory.high was created")
		}
	})
	t.Run("reserve above total", func(t *testing.T) {
		root := fakeRoot(t, "cpu memory pids\n", true)
		before := snapshot(t, root)
		if st := Configure(root, gib, 2*gib); st.Err == nil || st.Limited {
			t.Fatalf("status = %+v, want an error and no limit", st)
		}
		if after := snapshot(t, root); !maps.Equal(before, after) {
			t.Fatalf("files changed:\nbefore %v\nafter  %v", before, after)
		}
	})
}

// useFakeHost points Ensure at a fake cgroup root and meminfo file.
func useFakeHost(t *testing.T, root, meminfo string) {
	t.Helper()
	oldRoot, oldMeminfo := cgroupRoot, meminfoPath
	cgroupRoot, meminfoPath = root, meminfo
	t.Cleanup(func() { cgroupRoot, meminfoPath = oldRoot, oldMeminfo })
}

// fakeHost returns a cgroup root and config dir for a host with total RAM.
func fakeHost(t *testing.T, total uint64) (root, configDir string) {
	t.Helper()
	root = fakeRoot(t, "cpu memory pids\n", true)
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	put(t, meminfo, "MemTotal:       "+strconv.FormatUint(total/1024, 10)+" kB\nMemFree: 1 kB\n")
	useFakeHost(t, root, meminfo)
	return root, t.TempDir()
}

func TestEnsure(t *testing.T) {
	const total = 8000000 * 1024
	for _, tt := range []struct {
		name     string
		total    uint64
		override *string
		want     Status
		maxFile  string
		note     string
	}{
		{"no override", total, nil, Status{Unified: true, Limited: true, Reserve: 1003 * mib, Max: total - 1003*mib}, strconv.FormatUint(total-1003*mib, 10), ""},
		{"empty override", total, ptr("\n"), Status{Unified: true, Limited: true, Reserve: 1003 * mib, Max: total - 1003*mib}, strconv.FormatUint(total-1003*mib, 10), ""},
		{"quantity", total, ptr("768Mi\n"), Status{Unified: true, Limited: true, Reserve: 768 * mib, Max: total - 768*mib}, strconv.FormatUint(total-768*mib, 10), ""},
		{"off", total, ptr("off\n"), Status{Unified: true}, "max", "off"},
		{"host too small for the default", 400 * mib, nil, Status{Unified: true}, "max", "no app memory budget"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, configDir := fakeHost(t, tt.total)
			put(t, filepath.Join(root, Parent, "memory.max"), "123456789\n")
			if tt.override != nil {
				put(t, filepath.Join(configDir, OverrideFile), *tt.override)
			}
			st := Ensure(configDir)
			note := st.Note
			st.Note = ""
			if st != tt.want {
				t.Fatalf("status = %+v, want %+v", st, tt.want)
			}
			if (tt.note == "") != (note == "") || !strings.Contains(note, tt.note) {
				t.Fatalf("note = %q, want one containing %q", note, tt.note)
			}
			if got := get(t, filepath.Join(root, Parent, "memory.max")); got != tt.maxFile {
				t.Fatalf("memory.max = %q, want %q", got, tt.maxFile)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestEnsureUsesTheDefaultForABadOverride(t *testing.T) {
	const total = 8000000 * 1024
	for _, bad := range []string{"abc", "0", "-1", "768", "255Mi", "8Gi", "99999999999999Ti", "a directory"} {
		t.Run(bad, func(t *testing.T) {
			root, configDir := fakeHost(t, total)
			path := filepath.Join(configDir, OverrideFile)
			if bad == "a directory" {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, path, bad+"\n")
			}
			st := Ensure(configDir)
			if st.Err != nil || !st.Limited || st.Reserve != 1003*mib || st.Max != total-1003*mib {
				t.Fatalf("status = %+v", st)
			}
			if !strings.Contains(st.Note, path) {
				t.Fatalf("note %q does not name %s", st.Note, path)
			}
			if got := get(t, filepath.Join(root, Parent, "memory.max")); got != strconv.FormatUint(total-1003*mib, 10) {
				t.Fatalf("memory.max = %q", got)
			}
		})
	}
}

func TestEnsureIsSafeConcurrently(t *testing.T) {
	_, configDir := fakeHost(t, 4*gib)
	results := make([]Status, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = Ensure(configDir)
		}()
	}
	wg.Wait()
	want := Status{Unified: true, Limited: true, Reserve: 891 * mib, Max: 4*gib - 891*mib}
	for i, st := range results {
		if st != want {
			t.Errorf("goroutine %d: status = %+v, want %+v", i, st, want)
		}
	}
}

func TestContainerPath(t *testing.T) {
	t.Setenv("WENDY_SYSTEMD_SERVICE_NAME", "edge-agent")
	for _, tt := range []struct {
		appID, service string
		unified        bool
		want           string
	}{
		{"com.example.app", "", true, "/wendy-apps/system.slice:edge-agent:com.example.app"},
		{"com.example.app", "worker", true, "/wendy-apps/system.slice:edge-agent:com.example.app@worker"},
		{"com.example.app", "", false, "system.slice:edge-agent:com.example.app"},
		{"com.example.app", "worker", false, "system.slice:edge-agent:com.example.app@worker"},
	} {
		if got := ContainerPath(tt.appID, tt.service, tt.unified); got != tt.want {
			t.Errorf("ContainerPath(%q, %q, %v) = %q, want %q", tt.appID, tt.service, tt.unified, got, tt.want)
		}
	}
	// A service and an app with a similar name must remain separate leaves.
	if ContainerPath("com.example.app", "worker", true) == ContainerPath("com.example.app-worker", "", true) {
		t.Fatal("cgroup identity collision")
	}
}

func TestInBudget(t *testing.T) {
	for _, tt := range []struct {
		membership string
		want       bool
	}{
		{"0::/wendy-apps/system.slice:edge-agent:com.example.app\n", true},
		{"0::/wendy-apps/system.slice:edge-agent:com.example.app/child\n", true},
		{"0::/system.slice:edge-agent:com.example.app\n", false},
		{"0::/wendy-apps-other/system.slice:edge-agent:com.example.app\n", false},
		{"0::/wendy-apps\n", false},
		{"1:memory:/wendy-apps/system.slice:edge-agent:com.example.app\n", false},
	} {
		if got := InBudget(tt.membership); got != tt.want {
			t.Errorf("InBudget(%q) = %v, want %v", tt.membership, got, tt.want)
		}
	}
}

func TestUnified(t *testing.T) {
	useFakeHost(t, fakeRoot(t, "memory\n", false), meminfoPath)
	if !Unified() {
		t.Fatal("a root with cgroup.controllers is unified")
	}
	useFakeHost(t, t.TempDir(), meminfoPath)
	if Unified() {
		t.Fatal("a root without cgroup.controllers is not unified")
	}
}

func TestHelperPath(t *testing.T) {
	for _, tt := range []struct {
		name    string
		unified bool
		want    string
		ok      bool
	}{
		{"wendy-ros2-cli-sidecar-fastrtps", true, "/wendy-apps/wendy-ros2-cli-sidecar-fastrtps", true},
		{"wendy-ros2-cli-sidecar-fastrtps", false, "", false},
		{"", true, "", false},
		{"a/b", true, "", false},
		{"..", true, "", false},
		{".", true, "", false},
		{"system.slice:edge-agent:x", true, "", false},
	} {
		got, ok := HelperPath(tt.name, tt.unified)
		if got != tt.want || ok != tt.ok {
			t.Errorf("HelperPath(%q, %v) = (%q, %v), want (%q, %v)", tt.name, tt.unified, got, ok, tt.want, tt.ok)
		}
	}
}

package data

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sizes from Mesh 1, the Jetson Orin Nano the bug was found on: a 12 GB A/B
// root slot beside a 433 GB /data.
const (
	meshRootBytes = int64(12_000_000_000)
	meshDataBytes = int64(433_000_000_000)
)

// testLayout is a storeLayout under one temporary directory. The data mount
// and the data partition exist; the stores themselves do not yet.
func testLayout(t *testing.T) storeLayout {
	t.Helper()
	dir := t.TempDir()
	l := storeLayout{
		dataMount:     filepath.Join(dir, "var/lib/wendy"),
		legacy:        filepath.Join(dir, "var/lib/wendy-agent/data/episodes"),
		dataPartition: filepath.Join(dir, "data"),
	}
	l.dataDisk = filepath.Join(l.dataMount, "wendy-data", "episodes")
	for _, d := range []string{l.dataMount, l.dataPartition} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// fakeFilesystems answers filesystemOf by the longest matching path prefix,
// and with the system filesystem for anything else.
func fakeFilesystems(t *testing.T, system fsIdentity, mounts map[string]fsIdentity) {
	t.Helper()
	previous := filesystemOf
	filesystemOf = func(path string) (fsIdentity, error) {
		best, found := "", system
		for prefix, fs := range mounts {
			if (path == prefix || strings.HasPrefix(path, prefix+string(filepath.Separator))) && len(prefix) > len(best) {
				best, found = prefix, fs
			}
		}
		return found, nil
	}
	t.Cleanup(func() { filesystemOf = previous })
}

// meshFilesystems is Mesh 1: the data mount and the data partition are the
// same separate, larger filesystem, and everything else is the root slot.
func meshFilesystems(t *testing.T, l storeLayout) {
	data := fsIdentity{device: 2, size: meshDataBytes}
	fakeFilesystems(t, fsIdentity{device: 1, size: meshRootBytes}, map[string]fsIdentity{l.dataMount: data, l.dataPartition: data})
}

func useStoreLayout(t *testing.T, l storeLayout) {
	t.Helper()
	previous := defaultStoreLayout
	defaultStoreLayout = l
	t.Cleanup(func() { defaultStoreLayout = previous })
}

func TestDefaultStoreMovesToSeparateLargerFilesystem(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)

	got := l.choose()
	if got.Path != l.dataDisk {
		t.Fatalf("store = %s, want the data disk %s (%s)", got.Path, l.dataDisk, got.Reason)
	}
	if want := filepath.Join(filepath.Dir(l.dataDisk), "campaigns"); got.CampaignDir != want {
		t.Fatalf("campaign plans = %s, want them beside the store at %s", got.CampaignDir, want)
	}
	if want := "is on its own filesystem (433 GB), larger than the one holding " + l.legacy + " (12 GB)"; !strings.Contains(got.Reason, want) {
		t.Fatalf("reason %q does not say %q", got.Reason, want)
	}
	if info, err := os.Stat(l.dataDisk); err != nil || !info.IsDir() {
		t.Fatalf("data disk store was not created: %v", err)
	}
}

func TestDefaultStoreKeepsLegacyRootWhenDataDiskGainsNothing(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, l storeLayout)
		reason string
	}{
		{
			name: "same filesystem as the old store",
			setup: func(t *testing.T, l storeLayout) {
				fakeFilesystems(t, fsIdentity{device: 1, size: meshRootBytes}, nil)
			},
			reason: "is on the same filesystem as",
		},
		{
			// A host with a small / and a larger, separate /var: both stores
			// would be on /var, so moving gains nothing and hides every episode.
			name: "separate /var holds both stores",
			setup: func(t *testing.T, l storeLayout) {
				varDir := filepath.Dir(filepath.Dir(l.dataMount))
				fakeFilesystems(t, fsIdentity{device: 1, size: 20_000_000_000}, map[string]fsIdentity{varDir: {device: 3, size: 100_000_000_000}})
			},
			reason: "is on the same filesystem as",
		},
		{
			// A WendyOS VM: 3 GiB slots beside a 512 MiB /data.
			name: "separate but smaller",
			setup: func(t *testing.T, l storeLayout) {
				fakeFilesystems(t, fsIdentity{device: 1, size: 3 << 30}, map[string]fsIdentity{l.dataMount: {device: 2, size: 512 << 20}})
			},
			reason: "is on its own filesystem, but it (537 MB) is not larger than the one holding",
		},
		{
			name: "data mount missing",
			setup: func(t *testing.T, l storeLayout) {
				meshFilesystems(t, l)
				if err := os.Remove(l.dataMount); err != nil {
					t.Fatal(err)
				}
			},
			reason: "does not exist, so this is not a WendyOS device with a data partition",
		},
		{
			name: "filesystem cannot be inspected",
			setup: func(t *testing.T, l storeLayout) {
				previous := filesystemOf
				filesystemOf = func(string) (fsIdentity, error) { return fsIdentity{}, errors.New("statfs: permission denied") }
				t.Cleanup(func() { filesystemOf = previous })
			},
			reason: "could not inspect",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := testLayout(t)
			tc.setup(t, l)
			got := l.choose()
			if got.Path != l.legacy {
				t.Fatalf("store = %s, want the root-disk default %s", got.Path, l.legacy)
			}
			if want := filepath.Join(filepath.Dir(l.legacy), "campaigns"); got.CampaignDir != want {
				t.Fatalf("campaign plans = %s, want %s", got.CampaignDir, want)
			}
			if !strings.HasPrefix(got.Reason, "kept the root-disk default: ") || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason %q does not say %q", got.Reason, tc.reason)
			}
			if _, err := os.Stat(l.dataDisk); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("data disk store was created although it was not chosen: %v", err)
			}
			if got.LeftBehindMessage() != "" {
				t.Fatalf("a store that did not move reported episodes left behind: %q", got.LeftBehindMessage())
			}
			if want := filepath.Join(filepath.Dir(l.legacy), "inference"); got.InferenceDir != want {
				t.Fatalf("inference cache = %s, want it beside the store at %s, as before", got.InferenceDir, want)
			}
		})
	}
}

// writeInferenceCache lays out an inference cache the way the agent's runtime
// leaves it (Mesh 1, agent 2026.10.27): the pinned uv, uv's package cache, the
// managed Python, a virtual environment and a Hugging Face model. It returns
// each file with its contents.
func writeInferenceCache(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{
		"uv-0.10.9": "uv",
		"cache/archive-v0/SIqgOXNS0cHwV4_3T6s34/setuptools/__init__.py":                                          "",
		"cache/wheels-v6/index/e1d141a6ca947dff/torch/2.8.0+cpu-cp312-cp312-manylinux_2_28_aarch64.http":         "policy",
		"python/cpython-3.12.13-linux-aarch64-gnu/bin/python3.12":                                                "python",
		"runtime-fde4e167d62e7f28/.venv/pyvenv.cfg":                                                              "home = " + filepath.Join(dir, "python/cpython-3.12.13-linux-aarch64-gnu/bin") + "\n",
		"runtime-fde4e167d62e7f28/uv.lock":                                                                       "version = 1\n",
		"models/hub/models--webnn--yolo11n/snapshots/9c5acfdd74aaff2d0f47c51b878506361039a51f/onnx/yolo11n.onnx": "onnx",
	}
	for name, contents := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestInferenceCacheChoiceWhenTheStoreMovesToTheDataDisk(t *testing.T) {
	cases := []struct {
		name string
		// setup prepares the inference caches and returns the one the agent
		// must use.
		setup func(t *testing.T, earlier, moved string) string
	}{
		{
			name:  "fresh device",
			setup: func(t *testing.T, earlier, moved string) string { return moved },
		},
		{
			name: "an earlier agent downloaded it",
			setup: func(t *testing.T, earlier, moved string) string {
				writeInferenceCache(t, earlier)
				return earlier
			},
		},
		{
			// The first download failed before uv arrived: an empty home and an
			// interrupted temporary file are nothing to keep.
			name: "an earlier agent downloaded nothing",
			setup: func(t *testing.T, earlier, moved string) string {
				if err := os.MkdirAll(filepath.Join(earlier, "home"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(earlier, ".uv-123456"), []byte("partial"), 0o600); err != nil {
					t.Fatal(err)
				}
				return moved
			},
		},
		{
			// For example after an OS update to the other slot downloaded it to
			// the data disk, and the device then came back to this slot.
			name: "both hold one",
			setup: func(t *testing.T, earlier, moved string) string {
				writeInferenceCache(t, earlier)
				writeInferenceCache(t, moved)
				return moved
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := testLayout(t)
			meshFilesystems(t, l)
			earlier := filepath.Join(filepath.Dir(l.legacy), "inference")
			moved := filepath.Join(filepath.Dir(l.dataDisk), "inference")
			want := tc.setup(t, earlier, moved)

			got := l.choose()
			if got.Path != l.dataDisk {
				t.Fatalf("store = %s, want the data disk %s", got.Path, l.dataDisk)
			}
			if got.InferenceDir != want {
				t.Fatalf("inference cache = %s, want %s (%s)", got.InferenceDir, want, got.InferenceReason)
			}
			if want == moved && got.InferenceReason != "beside the episode store" {
				t.Fatalf("inference reason = %q", got.InferenceReason)
			}
			if want == earlier && !strings.Contains(got.InferenceReason, "move the folder to "+moved+" while the agent is stopped") {
				t.Fatalf("inference reason %q does not say how to move it", got.InferenceReason)
			}
		})
	}
}

// TestUpdateKeepsTheInferenceCacheAnEarlierAgentDownloaded drives the agent's
// path, NewManager(""), on Mesh 1 updated from an agent that kept everything
// on the root disk: the store moves, the inference cache does not, and
// nothing in it is touched or copied.
func TestUpdateKeepsTheInferenceCacheAnEarlierAgentDownloaded(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	useStoreLayout(t, l)
	earlier := filepath.Join(filepath.Dir(l.legacy), "inference")
	files := writeInferenceCache(t, earlier)

	m, err := NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.StoreLocation().Path; got != l.dataDisk {
		t.Fatalf("store = %s, want %s", got, l.dataDisk)
	}
	if got := m.InferenceDirectory(); got != earlier {
		t.Fatalf("inference cache = %s, want the one already downloaded at %s", got, earlier)
	}
	for name, contents := range files {
		b, err := os.ReadFile(filepath.Join(earlier, name))
		if err != nil || string(b) != contents {
			t.Fatalf("%s in the kept cache = %q, %v; want it untouched", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(l.dataDisk), "inference")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an inference cache was created on the data disk although the old one is in use: %v", err)
	}
}

func writeCampaignPlan(t *testing.T, dir, name, state string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".json")
	contents := `{"name":"` + name + `","state":"` + state + `"}`
	if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
	return contents
}

func deployedCampaigns(t *testing.T, m *Manager) map[string]string {
	t.Helper()
	deployed, err := m.Campaigns()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range deployed {
		out[c.Name] = c.State
	}
	return out
}

// TestNewManagerDefaultStore drives the agent's path, NewManager(""), and
// checks that a campaign an earlier agent deployed is still found once the
// episodes have moved: its plan is copied beside the new store, where it
// survives an OS update with the episodes it records, and the original is
// left where it was.
func TestNewManagerDefaultStore(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	useStoreLayout(t, l)
	earlier := filepath.Join(filepath.Dir(l.legacy), "campaigns")
	moved := filepath.Join(filepath.Dir(l.dataDisk), "campaigns")
	original := writeCampaignPlan(t, earlier, "forklift", "armed")

	m, err := NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.StoreLocation().Path; got != l.dataDisk {
		t.Fatalf("store = %s, want %s", got, l.dataDisk)
	}
	if got := m.InferenceDirectory(); got != filepath.Join(filepath.Dir(l.dataDisk), "inference") {
		t.Fatalf("inference cache = %s, want it beside the store on the data disk", got)
	}
	if got := m.StoreLocation(); got.CampaignDir != moved || !strings.Contains(got.CampaignReason, "copied 1 campaign plan from "+earlier) {
		t.Fatalf("campaign plans = %s (%q), want them copied beside the store at %s", got.CampaignDir, got.CampaignReason, moved)
	}
	if got := deployedCampaigns(t, m); len(got) != 1 || got["forklift"] != "armed" {
		t.Fatalf("campaigns after the store moved = %+v, want the one deployed before", got)
	}
	if b, err := os.ReadFile(filepath.Join(earlier, "forklift.json")); err != nil || string(b) != original {
		t.Fatalf("the earlier agent's plan = %q, %v; want it left as it was", b, err)
	}
}

// TestCampaignPlansAreCopiedOnlyOnce checks that a later start keeps what is
// beside the store: a plan redeployed after the copy is not overwritten, and
// one that appears in the earlier folder later (an older agent run again) is
// not copied over.
func TestCampaignPlansAreCopiedOnlyOnce(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	earlier := filepath.Join(filepath.Dir(l.legacy), "campaigns")
	moved := filepath.Join(filepath.Dir(l.dataDisk), "campaigns")
	writeCampaignPlan(t, earlier, "forklift", "armed")
	if got := l.choose(); got.CampaignDir != moved {
		t.Fatalf("campaign plans = %s, want %s", got.CampaignDir, moved)
	}

	writeCampaignPlan(t, moved, "forklift", "disarmed")
	writeCampaignPlan(t, earlier, "dock", "armed")
	got := l.choose()
	if got.CampaignDir != moved || got.CampaignReason != "beside the episode store" {
		t.Fatalf("campaign plans = %s (%q), want %s", got.CampaignDir, got.CampaignReason, moved)
	}
	b, err := os.ReadFile(filepath.Join(moved, "forklift.json"))
	if err != nil || !strings.Contains(string(b), `"disarmed"`) {
		t.Fatalf("the redeployed plan = %q, %v; want it kept", b, err)
	}
	if _, err := os.Stat(filepath.Join(moved, "dock.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a plan was copied again on a later start: %v", err)
	}
}

// TestCampaignPlansAfterAnOSUpdate is the other A/B slot: no earlier folder at
// all, and the plans beside the store on the data disk are what is deployed.
func TestCampaignPlansAfterAnOSUpdate(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	useStoreLayout(t, l)
	writeCampaignPlan(t, filepath.Join(filepath.Dir(l.dataDisk), "campaigns"), "forklift", "armed")

	m, err := NewManager("")
	if err != nil {
		t.Fatal(err)
	}
	if got := deployedCampaigns(t, m); len(got) != 1 || got["forklift"] != "armed" {
		t.Fatalf("campaigns after an OS update = %+v, want the one on the data disk", got)
	}
}

// TestCampaignPlansStayWhenTheyCannotBeCopied keeps every deployed campaign
// armed when the copy fails: the earlier folder stays in use, and nothing
// half-copied is left beside the store.
func TestCampaignPlansStayWhenTheyCannotBeCopied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without read permission")
	}
	l := testLayout(t)
	meshFilesystems(t, l)
	earlier := filepath.Join(filepath.Dir(l.legacy), "campaigns")
	moved := filepath.Join(filepath.Dir(l.dataDisk), "campaigns")
	writeCampaignPlan(t, earlier, "dock", "armed")
	writeCampaignPlan(t, earlier, "forklift", "armed")
	if err := os.Chmod(filepath.Join(earlier, "forklift.json"), 0); err != nil {
		t.Fatal(err)
	}

	got := l.choose()
	if got.CampaignDir != earlier || !strings.Contains(got.CampaignReason, "could not copy them to "+moved) {
		t.Fatalf("campaign plans = %s (%q), want the earlier folder %s kept in use", got.CampaignDir, got.CampaignReason, earlier)
	}
	entries, _ := os.ReadDir(filepath.Dir(moved))
	for _, e := range entries {
		if strings.Contains(e.Name(), "campaigns") {
			t.Fatalf("a failed copy left %s beside the store", e.Name())
		}
	}
}

func TestWendyDataDirOverridesDefaultStore(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	useStoreLayout(t, l)
	explicit := filepath.Join(t.TempDir(), "custom", "episodes")
	writeInferenceCache(t, filepath.Join(filepath.Dir(l.legacy), "inference"))

	m, err := NewManager(explicit)
	if err != nil {
		t.Fatal(err)
	}
	got := m.StoreLocation()
	if got.Path != explicit {
		t.Fatalf("store = %s, want WENDY_DATA_DIR's %s", got.Path, explicit)
	}
	if want := filepath.Join(filepath.Dir(explicit), "campaigns"); got.CampaignDir != want {
		t.Fatalf("campaign plans = %s, want them beside WENDY_DATA_DIR at %s, as before", got.CampaignDir, want)
	}
	if got.Reason != "WENDY_DATA_DIR names it" {
		t.Fatalf("reason = %q", got.Reason)
	}
	if want := filepath.Join(filepath.Dir(explicit), "inference"); m.InferenceDirectory() != want {
		t.Fatalf("inference cache = %s, want it beside WENDY_DATA_DIR at %s, as before", m.InferenceDirectory(), want)
	}
	if _, err := os.Stat(l.dataDisk); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("WENDY_DATA_DIR was set, yet the default store was created: %v", err)
	}
}

func TestLeftBehindEpisodesAreCountedAndLeftInPlace(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	episodes := map[string]int{
		"20261025T185000Z-0123456789abcdef":         1000,
		"20261026T090000Z-fedcba9876543210.partial": 500,
	}
	for name, size := range episodes {
		dir := filepath.Join(l.legacy, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), make([]byte, size), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// Store bookkeeping is not an episode.
	if err := os.MkdirAll(filepath.Join(l.legacy, ".recordings", "app"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.legacy, ".device-events.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := l.choose()
	if got.LeftBehind != 2 || got.LeftBehindBytes != 1500 {
		t.Fatalf("left behind = %d episodes, %d bytes; want 2 and 1500", got.LeftBehind, got.LeftBehindBytes)
	}
	want := "Wendy Data now stores episodes in " + l.dataDisk + ". 2 episodes (1.5 kB) recorded before that remain in " + l.legacy +
		", no longer listed or uploaded; nothing was moved or deleted. Move them into " + l.dataDisk +
		" while the agent is stopped to list them again, or delete them to free the root disk."
	if msg := got.LeftBehindMessage(); msg != want {
		t.Fatalf("left-behind line:\n got %q\nwant %q", msg, want)
	}
	for name := range episodes {
		if _, err := os.Stat(filepath.Join(l.legacy, name, "payload")); err != nil {
			t.Fatalf("episode %s at the old location was touched: %v", name, err)
		}
	}

	one := StoreLocation{Path: "/new", LegacyPath: "/old", LeftBehind: 1, LeftBehindBytes: 2_500_000_000}
	if msg := one.LeftBehindMessage(); !strings.Contains(msg, " 1 episode (2.5 GB) recorded before that remains in /old, no longer listed or uploaded;") || !strings.Contains(msg, "Move it into /new") {
		t.Fatalf("single episode line: %q", msg)
	}
}

// TestLeftBehindEpisodesNotUploadedAreCounted checks that the left-behind line
// says how many of those episodes never reached the cloud: the transfer worker
// reads only the new store, so they stay unuploaded until someone moves them.
func TestLeftBehindEpisodesNotUploadedAreCounted(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	for name, state := range map[string]string{
		"20261025T180000Z-0000000000000001": "uploaded",
		"20261025T180000Z-0000000000000002": "local",
		"20261025T180000Z-0000000000000003": "pending",
		"20261025T180000Z-0000000000000004": "uploading",
		"20261025T180000Z-0000000000000005": "failed",
	} {
		dir := filepath.Join(l.legacy, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"upload":{"state":"`+state+`"}}`), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// Mid-recording when the earlier agent stopped: no manifest, never uploaded
	// by anyone, and not counted as waiting either.
	if err := os.MkdirAll(filepath.Join(l.legacy, "20261026T090000Z-fedcba9876543210.partial"), 0o750); err != nil {
		t.Fatal(err)
	}

	got := l.choose()
	if got.LeftBehind != 6 || got.LeftBehindNotUploaded != 3 {
		t.Fatalf("left behind = %d episodes, %d not uploaded; want 6 and 3", got.LeftBehind, got.LeftBehindNotUploaded)
	}
	msg := got.LeftBehindMessage()
	if !strings.Contains(msg, "no longer listed or uploaded; nothing was moved or deleted. 3 of them have not been uploaded. Move them into "+l.dataDisk+
		" while the agent is stopped to list and upload them, or delete them to free the root disk.") {
		t.Fatalf("left-behind line: %q", msg)
	}

	one := StoreLocation{Path: "/new", LegacyPath: "/old", LeftBehind: 1, LeftBehindBytes: 1000, LeftBehindNotUploaded: 1}
	if msg := one.LeftBehindMessage(); !strings.Contains(msg, "nothing was moved or deleted. It has not been uploaded. Move it into /new while the agent is stopped to list and upload it, or delete it") {
		t.Fatalf("single episode line: %q", msg)
	}
	some := StoreLocation{Path: "/new", LegacyPath: "/old", LeftBehind: 3, LeftBehindBytes: 1000, LeftBehindNotUploaded: 1}
	if msg := some.LeftBehindMessage(); !strings.Contains(msg, " 1 of them has not been uploaded. Move them into /new") {
		t.Fatalf("one of several line: %q", msg)
	}
}

func TestStoreSpaceErrorText(t *testing.T) {
	cases := []struct {
		name string
		err  StoreSpaceError
		want string
	}{
		{
			// The failure seen on Mesh 1 before this change: an empty store on
			// the root slot, which has less free than the reserve.
			name: "reserve on the root disk",
			err: StoreSpaceError{Path: LegacyRoot, Free: 4_100_000_000, Reserve: DefaultReserveBytes,
				Used: 0, Quota: 2393926860, MaxQuota: DefaultMaxQuotaBytes, SuggestDataPartition: true},
			want: "Wendy Data can't record: the disk holding /var/lib/wendy-agent/data/episodes has 4.1 GB free and needs 5.4 GB free. " +
				"Point WENDY_DATA_DIR at a folder on a bigger disk (e.g. under /data) or lower WENDY_DATA_RESERVE_BYTES.",
		},
		{
			name: "reserve already on the data disk",
			err: StoreSpaceError{Path: DataDiskRoot, Free: 3_000_000_000, Reserve: DefaultReserveBytes,
				Used: 60_000_000_000, Quota: 50_000_000_000, MaxQuota: DefaultMaxQuotaBytes},
			want: "Wendy Data can't record: the disk holding /var/lib/wendy/wendy-data/episodes has 3.0 GB free and needs 5.4 GB free. " +
				"Point WENDY_DATA_DIR at a folder on a bigger disk or lower WENDY_DATA_RESERVE_BYTES. " +
				"The recordings there also take 60 GB, over the store's 50 GB limit.",
		},
		{
			name: "quota held by a recording in progress",
			err: StoreSpaceError{Path: DataDiskRoot, Free: 300_000_000_000, Reserve: DefaultReserveBytes,
				Used: 90_000_000_000, Quota: 86_600_000_000, MaxQuota: DefaultMaxQuotaBytes, Recording: 1_200_000_000},
			want: "Wendy Data can't record: the recordings in /var/lib/wendy/wendy-data/episodes take 90 GB, over the store's 87 GB limit, " +
				"and none of them can be removed to make room yet (1.2 GB is still being recorded). " +
				"Wait for recordings, uploads and downloads in progress to finish, or point WENDY_DATA_DIR at a folder on a bigger disk.",
		},
		{
			name: "quota capped by WENDY_DATA_MAX_BYTES",
			err: StoreSpaceError{Path: DataDiskRoot, Free: 300_000_000_000, Reserve: DefaultReserveBytes,
				Used: 2_000_000_000, Quota: 1_000_000_000, MaxQuota: 1_000_000_000},
			want: "Wendy Data can't record: the recordings in /var/lib/wendy/wendy-data/episodes take 2.0 GB, over the store's 1.0 GB limit, " +
				"and none of them can be removed to make room yet. Wait for recordings, uploads and downloads in progress to finish, or raise WENDY_DATA_MAX_BYTES.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestStartReportsStoreSpaceError checks the error a recording or a campaign
// trigger actually gets: Start is the path both take.
func TestStartReportsStoreSpaceError(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetWarnLogger(func(string) {})
	// No real filesystem has an exabyte free, so the reserve cannot be met.
	m.SetQuota(0, 1<<60)
	_, err = m.Start(StartOptions{Sources: []string{"applications"}})
	var space *StoreSpaceError
	if !errors.As(err, &space) {
		t.Fatalf("Start error = %v, want a *StoreSpaceError", err)
	}
	if space.Path != root || space.Reserve != 1<<60 || space.Free >= space.Reserve {
		t.Fatalf("error fields = %+v", space)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "Wendy Data can't record: the disk holding "+root+" has ") ||
		!strings.Contains(msg, "needs 1.2 EB free") || !strings.Contains(msg, "WENDY_DATA_RESERVE_BYTES") {
		t.Fatalf("message = %q", msg)
	}
}

func TestSuggestDataPartition(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	if !l.suggestDataPartition(l.legacy) {
		t.Fatal("a store on the root slot beside a separate /data should be pointed at /data")
	}
	if l.suggestDataPartition(l.dataDisk) {
		t.Fatal("a store already on the data disk must not be told to move there")
	}
	if err := os.Remove(l.dataPartition); err != nil {
		t.Fatal(err)
	}
	if l.suggestDataPartition(l.legacy) {
		t.Fatal("advice named a /data that does not exist")
	}
}

// TestSuggestDataPartitionOnlyWhenItIsLarger covers the hint on layouts where
// /data is not simply a bigger disk beside the root slot.
func TestSuggestDataPartitionOnlyWhenItIsLarger(t *testing.T) {
	t.Run("smaller than the store's filesystem", func(t *testing.T) {
		// A WendyOS VM: the store stays on the 3 GiB slot because /data is
		// 512 MiB, so pointing at /data would send the user to a smaller disk.
		l := testLayout(t)
		data := fsIdentity{device: 2, size: 512 << 20}
		fakeFilesystems(t, fsIdentity{device: 1, size: 3 << 30}, map[string]fsIdentity{l.dataMount: data, l.dataPartition: data})
		if l.suggestDataPartition(l.legacy) {
			t.Fatal("advice named a /data smaller than the disk the store is on")
		}
	})
	t.Run("store on a separate small /var", func(t *testing.T) {
		// The store is not on / at all, but /data is still the bigger disk.
		l := testLayout(t)
		varDir := filepath.Dir(filepath.Dir(l.dataMount))
		fakeFilesystems(t, fsIdentity{device: 1, size: 4_000_000_000}, map[string]fsIdentity{
			varDir: {device: 3, size: 8_000_000_000}, l.dataPartition: {device: 2, size: meshDataBytes}})
		if !l.suggestDataPartition(l.legacy) {
			t.Fatal("a store on a separate /var smaller than /data should be pointed at /data")
		}
	})
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0:                    "0 B",
		999:                  "999 B",
		1500:                 "1.5 kB",
		2393926860:           "2.4 GB",
		DefaultReserveBytes:  "5.4 GB",
		DefaultMaxQuotaBytes: "54 GB",
		433_000_000_000:      "433 GB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDefaultStoreRetainsNativeAppMigration(t *testing.T) {
	l := testLayout(t)
	meshFilesystems(t, l)
	l.previousDataDisk = filepath.Join(l.dataPartition, "wendy-agent/data/episodes")
	if err := os.MkdirAll(l.previousDataDisk, 0750); err != nil {
		t.Fatal(err)
	}
	got := l.choose()
	if got.Path != l.previousDataDisk || got.CampaignDir != filepath.Join(filepath.Dir(l.previousDataDisk), "campaigns") || got.InferenceDir != filepath.Join(filepath.Dir(l.previousDataDisk), "inference") {
		t.Fatalf("lost migrated store: %+v", got)
	}
	if _, err := os.Stat(l.dataDisk); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created replacement store: %v", err)
	}
}

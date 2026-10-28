package data

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// Where the episode store lives when WENDY_DATA_DIR does not say.
//
// The store used to default to LegacyRoot, on the root filesystem. On WendyOS
// that is one A/B slot of 4 to 12 GiB, so with DefaultReserveBytes of headroom
// to keep, a slot with less than 5 GiB free refused every recording while the
// /data partition beside it sat almost empty. WendyOS already bind-mounts
// /var/lib/wendy from /data/wendy (var-lib-wendy.mount in WendyOS-Builder) for
// app volumes, so the store follows them there: DataDiskRoot.
//
// The folder is wendy-data rather than data because /var/lib/wendy already
// holds app-data (the per-app Wendy Data socket directories), and a bare data
// beside it, on a disk mounted at /data, would say nothing about what it holds.
// Its episodes and inference children keep the shape of the old
// /var/lib/wendy-agent/data.
//
// The inference cache does not follow the store on a device where an earlier
// agent already downloaded it; see inferenceDir. Campaign plans do, copied
// once from where an earlier agent deployed them; see campaignDir.
const (
	// DataDiskRoot is the default store on a device whose /var/lib/wendy is a
	// different filesystem from LegacyRoot's, and a larger one, which is every
	// WendyOS board with a /data partition.
	DataDiskRoot = "/var/lib/wendy/wendy-data/episodes"
	// LegacyRoot is the store every earlier agent defaulted to, and the one
	// this agent still uses where DataDiskRoot would gain nothing: a host
	// without /var/lib/wendy, or one where it shares LegacyRoot's filesystem
	// (a host with a separate /var holds both) or is smaller than it.
	LegacyRoot = "/var/lib/wendy-agent/data/episodes"
)

// StoreLocation says where the episode store is and why it is there.
type StoreLocation struct {
	// Path is the episode store.
	Path string
	// CampaignDir holds deployed campaign plans. It is beside Path, except
	// when the store moved to DataDiskRoot by default and the plans an earlier
	// agent deployed beside LegacyRoot could not be copied there: they stay in
	// use where they are. See campaignDir.
	CampaignDir string
	// InferenceDir holds the campaign inference runtime and models: the
	// pinned uv, its package cache, the managed Python, the virtual
	// environments and the Hugging Face cache. It is beside Path, except on a
	// device where the store moved to DataDiskRoot by default and an earlier
	// agent already downloaded them beside LegacyRoot: they stay there.
	InferenceDir string
	// Reason is the clause the agent logs to say why Path was chosen, and
	// CampaignReason and InferenceReason the ones for CampaignDir and
	// InferenceDir.
	Reason          string
	CampaignReason  string
	InferenceReason string
	// LeftBehind and LeftBehindBytes describe the episodes still in LegacyRoot
	// when the store moved to DataDiskRoot. Nothing moves or deletes them;
	// they are no longer listed or uploaded. LeftBehindNotUploaded counts the
	// ones whose upload was pending, in progress or failed.
	LeftBehind            int
	LeftBehindBytes       int64
	LeftBehindNotUploaded int
	LegacyPath            string
}

// LeftBehindMessage is the one log line about episodes an earlier agent
// recorded at LegacyRoot, or "" when there are none.
func (l StoreLocation) LeftBehindMessage() string {
	if l.LeftBehind == 0 {
		return ""
	}
	noun, verb, pronoun := "episodes", "remain", "them"
	if l.LeftBehind == 1 {
		noun, verb, pronoun = "episode", "remains", "it"
	}
	msg := fmt.Sprintf("Wendy Data now stores episodes in %s. %d %s (%s) recorded before that %s in %s, no longer listed or uploaded; nothing was moved or deleted.",
		l.Path, l.LeftBehind, noun, humanBytes(l.LeftBehindBytes), verb, l.LegacyPath)
	switch {
	case l.LeftBehindNotUploaded == 0:
	case l.LeftBehind == 1:
		msg += " It has not been uploaded."
	case l.LeftBehindNotUploaded == 1:
		msg += " 1 of them has not been uploaded."
	default:
		msg += fmt.Sprintf(" %d of them have not been uploaded.", l.LeftBehindNotUploaded)
	}
	then := "list " + pronoun + " again"
	if l.LeftBehindNotUploaded > 0 {
		then = "list and upload " + pronoun
	}
	return msg + fmt.Sprintf(" Move %s into %s while the agent is stopped to %s, or delete %s to free the root disk.", pronoun, l.Path, then, pronoun)
}

// explicitStoreLocation is the location of a store whose path was given, which
// in the agent means WENDY_DATA_DIR. Everything stays beside it, exactly as it
// did before the default moved.
func explicitStoreLocation(root string) StoreLocation {
	return besideStore(StoreLocation{Path: root, Reason: "WENDY_DATA_DIR names it"})
}

// besideStore puts campaign plans and the inference cache beside the store,
// where every earlier agent kept them.
func besideStore(l StoreLocation) StoreLocation {
	l.CampaignDir = filepath.Join(filepath.Dir(l.Path), "campaigns")
	l.CampaignReason = besideStoreReason
	l.InferenceDir = filepath.Join(filepath.Dir(l.Path), "inference")
	l.InferenceReason = besideStoreReason
	return l
}

const besideStoreReason = "beside the episode store"

// fsIdentity is what the store choice compares: which filesystem holds a path,
// and how big that filesystem is.
type fsIdentity struct {
	device uint64
	size   int64
}

// filesystemOf is a variable so tests can present a separate, larger data
// filesystem on a host that has only one.
var filesystemOf = statFilesystem

// storeLayout names the paths the default store is chosen between. The agent
// uses defaultStoreLayout; tests substitute temporary directories.
type storeLayout struct {
	// dataMount is the directory WendyOS bind-mounts from /data/wendy.
	dataMount string
	dataDisk  string
	// legacy is LegacyRoot. Its filesystem is what the data mount must differ
	// from: on WendyOS that is the root slot, and on a host with a separate
	// /var it is /var, which holds the data mount too.
	legacy string
	// dataPartition is named in the advice of a space error, and only when
	// it really is a separate filesystem larger than the store's.
	dataPartition string
}

var defaultStoreLayout = storeLayout{
	dataMount:     "/var/lib/wendy",
	dataDisk:      DataDiskRoot,
	legacy:        LegacyRoot,
	dataPartition: "/data",
}

// choose picks the default store. DataDiskRoot wins only when it is really on
// a different filesystem from LegacyRoot's, and a larger one; anything that
// cannot be confirmed keeps LegacyRoot, which is what every earlier agent did.
func (l storeLayout) choose() StoreLocation {
	keep := func(why string) StoreLocation {
		return besideStore(StoreLocation{Path: l.legacy, Reason: "kept the root-disk default: " + why})
	}
	if info, err := os.Stat(l.dataMount); err != nil || !info.IsDir() {
		return keep(l.dataMount + " does not exist, so this is not a WendyOS device with a data partition")
	}
	data, err := filesystemOf(nearestExisting(l.dataDisk))
	if err != nil {
		return keep(fmt.Sprintf("could not inspect %s: %v", l.dataMount, err))
	}
	legacy, err := filesystemOf(nearestExisting(l.legacy))
	if err != nil {
		return keep(fmt.Sprintf("could not inspect the filesystem holding %s: %v", l.legacy, err))
	}
	if data.device == legacy.device {
		return keep(fmt.Sprintf("%s is on the same filesystem as %s", l.dataMount, l.legacy))
	}
	if data.size <= legacy.size {
		return keep(fmt.Sprintf("%s is on its own filesystem, but it (%s) is not larger than the one holding %s (%s)", l.dataMount, humanBytes(data.size), l.legacy, humanBytes(legacy.size)))
	}
	if err := os.MkdirAll(l.dataDisk, 0o750); err != nil {
		return keep(fmt.Sprintf("could not create %s: %v", l.dataDisk, err))
	}
	location := StoreLocation{
		Path:       l.dataDisk,
		Reason:     fmt.Sprintf("%s is on its own filesystem (%s), larger than the one holding %s (%s)", l.dataMount, humanBytes(data.size), l.legacy, humanBytes(legacy.size)),
		LegacyPath: l.legacy,
	}
	location.CampaignDir, location.CampaignReason = l.campaignDir()
	location.InferenceDir, location.InferenceReason = l.inferenceDir()
	location.LeftBehind, location.LeftBehindBytes, location.LeftBehindNotUploaded = countEpisodes(l.legacy)
	return location
}

// campaignDir chooses where campaign plans live for a store that moved to the
// data disk: beside it. /var/lib/wendy-agent is on the A/B root slot, which an
// OS update replaces, and an episode on the data disk that outlives its plan
// uploads as upload.when: always with no rate cap. The first time, the plans an
// earlier agent deployed beside LegacyRoot are copied there, so every deployed
// campaign stays armed across the agent update; the originals are left as they
// were. Once the folder beside the store exists it is used as it is, so a later
// redeploy is never overwritten by an older plan. If the copy fails, the
// earlier folder stays in use and nothing half-copied is left behind.
func (l storeLayout) campaignDir() (string, string) {
	moved := filepath.Join(filepath.Dir(l.dataDisk), "campaigns")
	earlier := filepath.Join(filepath.Dir(l.legacy), "campaigns")
	if _, err := os.Stat(moved); err == nil {
		return moved, besideStoreReason
	}
	copied, err := copyCampaignPlans(earlier, moved)
	if err != nil {
		return earlier, fmt.Sprintf("kept where an earlier agent deployed them: could not copy them to %s: %v", moved, err)
	}
	if copied == 0 {
		return moved, besideStoreReason
	}
	noun, pronoun := "campaign plans", "them"
	if copied == 1 {
		noun, pronoun = "campaign plan", "it"
	}
	return moved, fmt.Sprintf("beside the episode store; copied %d %s from %s, where an earlier agent deployed %s, and left %s there", copied, noun, earlier, pronoun, pronoun)
}

// copyCampaignPlans copies the plans in from into a new folder at to, through
// a staging folder renamed into place, so to either holds every plan or does
// not exist. It returns how many it copied, and creates nothing when from
// holds none. Hidden names are atomicfile's temporary files.
func copyCampaignPlans(from, to string) (int, error) {
	entries, err := os.ReadDir(from)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	staging := filepath.Join(filepath.Dir(to), ".campaigns-copying")
	if err := os.RemoveAll(staging); err != nil {
		return 0, err
	}
	copied := 0
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, name))
		if err == nil {
			err = os.MkdirAll(staging, 0o750)
		}
		if err == nil {
			err = atomicfile.Write(filepath.Join(staging, name), b, 0o640)
		}
		if err != nil {
			_ = os.RemoveAll(staging)
			return 0, err
		}
		copied++
	}
	if copied == 0 {
		return 0, nil
	}
	if err := os.Rename(staging, to); err != nil {
		_ = os.RemoveAll(staging)
		return 0, err
	}
	return copied, nil
}

// inferenceDir chooses the inference cache for a store that moved to the data
// disk. It goes beside the store, unless the data disk holds none yet and an
// earlier agent already downloaded one beside LegacyRoot: then that one stays
// in use where it is, so the update downloads nothing (about 1.1 GB on Mesh 1).
// Nothing is copied, moved or deleted.
//
// Moving it would also work: a moved virtual environment names the old folder
// in its interpreter link, pyvenv.cfg and every script's #! line, so uv
// discards it, but it rebuilds it from the moved package cache without
// downloading. It is not done because it means copying a gigabyte across
// filesystems during startup to free root space the cache already fits in;
// what fills the root disk is the episode store, and that moved. An OS update
// starts the other A/B slot without /var/lib/wendy-agent/data/inference, so the
// runtime it downloads, as every OS update already did, lands on the data disk
// and stays there.
func (l storeLayout) inferenceDir() (string, string) {
	moved := filepath.Join(filepath.Dir(l.dataDisk), "inference")
	earlier := filepath.Join(filepath.Dir(l.legacy), "inference")
	if !holdsInferenceDownloads(moved) && holdsInferenceDownloads(earlier) {
		return earlier, fmt.Sprintf("an earlier agent downloaded the inference runtime and models there, and they stay so that this update downloads nothing; to put them on the data disk, move the folder to %s while the agent is stopped", moved)
	}
	return moved, besideStoreReason
}

// holdsInferenceDownloads reports whether an inference cache directory holds
// anything campaign inference downloaded: the pinned uv, its package cache,
// the managed Python, a virtual environment or a model. home is the worker's
// home directory, created empty, and hidden names are the temporary files of
// an interrupted download.
func holdsInferenceDownloads(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if name := e.Name(); name != "home" && !strings.HasPrefix(name, ".") {
			return true
		}
	}
	return false
}

// suggestDataPartition reports whether a space error for the store at root
// should name the data partition: it is a separate filesystem from the
// store's, and a larger one. Advice to move to a disk that is not there, that
// the store is already on, or that is smaller (the WendyOS VM's 512 MiB /data
// beside a 3 GiB slot) would only mislead.
func (l storeLayout) suggestDataPartition(root string) bool {
	store, err := filesystemOf(nearestExisting(root))
	if err != nil {
		return false
	}
	if info, err := os.Stat(l.dataPartition); err != nil || !info.IsDir() {
		return false
	}
	data, err := filesystemOf(l.dataPartition)
	return err == nil && data.device != store.device && data.size > store.size
}

// nearestExisting returns path, or its closest ancestor that exists, so a
// store directory that has not been created yet is judged by the filesystem
// it would be created on.
func nearestExisting(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// countEpisodes counts the episode directories in a store and the bytes they
// hold, including any an earlier agent left mid-recording, and how many of the
// finished ones had not been uploaded: pending, in progress or failed, the
// states evictionTier keeps longest. Hidden entries are the store's own
// bookkeeping (the app-stream journal, event inboxes), not episodes.
func countEpisodes(root string) (count int, size int64, notUploaded int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, 0
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		count++
		size += dirBytes(dir)
		if strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		if mf, err := readManifest(dir); err == nil && evictionTier(mf.Upload.State) > 0 {
			notUploaded++
		}
	}
	return count, size, notUploaded
}

// StoreSpaceError is the episode store refusing a new episode for lack of
// room. The message rounds every size for a person; the fields keep the exact
// byte counts.
type StoreSpaceError struct {
	// Path is the episode store.
	Path string
	// Free is the free space on the store's filesystem once eviction has done
	// what it can, and Reserve is the free space eviction must preserve.
	Free, Reserve int64
	// Used is what the store holds once eviction has done what it can, and
	// Quota is the most it may hold: a fifth of the filesystem, capped at
	// MaxQuota.
	Used, Quota, MaxQuota int64
	// Recording is the part of Used in episodes still recording, which
	// eviction never touches.
	Recording int64
	// SuggestDataPartition adds "under /data" to the advice, set when a data
	// partition exists on a separate filesystem larger than the store's.
	SuggestDataPartition bool
}

func (e *StoreSpaceError) Error() string {
	disk := "a bigger disk"
	if e.SuggestDataPartition {
		disk = "a bigger disk (e.g. under /data)"
	}
	if e.Free < e.Reserve {
		msg := fmt.Sprintf("Wendy Data can't record: the disk holding %s has %s free and needs %s free. Point WENDY_DATA_DIR at a folder on %s or lower WENDY_DATA_RESERVE_BYTES.",
			e.Path, humanBytes(e.Free), humanBytes(e.Reserve), disk)
		if e.Used > e.Quota {
			msg += fmt.Sprintf(" The recordings there also take %s, over the store's %s limit.", humanBytes(e.Used), humanBytes(e.Quota))
		}
		return msg
	}
	held := "none of them can be removed to make room yet"
	if e.Recording > 0 {
		held = fmt.Sprintf("none of them can be removed to make room yet (%s is still being recorded)", humanBytes(e.Recording))
	}
	// The quota is the smaller of a fifth of the disk and MaxQuota, so raising
	// WENDY_DATA_MAX_BYTES helps only when the cap is what binds.
	way := "point WENDY_DATA_DIR at a folder on " + disk
	if e.Quota == e.MaxQuota {
		way = "raise WENDY_DATA_MAX_BYTES"
	}
	return fmt.Sprintf("Wendy Data can't record: the recordings in %s take %s, over the store's %s limit, and %s. Wait for recordings, uploads and downloads in progress to finish, or %s.",
		e.Path, humanBytes(e.Used), humanBytes(e.Quota), held, way)
}

// errStoreUnreadable wraps a failure to read the store's free space.
func errStoreUnreadable(path string, err error) error {
	return fmt.Errorf("Wendy Data can't record: could not read how much space is free on the disk holding %s: %w", path, err)
}

// humanBytes renders a byte count in decimal units with at most one decimal,
// the way disk sizes are sold and the way df -H prints them: 4.1 GB, 433 GB.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	prefix := "kMGTPE"[exp]
	if value < 10 {
		return fmt.Sprintf("%.1f %cB", value, prefix)
	}
	return fmt.Sprintf("%.0f %cB", value, prefix)
}

// StoreLocation reports where the episode store is, where campaign plans
// live, and why, for the agent to log once at startup.
func (m *Manager) StoreLocation() StoreLocation { return m.location }

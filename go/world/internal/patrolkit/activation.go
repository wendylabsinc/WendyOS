package patrolkit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (c *Catalog) Inspect(now time.Time) ([]Inspection, error) {
	entries, e := os.ReadDir(filepath.Join(c.Path, "pins"))
	if e != nil {
		return nil, e
	}
	if len(entries) > MaxEntries {
		return nil, errors.New("catalog entry bound exceeded")
	}
	result := []Inspection{}
	for _, f := range entries {
		id := strings.TrimSuffix(f.Name(), ".json")
		item := Inspection{State: "unavailable"}
		item.Profile.ID = id
		var pin Entry
		if token.MatchString(id) && privateJSON(filepath.Join(c.Path, "pins", id+".json"), &pin) == nil && pin.Profile.ID == id {
			item.Profile = pin.Profile
			item.KitSHA256 = pin.KitSHA256
		}
		v, e := c.Verify(id, now)
		if e != nil {
			item.Reason = e.Error()
			if item.Profile.ExpiresUnixMS > 0 && item.Profile.ExpiresUnixMS <= now.UnixMilli() {
				item.State = "expired"
				item.Reason = "Original approval expired; prepare a new kit"
			}
		} else {
			item.Profile = v.Entry.Profile
			item.KitSHA256 = v.Entry.KitSHA256
			item.Policy = v.Spec.Patrol
			item.State = "ready"
			item.FileCount = len(v.Files)
			for _, b := range v.Files {
				item.Bytes += int64(len(b))
			}
			if _, e = os.Lstat(filepath.Join(c.Path, "claims", id+".json")); !os.IsNotExist(e) {
				item.State = "consumed"
				item.Reason = "This approval has already been claimed. Prepare a new kit."
			}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Profile.PreparedUnixMS > result[j].Profile.PreparedUnixMS })
	return result, nil
}

// Claim consumes the approval before launching any simulator or executor. A
// crash at any later step cannot replenish it. The snapshot contains only
// verified bytes and remains private via the enclosing owner-only run directory.
func (c *Catalog) Claim(id, output string, now time.Time) (Claim, error) {
	var zero Claim
	v, e := c.Verify(id, now)
	if e != nil {
		return zero, e
	}
	output, e = filepath.Abs(output)
	if e != nil {
		return zero, e
	}
	if _, e = os.Lstat(output); !os.IsNotExist(e) {
		return zero, errors.New("activation requires a new output directory")
	}
	if e = os.Mkdir(output, 0700); e != nil {
		return zero, e
	}
	claim := Claim{Schema: Schema, ID: id, KitSHA256: v.Entry.KitSHA256, RunID: v.Entry.Profile.RunID, Output: output, ClaimedUnixMS: now.UnixMilli()}
	raw, _ := json.Marshal(claim)
	if e = exclusive(filepath.Join(c.Path, "claims", id+".json"), raw, 0600); e != nil {
		return zero, errors.New("kit already claimed or durable claim unavailable")
	}
	// No cleanup rolls back a claim, including snapshot or disk failures.
	if e = exclusive(filepath.Join(output, "offline-claim.json"), raw, 0400); e != nil {
		return zero, e
	}
	root := filepath.Join(output, "kit")
	if e = os.Mkdir(root, 0700); e != nil {
		return zero, e
	}
	for name, data := range v.Files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			return zero, e
		}
		mode := os.FileMode(0444)
		if name == "runtime/world-patrol" {
			mode = 0500
		}
		if e = exclusive(path, data, mode); e != nil {
			return zero, e
		}
	}
	// Public-data files are world-readable inside read-only container mounts;
	// the outer run directory remains owner-only on the host.
	if e = filepath.WalkDir(filepath.Join(root, "world"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if err = os.Chmod(path, 0755); err != nil {
				return err
			}
			return syncDir(path)
		}
		return os.Chmod(path, 0444)
	}); e != nil {
		return zero, e
	}
	if e = syncDir(root); e != nil {
		return zero, e
	}
	if e = syncDir(output); e != nil {
		return zero, e
	}
	return claim, nil
}

// Activation rechecks the trusted catalog, consumed claim, complete snapshot,
// and the executable itself. No signing key, asset service or renewal is used.
func (c *Catalog) Activation(id, output, executable string, now time.Time) (Verified, Claim, error) {
	var zero Verified
	var claim Claim
	if !token.MatchString(id) {
		return zero, claim, errors.New("invalid kit identifier")
	}
	output, e := filepath.Abs(output)
	if e != nil {
		return zero, claim, e
	}
	if e = privateJSON(filepath.Join(c.Path, "claims", id+".json"), &claim); e != nil {
		return zero, claim, e
	}
	if claim.Schema != Schema || claim.ID != id || claim.Output != output || claim.ClaimedUnixMS <= 0 || claim.ClaimedUnixMS > now.UnixMilli() {
		return zero, claim, errors.New("activation claim scope or clock differs")
	}
	var local Claim
	if e = privateJSON(filepath.Join(output, "offline-claim.json"), &local); e != nil || local != claim {
		return zero, claim, errors.New("activation output differs from durable claim")
	}
	v, e := c.Verify(id, now)
	if e != nil {
		return zero, claim, e
	}
	if claim.KitSHA256 != v.Entry.KitSHA256 || claim.RunID != v.Entry.Profile.RunID {
		return zero, claim, errors.New("activation claim binding differs")
	}
	root := filepath.Join(output, "kit")
	count := 0
	e = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("snapshot symlink refused")
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, ok := v.Files[filepath.ToSlash(rel)]
		if !ok {
			return errors.New("undeclared activation file")
		}
		actual, e := ReadRegular(path, int64(len(data)))
		if e != nil || Digest(actual) != Digest(data) {
			return errors.New("activation snapshot changed")
		}
		count++
		return nil
	})
	if e != nil {
		return zero, claim, e
	}
	if count != len(v.Files) {
		return zero, claim, errors.New("activation snapshot incomplete")
	}
	actual, e := ReadRegular(executable, MaxRuntimeBytes)
	if e != nil || Digest(actual) != v.Entry.Profile.Runtime.ExecutorSHA256 {
		return zero, claim, errors.New("executor differs from approved runtime")
	}
	return v, claim, nil
}

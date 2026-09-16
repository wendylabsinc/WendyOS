package data

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

var ErrInvalidRecordingCheckpoint = errors.New("invalid recording export checkpoint")

// The state is synced before a sealed snapshot is exposed or a reclaimed
// segment is unlinked. Recovery finishes an interrupted reclamation. Persisted
// sequences and the secret prevent tokens from referring to future/reused files.
type recordingJournalState struct {
	Version             int     `json:"version"`
	Secret              []byte  `json:"secret"`
	NextSegment         uint64  `json:"nextSegment"`
	SealedThrough       *uint64 `json:"sealedThrough,omitempty"`
	AcknowledgedThrough *uint64 `json:"acknowledgedThrough,omitempty"`
	MaxBytes            int64   `json:"maxBytes"`
	RetentionSeconds    int64   `json:"retentionSeconds"`
}

func (j *recordingJournal) loadState() error {
	b, err := os.ReadFile(filepath.Join(j.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		j.state = recordingJournalState{Version: 1, Secret: make([]byte, 32), MaxBytes: recordingQuotaBytes, RetentionSeconds: int64(recordingRetention / time.Second)}
		_, err = rand.Read(j.state.Secret)
		return err
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &j.state); err != nil {
		return err
	}
	if j.state.Version != 1 || len(j.state.Secret) != 32 || j.state.MaxBytes == 0 {
		return errors.New("invalid recording journal state")
	}
	if err = appconfig.ValidateRecordingStorage(&appconfig.RecordingStorage{MaxBytes: j.state.MaxBytes, RetentionSeconds: &j.state.RetentionSeconds}); err != nil {
		return err
	}
	if j.state.SealedThrough != nil && *j.state.SealedThrough >= j.state.NextSegment || j.state.AcknowledgedThrough != nil && (j.state.SealedThrough == nil || *j.state.AcknowledgedThrough > *j.state.SealedThrough) {
		return errors.New("invalid recording journal checkpoint state")
	}
	j.nextSegment = j.state.NextSegment
	return nil
}

func (j *recordingJournal) saveState() error {
	j.state.NextSegment = j.nextSegment
	b, err := json.Marshal(j.state)
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(filepath.Join(j.dir, "state.tmp"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = f.Write(b)
			if err == nil {
				err = f.Sync()
			}
			err = errors.Join(err, f.Close())
		}
	}
	if err == nil {
		err = os.Rename(filepath.Join(j.dir, "state.tmp"), filepath.Join(j.dir, "state.json"))
	}
	if err == nil {
		err = syncDirectory(j.dir)
	}
	if err != nil {
		j.failure = fmt.Errorf("journal requires recovery after state write failure: %w", err)
		return j.failure
	}
	return nil
}

func (j *recordingJournal) configure(storage *appconfig.RecordingStorage) error {
	if err := appconfig.ValidateRecordingStorage(storage); err != nil {
		return err
	}
	quota, retention := int64(recordingQuotaBytes), int64(recordingRetention/time.Second)
	if storage != nil {
		if storage.MaxBytes != 0 {
			quota = storage.MaxBytes
		}
		if storage.RetentionSeconds != nil {
			retention = *storage.RetentionSeconds
		}
	}
	if j.state.MaxBytes == quota && j.state.RetentionSeconds == retention {
		return nil
	}
	j.state.MaxBytes, j.state.RetentionSeconds = quota, retention
	return j.saveState()
}

func (j *recordingJournal) checkpoint(sequence uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], sequence)
	mac := hmac.New(sha256.New, j.state.Secret)
	mac.Write([]byte(filepath.Base(j.dir)))
	mac.Write(b[:])
	return base64.RawURLEncoding.EncodeToString(append(b[:], mac.Sum(nil)...))
}

func (j *recordingJournal) snapshotCheckpoint(checkpoint bool) ([]*os.File, []int64, string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failure != nil {
		return nil, nil, "", j.failure
	}
	if err := j.expire(); err != nil {
		return nil, nil, "", err
	}
	token := ""
	segments := j.segments
	if checkpoint {
		// Bound snapshot descriptors and transfer size even for large spools.
		var size int64
		for i, seg := range segments {
			size += seg.size
			if size >= 16<<20 || i == 63 {
				segments = segments[:i+1]
				break
			}
		}
	}
	if checkpoint && len(j.segments) > 0 {
		sequence := segments[len(segments)-1].sequence
		if j.state.SealedThrough == nil || sequence > *j.state.SealedThrough {
			j.state.SealedThrough = &sequence
			if err := j.saveState(); err != nil {
				return nil, nil, "", err
			}
		}
		token = j.checkpoint(sequence)
	}
	files, sizes, err := j.openSnapshot(segments)
	return files, sizes, token, err
}

func (j *recordingJournal) acknowledge(token string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failure != nil {
		return j.failure
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 40 {
		return ErrInvalidRecordingCheckpoint
	}
	sequence := binary.BigEndian.Uint64(b[:8])
	if !hmac.Equal([]byte(token), []byte(j.checkpoint(sequence))) || j.state.SealedThrough == nil || sequence > *j.state.SealedThrough {
		return ErrInvalidRecordingCheckpoint
	}
	if j.state.AcknowledgedThrough == nil || sequence > *j.state.AcknowledgedThrough {
		j.state.AcknowledgedThrough = &sequence
		if err := j.saveState(); err != nil {
			return err
		}
	}
	// Use the highest acknowledged sequence, also on retries after partial unlink.
	for len(j.segments) > 0 && j.segments[0].sequence <= *j.state.AcknowledgedThrough {
		seg := j.segments[0]
		if err := os.Remove(seg.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, id := range seg.ids {
			delete(j.ids, id)
		}
		j.size -= seg.size
		j.segments = j.segments[1:]
	}
	return syncDirectory(j.dir)
}

func (m *Manager) AcknowledgeRecordingExport(app, service, stream, token string) error {
	if _, err := os.Stat(filepath.Join(m.recordings.root, recordingKey(app, service, stream))); err != nil {
		return err
	}
	j, err := m.recordings.journal(app, service, stream)
	if err != nil {
		return err
	}
	return j.acknowledge(token)
}

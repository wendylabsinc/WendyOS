package agentvmfixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

type JournalReceipt struct {
	Kind                  string `json:"kind"`
	Service               string `json:"service"`
	ProofSHA256           string `json:"proof_sha256"`
	RecordSHA256          string `json:"record_sha256"`
	Epoch                 uint64 `json:"epoch"`
	ReadOnly              bool   `json:"read_only"`
	ExactEnvelopeRetained bool   `json:"exact_envelope_retained"`
	CheckpointMatches     bool   `json:"checkpoint_matches"`
	Qualification         string `json:"qualification"`
}

// CompareJournal is a test-only, offline diagnostic after the actual agent has
// stopped. It opens existing bbolt state read-only; no Create/Open recovery path
// or synthetic authority provider is used. It does not authenticate the proof:
// pair this receipt with the separate live Verify receipt for the same hash.
func CompareJournal(directory, proof, service string, generation, epoch uint64) (JournalReceipt, error) {
	if (service != "gas" && service != "temperature") || generation == 0 || epoch == 0 {
		return JournalReceipt{}, errors.New("exact service generation and epoch required")
	}
	p, err := readPublic(directory)
	if err != nil {
		return JournalReceipt{}, err
	}
	b, err := agentmodule.ReadCommissionedFile(p.ConfigPath, 256<<10)
	var cfg agentmodule.FileConfig
	if err != nil || jws.DecodeStrict(b, &cfg) != nil || cfg.Policy.Device != p.Principal || cfg.Policy.Tenant != p.Tenant || cfg.Policy.Site != "office" {
		return JournalReceipt{}, errors.New("fixture commission required")
	}
	proofBytes, err := readOutput(proof)
	if err != nil {
		return JournalReceipt{}, err
	}
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	if jws.DecodeStrict(proofBytes, &signed) != nil || jws.DecodeStrict(signed.Record, &record) != nil || record.Scope.Device != p.Principal || record.Scope.Tenant != p.Tenant || record.Scope.App != App || record.Scope.Service != service || record.Scope.DeploymentGeneration != generation || record.Epoch != epoch {
		return JournalReceipt{}, errors.New("expected original publication required")
	}
	canonical, err := json.Marshal(signed)
	if err != nil || !bytes.Equal(canonical, proofBytes) {
		return JournalReceipt{}, errors.New("proof must preserve exact returned JSON bytes")
	}
	if err = hostpath.EnsureDirectory(filepath.Dir(cfg.JournalPath), 0700, false); err != nil {
		return JournalReceipt{}, err
	}
	// Opening through O_NOFOLLOW keeps even this diagnostic from creating or
	// following a replacement final file. The directory belongs to this fixture.
	db, err := bolt.Open(cfg.JournalPath, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second, OpenFile: func(path string, flags int, mode os.FileMode) (*os.File, error) {
		if path != cfg.JournalPath || flags != os.O_RDONLY {
			return nil, errors.New("read-only existing journal required")
		}
		fd, e := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return nil, e
		}
		f := os.NewFile(uintptr(fd), "fixture-journal")
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 || st.Size <= 0 || st.Size > 256<<20 {
			f.Close()
			return nil, errors.New("bounded private existing journal required")
		}
		return f, nil
	}})
	if err != nil {
		return JournalReceipt{}, err
	}
	defer db.Close()
	c := (serviceorigin.VerifiedRecord{Record: record}).Component()
	key, _ := json.Marshal(c.Key)
	sourceKey, _ := json.Marshal(struct {
		Tenant  string `json:"tenant"`
		Device  string `json:"device"`
		App     string `json:"app"`
		Service string `json:"service"`
	}{p.Tenant, p.Principal, App, service})
	err = db.View(func(tx *bolt.Tx) error {
		proofs, epochs, meta := tx.Bucket([]byte("proofs")), tx.Bucket([]byte("epochs")), tx.Bucket([]byte("metadata"))
		if proofs == nil || epochs == nil || meta == nil {
			return errors.New("journal buckets missing")
		}
		var stored struct {
			SHA256   string `json:"sha256"`
			Envelope []byte `json:"envelope"`
		}
		v := proofs.Get(key)
		if len(v) > 384<<10 || jws.DecodeStrict(v, &stored) != nil || stored.SHA256 != digest(stored.Envelope) || !bytes.Equal(stored.Envelope, proofBytes) {
			return errors.New("original proof not retained byte-for-byte")
		}
		var reserved struct {
			Epoch  uint64 `json:"epoch"`
			SHA256 string `json:"sha256"`
		}
		v = epochs.Get(sourceKey)
		if len(v) > 256 || jws.DecodeStrict(v, &reserved) != nil || reserved.Epoch != epoch {
			return errors.New("durable reserved epoch differs")
		}
		epochBytes, _ := json.Marshal(struct {
			Source string
			Epoch  uint64
		}{string(sourceKey), epoch})
		if reserved.SHA256 != digest(epochBytes) {
			return errors.New("durable epoch integrity mismatch")
		}
		var checked struct {
			SHA256 string `json:"sha256"`
			Data   []byte `json:"data"`
		}
		v = meta.Get([]byte("checkpoint"))
		if len(v) > 64<<20 || jws.DecodeStrict(v, &checked) != nil || checked.SHA256 != digest(checked.Data) {
			return errors.New("checkpoint integrity mismatch")
		}
		var cp world.Checkpoint
		if jws.DecodeStrict(checked.Data, &cp) != nil || len(cp.Entries) > 256 {
			return errors.New("checkpoint exceeds fixture bounds")
		}
		want, _ := json.Marshal(c)
		matches := 0
		for _, entry := range cp.Entries {
			if entry.Component.Key == c.Key {
				actual, _ := json.Marshal(entry.Component)
				if !bytes.Equal(want, actual) {
					return errors.New("checkpoint differs from original proof projection")
				}
				matches++
			}
		}
		if matches != 1 {
			return errors.New("unique original proof checkpoint absent")
		}
		return nil
	})
	if err != nil {
		return JournalReceipt{}, err
	}
	return JournalReceipt{Kind: Kind, Service: service, ProofSHA256: digest(proofBytes), RecordSHA256: digest(signed.Record), Epoch: epoch, ReadOnly: true, ExactEnvelopeRetained: true, CheckpointMatches: true, Qualification: "historical at-rest byte and epoch comparison only; pair with live proof verification, no current authority claim"}, nil
}

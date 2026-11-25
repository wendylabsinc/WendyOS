package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

func preloadNode(t *testing.T, count int) (options, *store.Store) {
	t.Helper()
	o, cfg, v := fixture(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	wc := world.Config{Tenant: "lab", Site: "synthetic-office", ClockDomain: "simulation:" + o.runID, Now: time.Now, MaxEntries: 4096, MaxFutureSkew: 100 * time.Millisecond, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "office-floor", Revision: "office-frame-v1"}}}
	s, err := store.Open(filepath.Join(dir, "node-2.state"), wc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	n, err := node.New(node.Config{Tenant: "lab", Site: "synthetic-office", Audience: "node-2", ProofPath: filepath.Join(dir, "node-2.proofs"), MaxRecords: 4096}, s, v)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(n.Handler())
	t.Cleanup(server.Close)
	cfg.URL = server.URL
	writeJSON(t, o.credentials, cfg)
	o.preload = true
	o.count = count
	o.manifest = filepath.Join(t.TempDir(), "manifest.json")
	o.stateDir = dir
	o.nodeIndex = 2
	o.minFresh = 60
	return o, s
}

func TestExactPreloadZeroAnd200SurviveDurableVerification(t *testing.T) {
	for _, count := range []int{0, 200} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			o, _ := preloadNode(t, count)
			var out bytes.Buffer
			if err := run(context.Background(), o, &out); err != nil {
				t.Fatalf("preload: %v; %s", err, out.String())
			}
			var report preloadReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if !report.Complete || report.Acknowledged != count || report.Attempted != count || report.RecordTTLNS != int64(5*time.Minute) {
				t.Fatal(report)
			}
			out.Reset()
			if err := verifyPreload(o, &out); err != nil {
				t.Fatal(err)
			}
			var proof map[string]any
			if err := json.Unmarshal(out.Bytes(), &proof); err != nil {
				t.Fatal(err)
			}
			if proof["exact_signed_set"] != true || int(proof["count"].(float64)) != count {
				t.Fatal("missing exact-set verification")
			}
			if count > 0 && proof["minimum_remaining_ms"].(float64) < 60000 {
				t.Fatal("freshness not proven")
			}
			if count > 0 {
				// Read both the predecessor format and the page-recovery format,
				// while retaining exact signed-set and cardinality verification.
				path := filepath.Join(o.stateDir, "node-2.proofs")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(original, &envelope); err != nil {
					t.Fatal(err)
				}
				for _, version := range []int{1, 2, 3} {
					envelope["version"] = json.RawMessage(strconv.Itoa(version))
					writeJSON(t, path, envelope)
					err := verifyPreload(o, io.Discard)
					if (err == nil) != (version == 1 || version == 2) {
						t.Fatalf("proof version %d: %v", version, err)
					}
				}
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Each replica runs the same read-only validator against its own checkpoint
			// and unchanged origin proofs. The node audience does not impersonate the source.
			for _, index := range []int{1, 3} {
				for _, kind := range []string{"state", "proofs"} {
					b, err := os.ReadFile(filepath.Join(o.stateDir, "node-2."+kind))
					if os.IsNotExist(err) && count == 0 {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(o.stateDir, "node-"+strconv.Itoa(index)+"."+kind)
					if err = os.WriteFile(path, b, 0600); err != nil {
						t.Fatal(err)
					}
				}
				o.nodeIndex = index
				if err := verifyPreload(o, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPreloadNeverRetriesAnUnknownWriteAndNeverOverwritesManifest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/publishers/epoch" {
			io.WriteString(w, `{"epoch":1}`)
			return
		}
		calls.Add(1)
		http.Error(w, "private-response-marker", 503)
	}))
	defer server.Close()
	o, _, _ := fixture(t, server.URL)
	o.preload = true
	o.count = 200
	o.manifest = filepath.Join(t.TempDir(), "manifest.json")
	var out bytes.Buffer
	if err := run(context.Background(), o, &out); err == nil {
		t.Fatal("unknown write reported as prepared")
	}
	var report preloadReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || report.Outcome != "unknown" || report.Complete || report.Acknowledged != 0 || report.Attempted != 1 {
		t.Fatal(report)
	}
	if bytes.Contains(out.Bytes(), []byte("private-response-marker")) {
		t.Fatal("raw response escaped")
	}
	if err := run(context.Background(), o, io.Discard); err == nil {
		t.Fatal("existing manifest overwritten")
	}
	if calls.Load() != 1 {
		t.Fatal("network used after manifest refusal")
	}
}

// preloadVerificationFixture constructs valid persisted inputs for the read-only
// validator. Publication deadlines and unknown-write handling are exercised by
// the separate live preload tests, without making tamper checks a throughput test.
func preloadVerificationFixture(t *testing.T) options {
	t.Helper()
	o, s := preloadNode(t, 200)
	var cfg client.Config
	if err := readPrivate(o.credentials, &cfg); err != nil {
		t.Fatal(err)
	}
	defer clear(cfg.PrivateKey)
	epoch, err := s.NextEpoch(cfg.Grant.Grant.Source)
	if err != nil {
		t.Fatal(err)
	}
	manifest := preloadManifest{Schema: "wendy.world.preload-manifest.v1", RunID: o.runID, Count: o.count, Epoch: epoch, Digests: []string{}}
	proofs := struct {
		Version int                      `json:"version"`
		Records []authority.SignedRecord `json:"records"`
	}{Version: 2}
	page := make([]store.Publication, 0, store.MaxRepairRecords)
	for index := 0; index < o.count; index++ {
		record := component(cfg, o.runID, epoch, uint64(index+1), index, time.Now())
		record.TTL = preloadTTL
		signed, err := authority.SignRecord(cfg.Grant, ed25519.PrivateKey(cfg.PrivateKey), record, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		proofs.Records = append(proofs.Records, signed)
		manifest.Digests = append(manifest.Digests, preloadDigest(record))
		page = append(page, store.Publication{Source: record.Key.Source, Component: record, ExpiresAt: time.UnixMilli(signed.ExpiresAtUnixMS)})
		if len(page) == store.MaxRepairRecords || index == o.count-1 {
			results, err := s.ApplyPreparedBatch(page, func(store.BatchPublicationState) error {
				writeJSON(t, filepath.Join(o.stateDir, "node-2.proofs"), proofs)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range results {
				if result != world.Applied {
					t.Fatalf("fixture publication: %s", result)
				}
			}
			page = page[:0]
		}
	}
	writeJSON(t, o.manifest, manifest)
	if err := verifyPreload(o, io.Discard); err != nil {
		t.Fatalf("valid verification fixture refused: %v", err)
	}
	return o
}

func TestPreloadVerificationRefusesTamperedInputsAndStaleState(t *testing.T) {
	o := preloadVerificationFixture(t)
	manifest, err := os.ReadFile(o.manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"wrong-count", "wrong-run", "wrong-root", "digest-order", "proof-symlink", "state-fifo", "state-oversize", "stale-state"} {
		t.Run(kind, func(t *testing.T) {
			test := o
			switch kind {
			case "wrong-count":
				test.count = 0
			case "wrong-run":
				test.runID = "other-run"
			case "wrong-root":
				test.trust = filepath.Join(t.TempDir(), "trust.json")
				writeJSON(t, test.trust, client.SupervisionTrust{})
			case "digest-order":
				var m preloadManifest
				json.Unmarshal(manifest, &m)
				m.Digests[0], m.Digests[1] = m.Digests[1], m.Digests[0]
				test.manifest = filepath.Join(t.TempDir(), "manifest.json")
				writeJSON(t, test.manifest, m)
			default:
				test.stateDir = t.TempDir()
				for _, suffix := range []string{"state", "proofs"} {
					b, err := os.ReadFile(filepath.Join(o.stateDir, "node-2."+suffix))
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(test.stateDir, "node-2."+suffix), b, 0600); err != nil {
						t.Fatal(err)
					}
				}
				switch kind {
				case "proof-symlink":
					p := filepath.Join(test.stateDir, "node-2.proofs")
					os.Remove(p)
					if err := os.Symlink(filepath.Join(o.stateDir, "node-2.proofs"), p); err != nil {
						t.Fatal(err)
					}
				case "state-fifo":
					p := filepath.Join(test.stateDir, "node-2.state")
					os.Remove(p)
					if err := unix.Mkfifo(p, 0600); err != nil {
						t.Fatal(err)
					}
				case "state-oversize":
					if err := os.WriteFile(filepath.Join(test.stateDir, "node-2.state"), bytes.Repeat([]byte(" "), (16<<20)+1), 0600); err != nil {
						t.Fatal(err)
					}
				case "stale-state":
					p := filepath.Join(test.stateDir, "node-2.state")
					b, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					var env struct {
						SHA256 string          `json:"sha256"`
						State  json.RawMessage `json:"state"`
					}
					json.Unmarshal(b, &env)
					// A valid checksum does not make an almost expired record fresh.
					var state map[string]json.RawMessage
					if err = json.Unmarshal(env.State, &state); err != nil {
						t.Fatal(err)
					}
					var cp world.Checkpoint
					if err = json.Unmarshal(state["world"], &cp); err != nil {
						t.Fatal(err)
					}
					cp.Entries[0].ExpiresAt = time.Now().Add(time.Second)
					state["world"], err = json.Marshal(cp)
					if err != nil {
						t.Fatal(err)
					}
					env.State, err = json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					sum := sha256.Sum256(env.State)
					env.SHA256 = hex.EncodeToString(sum[:])
					writeJSON(t, p, env)
				}
			}
			started := time.Now()
			if err := verifyPreload(test, io.Discard); err == nil {
				t.Fatal("invalid preload verified")
			}
			// Special files must refuse without blocking on their contents.
			// Other cases may verify all 200 signatures under the race detector.
			if (kind == "proof-symlink" || kind == "state-fifo") && time.Since(started) > time.Second {
				t.Fatal("special-file verification blocked")
			}
		})
	}
}

func TestPreloadValidationSamplesClockAfterAllStateReads(t *testing.T) {
	o, _ := preloadNode(t, 0)
	if err := run(context.Background(), o, io.Discard); err != nil {
		t.Fatal(err)
	}
	reads, calls := 0, 0
	read := func(path string, out any, missing bool) error {
		if calls != 0 {
			t.Fatal("validation time sampled before file reads completed")
		}
		reads++
		return readStateJSON(path, out, missing)
	}
	clock := func() time.Time {
		calls++
		if reads != 2 {
			t.Fatal("clock sampled before both state files")
		}
		return time.Now()
	}
	if err := verifyPreloadInputs(o, io.Discard, read, clock); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("validation did not use one consistent time")
	}
	// A genuine persisted floor in the future remains an error, not clamped time.
	path := filepath.Join(o.stateDir, "node-2.state")
	var env struct {
		SHA256 string          `json:"sha256"`
		State  json.RawMessage `json:"state"`
	}
	if err := readStateJSON(path, &env, false); err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	json.Unmarshal(env.State, &state)
	var cp world.Checkpoint
	json.Unmarshal(state["world"], &cp)
	cp.CapturedAt = time.Now().Add(time.Minute)
	state["world"], _ = json.Marshal(cp)
	env.State, _ = json.Marshal(state)
	sum := sha256.Sum256(env.State)
	env.SHA256 = hex.EncodeToString(sum[:])
	writeJSON(t, path, env)
	var output bytes.Buffer
	if err := verifyPreload(o, &output); err == nil {
		t.Fatal("future durable floor accepted")
	}
	var report map[string]any
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["failure_class"] != "clock_regression" || len(report) != 6 {
		t.Fatal("missing bounded failure classification")
	}
	if preloadVerificationClass(io.ErrUnexpectedEOF) != "verification_refused" {
		t.Fatal("raw error leaked")
	}
}

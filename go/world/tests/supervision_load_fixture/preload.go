package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

const preloadTTL = 5 * time.Minute

type preloadManifest struct {
	Schema  string   `json:"schema"`
	RunID   string   `json:"run_id"`
	Count   int      `json:"count"`
	Epoch   uint64   `json:"epoch"`
	Digests []string `json:"digests"`
}
type preloadReport struct {
	Schema       string `json:"schema"`
	Domain       string `json:"domain"`
	RunID        string `json:"run_id"`
	Count        int    `json:"count"`
	Attempted    int    `json:"attempted"`
	Acknowledged int    `json:"acknowledged"`
	Complete     bool   `json:"complete"`
	Outcome      string `json:"outcome"`
	FailureClass string `json:"failure_class"`
	ElapsedNS    int64  `json:"elapsed_ns"`
	RecordTTLNS  int64  `json:"record_ttl_ns"`
}

func preloadDigest(c world.Component) string {
	c.Timing.AgeHint = 0
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func runPreload(ctx context.Context, o options, output io.Writer) (resultErr error) {
	started := time.Now()
	r := preloadReport{Schema: "wendy.world.preload.v1", Domain: "simulation", RunID: o.runID, Count: o.count, Outcome: "not_attempted", RecordTTLNS: int64(preloadTTL)}
	defer func() {
		r.ElapsedNS = time.Since(started).Nanoseconds()
		if err := json.NewEncoder(output).Encode(r); err != nil && resultErr == nil {
			resultErr = errors.New("preload report unavailable")
		}
	}()
	if (o.count != 0 && o.count != 200) || o.manifest == "" {
		r.FailureClass = "input_refused"
		return errors.New("preload requires exact zero/200 cardinality and manifest")
	}
	cfg, c, err := load(o)
	if err != nil {
		r.FailureClass = "input_refused"
		return errors.New("preload input refused")
	}
	defer clear(cfg.PrivateKey)
	var trust client.SupervisionTrust
	if readPrivate(o.trust, &trust) != nil || time.Until(time.UnixMilli(cfg.Grant.Grant.NotAfterUnixMS)) < 3*time.Minute || time.Until(time.UnixMilli(trust.Revocations.Revocations.ExpiresAtUnixMS)) < 3*time.Minute {
		r.FailureClass = "authority"
		return errors.New("preload authority has insufficient remaining lifetime")
	}
	// Open before network access. Never overwrite an old manifest or follow a link.
	f, err := os.OpenFile(o.manifest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		r.FailureClass = "manifest_refused"
		return errors.New("preload manifest destination refused")
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	initial, done := context.WithTimeout(ctx, time.Second)
	epoch, err := c.NextEpoch(initial)
	done()
	if err != nil {
		r.Outcome = "unknown"
		r.FailureClass = failureClass(err)
		return errors.New("preload epoch outcome unknown")
	}
	manifest := preloadManifest{Schema: "wendy.world.preload-manifest.v1", RunID: o.runID, Count: o.count, Epoch: epoch, Digests: []string{}}
	for index := 0; index < o.count; index++ {
		if ctx.Err() != nil {
			r.Outcome = "unknown"
			r.FailureClass = "timeout_or_cancel"
			return errors.New("preload did not complete within bound")
		}
		record := component(cfg, o.runID, epoch, uint64(index+1), index, time.Now())
		record.TTL = preloadTTL
		signed, err := authority.SignRecord(cfg.Grant, ed25519.PrivateKey(cfg.PrivateKey), record, time.Now())
		if err != nil {
			r.FailureClass = "record_refused"
			return errors.New("preload record refused")
		}
		body, err := json.Marshal(signed)
		if err != nil {
			return errors.New("preload encoding refused")
		}
		r.Attempted++
		request, end := context.WithTimeout(ctx, time.Second)
		var ack struct {
			Result world.Result `json:"result"`
		}
		err = c.Do(request, "publish", "POST", "/v1/components", body, &ack)
		end()
		if err != nil || ack.Result != world.Applied {
			r.Outcome = "unknown"
			r.FailureClass = "unexpected_ack"
			if err != nil {
				r.FailureClass = failureClass(err)
			}
			return errors.New("preload publication outcome unconfirmed; no retry")
		}
		r.Acknowledged++
		manifest.Digests = append(manifest.Digests, preloadDigest(record))
	}
	if err = json.NewEncoder(f).Encode(manifest); err == nil {
		err = f.Sync()
	}
	if err != nil {
		r.FailureClass = "manifest_write"
		r.Outcome = "unknown"
		return errors.New("preload manifest could not be persisted")
	}
	r.Complete = true
	r.Outcome = "prepared"
	return nil
}

func readStateJSON(path string, out any, missing bool) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if missing && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("preload state unavailable")
	}
	f := os.NewFile(uintptr(fd), "preload-state")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("preload state must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || authority.DecodeStrictLimit(b, out, 16<<20) != nil {
		return errors.New("invalid bounded preload state")
	}
	return nil
}

func verifyPreload(o options, output io.Writer) (err error) {
	defer func() {
		if err != nil {
			_ = json.NewEncoder(output).Encode(map[string]any{"schema": "wendy.world.preload-verification-failure.v1", "domain": "simulation", "run_id": o.runID, "node_index": o.nodeIndex, "count": o.count, "failure_class": preloadVerificationClass(err)})
		}
	}()
	return verifyPreloadInputs(o, output, readStateJSON, time.Now)
}

func verifyPreloadInputs(o options, output io.Writer, readState func(string, any, bool) error, clock func() time.Time) error {
	if (o.count != 0 && o.count != 200) || o.nodeIndex < 1 || o.nodeIndex > 3 || o.stateDir == "" || o.minFresh < 1 || o.minFresh > 60 || !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(o.runID) {
		return errors.New("invalid bounded preload verification scope")
	}
	var manifest preloadManifest
	if err := readPrivate(o.manifest, &manifest); err != nil {
		return err
	}
	if manifest.Schema != "wendy.world.preload-manifest.v1" || manifest.RunID != o.runID || manifest.Count != o.count || len(manifest.Digests) != o.count || manifest.Epoch == 0 {
		return errors.New("preload manifest scope mismatch")
	}
	expected := map[string]bool{}
	for _, digest := range manifest.Digests {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) || expected[digest] {
			return errors.New("invalid preload digest set")
		}
		expected[digest] = true
	}
	var trust client.SupervisionTrust
	if err := readPrivate(o.trust, &trust); err != nil {
		return err
	}
	var envelope struct {
		SHA256 string          `json:"sha256"`
		State  json.RawMessage `json:"state"`
	}
	if err := readState(filepath.Join(o.stateDir, fmt.Sprintf("node-%d.state", o.nodeIndex)), &envelope, false); err != nil {
		return err
	}
	digest := sha256.Sum256(envelope.State)
	if envelope.SHA256 != hex.EncodeToString(digest[:]) {
		return errors.New("preload checkpoint checksum mismatch")
	}
	var saved struct {
		Version int              `json:"version"`
		World   world.Checkpoint `json:"world"`
		Epochs  []struct {
			Source world.Source `json:"source"`
			Value  uint64       `json:"value"`
		} `json:"epochs"`
	}
	if authority.DecodeStrictLimit(envelope.State, &saved, 16<<20) != nil || saved.Version != 1 || len(saved.World.Entries) != o.count {
		return errors.New("preload checkpoint cardinality mismatch")
	}
	var proofs struct {
		Version int                      `json:"version"`
		Records []authority.SignedRecord `json:"records"`
	}
	if err := readState(filepath.Join(o.stateDir, fmt.Sprintf("node-%d.proofs", o.nodeIndex)), &proofs, o.count == 0); err != nil {
		return err
	}
	if len(proofs.Records) != o.count || (o.count > 0 && proofs.Version != 1 && proofs.Version != 2) {
		return errors.New("preload proof cardinality mismatch")
	}
	// Read both bounded files before sampling validation time. Repair may
	// replace the checkpoint during those reads; its captured floor must not be
	// compared against a time sampled before the checkpoint was acquired.
	now := clock()
	if time.UnixMilli(trust.Revocations.Revocations.ExpiresAtUnixMS).Sub(now) < time.Duration(o.minFresh)*time.Second {
		return errors.New("preload revocation lifetime too short")
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: trust.RootPublicKey, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Audience: fmt.Sprintf("node-%d", o.nodeIndex), Now: func() time.Time { return now }, Replay: authority.NewMemoryReplay(8)})
	if err != nil || v.UpdateRevocations(trust.Revocations) != nil {
		return errors.New("preload trust invalid")
	}
	if _, err = world.Restore(world.Config{Tenant: "lab", Site: "synthetic-office", ClockDomain: "simulation:" + o.runID, Now: func() time.Time { return now }, MaxEntries: 4096, MaxFutureSkew: 100 * time.Millisecond, AllowSynthetic: true}, saved.World); err != nil {
		if errors.Is(err, world.ErrClockRegression) {
			return errors.New("preload checkpoint clock regression")
		}
		return errors.New("preload checkpoint invalid")
	}
	proofSet := map[string]bool{}
	for _, record := range proofs.Records {
		d := preloadDigest(record.Component)
		g := record.Grant.Grant
		if !expected[d] || proofSet[d] || !preloadGrant(g, o.runID) || time.UnixMilli(g.NotAfterUnixMS).Sub(now) < time.Duration(o.minFresh)*time.Second || time.UnixMilli(record.ExpiresAtUnixMS).Sub(now) < time.Duration(o.minFresh)*time.Second {
			return errors.New("preload proof scope/lifetime mismatch")
		}
		if _, err = v.VerifyRecord(record); err != nil {
			return errors.New("preload origin signature invalid")
		}
		sequence := record.Component.Version.Sequence
		if sequence < 1 || sequence > uint64(o.count) || record.Component.Version.Epoch != manifest.Epoch {
			return errors.New("preload version mismatch")
		}
		want := component(client.Config{Grant: record.Grant}, o.runID, manifest.Epoch, sequence, int(sequence-1), record.Component.Timing.SampleTime)
		want.TTL = preloadTTL
		if preloadDigest(want) != d || manifest.Digests[sequence-1] != d {
			return errors.New("preload synthetic lineage or exact sequence mismatch")
		}
		proofSet[d] = true
	}
	minimum := int64(0)
	seen := map[string]bool{}
	for _, entry := range saved.World.Entries {
		c := entry.Component
		d := preloadDigest(c)
		if !expected[d] || !proofSet[d] || seen[d] || c.Version.Epoch != manifest.Epoch || c.TTL != preloadTTL || c.Key.Type != "wendy.load.scalar.v1" || c.Provenance.Origin != world.Simulated || c.Frame != (world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}) {
			return errors.New("preload retained component mismatch")
		}
		remaining := entry.ExpiresAt.Sub(now)
		if remaining < time.Duration(o.minFresh)*time.Second {
			return errors.New("preload record freshness too short")
		}
		if minimum == 0 || remaining.Milliseconds() < minimum {
			minimum = remaining.Milliseconds()
		}
		seen[d] = true
	}
	return json.NewEncoder(output).Encode(map[string]any{"schema": "wendy.world.preload-verification.v1", "domain": "simulation", "run_id": o.runID, "node_index": o.nodeIndex, "count": o.count, "epoch": manifest.Epoch, "verified_at_unix_ms": now.UnixMilli(), "minimum_remaining_ms": minimum, "exact_signed_set": true})
}

func preloadGrant(g authority.Grant, run string) bool {
	return g.ID == "lab/"+run+"/sim-load-publisher" && g.Tenant == "lab" && g.Site == "synthetic-office" && g.Domain == "simulation" && g.Kind == "app" && g.Principal == "lab/sim-load-publisher" && g.PublishAudience == "node-2" && g.Source == (world.Source{Device: "sim-load-publisher", App: "wendy-world", Service: "sim-load-publisher", Stream: "state"}) && reflect.DeepEqual(g.Scopes, []string{"publish", "read"})
}

// Fixed public categories only. Filesystem, decoder and crypto error strings
// never enter the retained report.
func preloadVerificationClass(err error) string {
	switch err.Error() {
	case "invalid bounded preload verification scope", "preload manifest scope mismatch", "invalid preload digest set":
		return "scope_or_manifest"
	case "preload state unavailable":
		return "state_unavailable"
	case "preload state must be a regular file", "invalid bounded preload state":
		return "state_format"
	case "preload checkpoint checksum mismatch":
		return "checkpoint_checksum"
	case "preload checkpoint cardinality mismatch":
		return "checkpoint_cardinality"
	case "preload checkpoint clock regression":
		return "clock_regression"
	case "preload checkpoint invalid":
		return "checkpoint_invalid"
	case "preload proof cardinality mismatch":
		return "proof_cardinality"
	case "preload proof scope/lifetime mismatch":
		return "proof_scope_or_lifetime"
	case "preload origin signature invalid":
		return "origin_signature"
	case "preload version mismatch", "preload synthetic lineage or exact sequence mismatch", "preload retained component mismatch":
		return "record_mismatch"
	case "preload record freshness too short":
		return "record_freshness"
	case "preload revocation lifetime too short", "preload trust invalid":
		return "trust_or_revocation"
	default:
		return "verification_refused"
	}
}

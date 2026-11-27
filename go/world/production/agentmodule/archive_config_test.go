package agentmodule

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestCommissionedLimitsAndArchiveAreStrictAndImmutable(t *testing.T) {
	configured := FileConfig{Limits: appservice.Limits{MessagesPerSecond: 200, MessageBurst: 400}, ProofArchivePath: "/var/lib/wendy/world/proofs/archive.db"}
	raw, e := json.Marshal(configured)
	if e != nil {
		t.Fatal(e)
	}
	var decoded FileConfig
	if jws.DecodeStrict(raw, &decoded) != nil || decoded.Limits.MessagesPerSecond != 200 || decoded.Limits.MessageBurst != 400 || decoded.ProofArchivePath != configured.ProofArchivePath {
		t.Fatal("commissioned archive/budget not preserved")
	}
	if appservice.ValidateLimits(decoded.Limits) != nil || appservice.ValidateLimits(FileConfig{}.Limits) != nil {
		t.Fatal("bounded override or defaults refused")
	}
	for _, raw := range []string{`{"limits":{"MessagesPerSecond":"200"}}`, `{"limits":{"MessagesPerSecond":1e999}}`, `{"limits":{"MessageBurst":1.5}}`, `{"limits":{"Unknown":1}}`, `{"limits":{"MessagesPerSecond":200,"MessagesPerSecond":400}}`, `{"limits":NaN}`} {
		if jws.DecodeStrict([]byte(raw), &FileConfig{}) == nil {
			t.Fatal("malformed budget accepted", raw)
		}
	}
	dir := commissionedDirectory(t)
	path := filepath.Join(dir, "world.json")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	current := commissionedCurrent(path, raw, nil, [32]byte{})
	if e = current(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*FileConfig){func(f *FileConfig) { f.Limits.MessageBurst++ }, func(f *FileConfig) { f.ProofArchivePath = "/var/lib/wendy/world/other.db" }} {
		changed := configured
		mutate(&changed)
		b, _ := json.Marshal(changed)
		if e = os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
		if current(context.Background()) == nil {
			t.Fatal("live commissioned path/budget mutation accepted")
		}
	}
}

func TestModuleRejectsInvalidLimitsBeforeStoreInitialization(t *testing.T) {
	for _, limits := range []appservice.Limits{{MessagesPerSecond: math.NaN()}, {BytesPerSecond: math.Inf(1)}, {MessageBurst: 10001}, {MaxResponseBytes: 1 << 21}, {RequestTimeout: -1}, {MessagesPerSecond: -1}} {
		// Invalid limits must be rejected before dependency calls or creating a
		// journal/archive. This direct setup needs no network or private socket.
		dir := commissionedDirectory(t)
		journal := filepath.Join(dir, "journal", "world.db")
		cfg := Config{Policy: basePolicy(), JournalPath: journal, CreateJournal: true, Limits: limits}
		deps := Dependencies{Identity: unavailableIdentity{}, Clock: moduleTestClock(), SampleNow: func() time.Time { return moduleTestNow }, PolicyCurrent: func(context.Context) error { t.Fatal("invalid budget reached policy I/O"); return nil }, PeerCheck: func(context.Context, appservice.Peer, serviceorigin.Scope) error { return nil }}
		if m, e := New(context.Background(), cfg, deps); e == nil || m != nil {
			t.Fatal("invalid service budget accepted")
		}
		if _, e := os.Stat(filepath.Dir(journal)); !os.IsNotExist(e) {
			t.Fatal("invalid budget initialized storage", e)
		}
	}
}
